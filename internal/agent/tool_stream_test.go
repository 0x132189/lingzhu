package agent

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/permission"
	"lingzhu/internal/provider"
	"lingzhu/internal/tool"
)

type emittingTool struct{}

func (emittingTool) Name() string            { return "emit" }
func (emittingTool) Description() string     { return "emit output" }
func (emittingTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (emittingTool) Run(ctx context.Context, call tool.Call) (string, error) {
	if call.Emit != nil {
		_ = call.Emit(event.ToolOutputEvent(call.ID, "chunk"))
	}
	return "done", nil
}

func TestToolOnlyStepKeepsStreamTiming(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{
			{Usage: &provider.Usage{Input: 5, Output: 7}, ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "emit", Arguments: `{}`}}},
		},
		{{Usage: &provider.Usage{Input: 9, Output: 3}, Text: "finished"}},
	}}
	a, store := newTestAgent(t, fake)
	a.tools.Add(emittingTool{})

	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}

	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	var toolStep map[string]any
	for _, ev := range events {
		if ev.Type != event.AssistantMessage {
			continue
		}
		if text, _ := ev.Data["text"].(string); strings.TrimSpace(text) == "" {
			toolStep = ev.Data
		}
	}
	if toolStep == nil {
		t.Fatal("a tool-only step must still record its assistant message")
	}
	for _, key := range []string{"started", "firstToken", "streamed", "completed"} {
		if value, _ := toolStep[key].(string); value == "" {
			t.Fatalf("tool-only step is missing %s: %+v", key, toolStep)
		}
	}
	if output, _ := toolStep["outputTokens"].(int); output != 7 {
		t.Fatalf("tool-only step must carry its output tokens, got %+v", toolStep["outputTokens"])
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("unexpected messages: %+v", messages)
	}
	step := messages[1]
	if step.Role != model.RoleAssistant || len(step.ToolCalls) != 1 || step.OutputTokens != 7 || step.Started == "" || step.Streamed == "" {
		t.Fatalf("tool-only assistant step lost its metrics: %+v", step)
	}
}

func TestToolEventsKeepCallBeforeOutputBeforeResult(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "emit", Arguments: `{}`}}}},
		{{Text: "finished"}},
	}}
	a, store := newTestAgent(t, fake)
	a.tools.Add(emittingTool{})

	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	callAt, outputAt, resultAt, assistantAt := -1, -1, -1, -1
	for index, ev := range events {
		switch ev.Type {
		case event.ToolCall:
			callAt = index
		case event.ToolOutput:
			outputAt = index
		case event.ToolResult:
			resultAt = index
		case event.AssistantMessage:
			assistantAt = index
		}
	}
	if callAt < 0 || outputAt < 0 || resultAt < 0 {
		t.Fatalf("missing tool events: call=%d output=%d result=%d", callAt, outputAt, resultAt)
	}
	if !(callAt < outputAt && outputAt < resultAt) {
		t.Fatalf("tool events out of order: call=%d output=%d result=%d", callAt, outputAt, resultAt)
	}
	if assistantAt >= 0 && callAt > assistantAt {
		t.Fatalf("tool.call should be announced while the response streams: call=%d assistant=%d", callAt, assistantAt)
	}
}

type signalTool struct {
	started chan struct{}
	once    sync.Once
}

func (t *signalTool) Name() string            { return "signal" }
func (t *signalTool) Description() string     { return "signal that a tool started" }
func (t *signalTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *signalTool) Run(ctx context.Context, call tool.Call) (string, error) {
	t.once.Do(func() { close(t.started) })
	return "started", nil
}

type plainTool struct{ name string }

func (t plainTool) Name() string            { return t.name }
func (t plainTool) Description() string     { return "plain tool" }
func (t plainTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t plainTool) Run(ctx context.Context, call tool.Call) (string, error) {
	return "ok", nil
}

type delayTool struct {
	name  string
	delay time.Duration
}

func (t delayTool) Name() string            { return t.name }
func (t delayTool) Description() string     { return "delay" }
func (t delayTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t delayTool) Run(ctx context.Context, _ tool.Call) (string, error) {
	select {
	case <-time.After(t.delay):
		return t.name, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type channelStream struct {
	chunks chan provider.Chunk
}

func (s *channelStream) Recv() (provider.Chunk, error) {
	chunk, ok := <-s.chunks
	if !ok {
		return provider.Chunk{}, io.EOF
	}
	return chunk, nil
}

func (s *channelStream) Close() error { return nil }

// overlapProvider sends two tool calls. The second call proves the first one is
// complete; the provider then waits for the tool to start before finishing the
// stream, so a non-overlapping implementation would deadlock.
type overlapProvider struct {
	started chan struct{}
	calls   atomic.Int32
}

func (p *overlapProvider) Stream(ctx context.Context, req model.Request) (provider.Stream, error) {
	chunks := make(chan provider.Chunk)
	go func() {
		defer close(chunks)
		if p.calls.Add(1) > 1 {
			chunks <- provider.Chunk{Text: "finished"}
			return
		}
		chunks <- provider.Chunk{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "signal", Arguments: `{}`}}}
		chunks <- provider.Chunk{ToolCalls: []provider.ToolCallDelta{{Index: 1, ID: "call_2", Name: "echo", Arguments: `{}`}}}
		select {
		case <-p.started:
		case <-ctx.Done():
			return
		}
		chunks <- provider.Chunk{FinishReason: "tool_calls"}
	}()
	return &channelStream{chunks: chunks}, nil
}

// singleCallProvider sends one complete tool call and then waits for the tool
// to start before ending the stream, proving a single call does not wait for
// the finish reason.
type singleCallProvider struct {
	started chan struct{}
}

func (p *singleCallProvider) Stream(ctx context.Context, req model.Request) (provider.Stream, error) {
	chunks := make(chan provider.Chunk)
	go func() {
		defer close(chunks)
		chunks <- provider.Chunk{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "signal", Arguments: `{}`}}}
		select {
		case <-p.started:
		case <-ctx.Done():
			return
		}
		chunks <- provider.Chunk{FinishReason: "tool_calls"}
	}()
	return &channelStream{chunks: chunks}, nil
}

func TestSingleToolStartsWhenArgumentsComplete(t *testing.T) {
	started := make(chan struct{})
	registry := tool.NewRegistry()
	registry.Add(&signalTool{started: started})
	a := New(testStore(t), testCatalog(&singleCallProvider{started: started}, "test-model", 0), registry, nil, Options{MaxSteps: 2})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := a.Run(ctx, "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	select {
	case <-started:
	default:
		t.Fatal("single tool did not start from its completed arguments")
	}
}

func TestToolResultsPersistInCompletionOrder(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{
			{Index: 0, ID: "call_1", Name: "slow", Arguments: `{}`},
			{Index: 1, ID: "call_2", Name: "fast", Arguments: `{}`},
		}}},
		{{Text: "done"}},
	}}
	registry := tool.NewRegistry()
	registry.Add(delayTool{name: "slow", delay: 20 * time.Millisecond})
	registry.Add(delayTool{name: "fast"})
	a, store := newTestAgent(t, fake)
	a.tools = registry

	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, ev := range events {
		if ev.Type == event.ToolResult {
			results = append(results, ev.Data["name"].(string))
		}
	}
	if len(results) != 2 || results[0] != "fast" || results[1] != "slow" {
		t.Fatalf("want completion order fast, slow; got %v", results)
	}
}

func TestToolStartsBeforeStreamFinishes(t *testing.T) {
	started := make(chan struct{})
	p := &overlapProvider{started: started}
	registry := tool.NewRegistry()
	registry.Add(&signalTool{started: started})
	registry.Add(plainTool{name: "echo"})
	perm := permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})
	a := New(testStore(t), testCatalog(p, "test-model", 0), registry, perm, Options{MaxSteps: 4})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := a.Run(ctx, "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	select {
	case <-started:
	default:
		t.Fatal("tool did not start before the stream finished")
	}
	if p.calls.Load() < 2 {
		t.Fatal("follow-up step was not requested")
	}
}
