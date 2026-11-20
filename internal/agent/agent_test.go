package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"lingzhu/internal/agents"
	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/permission"
	"lingzhu/internal/prompt"
	"lingzhu/internal/provider"
	"lingzhu/internal/question"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
	"lingzhu/internal/warming"
)

type fakeProvider struct {
	requests []model.Request
	chunks   [][]provider.Chunk
	errs     []error
	// after fails a response once its chunks are exhausted.
	after []error
}

func (f *fakeProvider) Stream(_ context.Context, req model.Request) (provider.Stream, error) {
	f.requests = append(f.requests, req)
	index := len(f.requests) - 1
	if index < len(f.errs) && f.errs[index] != nil {
		return nil, f.errs[index]
	}
	var after error
	if index < len(f.after) {
		after = f.after[index]
	}
	if index >= len(f.chunks) {
		return &fakeStream{after: after}, nil
	}
	return &fakeStream{chunks: f.chunks[index], after: after}, nil
}

type fakeStream struct {
	chunks []provider.Chunk
	after  error
	i      int
}

func (s *fakeStream) Recv() (provider.Chunk, error) {
	if s.i < len(s.chunks) {
		chunk := s.chunks[s.i]
		s.i++
		return chunk, nil
	}
	if s.after != nil {
		err := s.after
		s.after = nil
		return provider.Chunk{}, err
	}
	return provider.Chunk{}, io.EOF
}

func (s *fakeStream) Close() error { return nil }

type echoTool struct{}

func (echoTool) Name() string            { return "echo" }
func (echoTool) Description() string     { return "echo text" }
func (echoTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (echoTool) Run(ctx context.Context, call tool.Call) (string, error) {
	if err := call.Allow(ctx, "echo", []string{"*"}, ""); err != nil {
		return "", err
	}
	return "echo:" + string(call.Input), nil
}

// barrierTool blocks until every concurrently started tool has announced
// itself. A sequential runner would deadlock on the second tool.
type lateTool struct{}

func (lateTool) Name() string            { return "late" }
func (lateTool) Description() string     { return "late tool" }
func (lateTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (lateTool) Run(context.Context, tool.Call) (string, error) { return "", nil }

type barrierTool struct {
	name    string
	started chan<- string
	release <-chan struct{}
}

func (b barrierTool) Name() string            { return b.name }
func (b barrierTool) Description() string     { return "barrier" }
func (b barrierTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (b barrierTool) Run(ctx context.Context, _ tool.Call) (string, error) {
	select {
	case b.started <- b.name:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case <-b.release:
		return b.name + " done", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func testStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newTestAgent(t *testing.T, fake *fakeProvider) (*Agent, *session.Store) {
	t.Helper()
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(echoTool{})
	catalog := testCatalog(fake, "test-model", 0)
	return New(store, catalog, registry, nil, Options{MaxSteps: 4}), store
}

// testCatalog wraps a fake provider so tests can select a model and window.
func testCatalog(p provider.Provider, model string, window int) *provider.Catalog {
	var models []provider.ModelSpec
	if model != "" {
		models = append(models, provider.ModelSpec{ID: model, Name: model, ContextWindow: window})
	}
	return provider.Static("test", "Test", p, models, model)
}

func TestStreamSendsVariantRequestModelAndOptions(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "ok"}}}}
	a, _ := newTestAgent(t, fake)
	target := provider.Target{
		Model:        "m::high",
		RequestModel: "m",
		Options:      map[string]any{"reasoning_effort": "high"},
		Provider:     fake,
	}
	if result := a.stream(context.Background(), "s1", []model.Message{{Role: model.RoleUser, Content: "hi"}}, target, nil, "", 0, nil); result.Err != nil {
		t.Fatal(result.Err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d", len(fake.requests))
	}
	req := fake.requests[0]
	if req.Model != "m" || req.Options["reasoning_effort"] != "high" {
		t.Fatalf("request = %+v", req)
	}
}

func TestStreamRecordsOrderedReasoningAndTextParts(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{{
		{Reasoning: "think "},
		{Reasoning: "again"},
		{Text: "answer"},
		{Reasoning: "later"},
	}}}
	a, _ := newTestAgent(t, fake)
	target := provider.Target{Model: "m", RequestModel: "m", Provider: fake}

	result := a.stream(context.Background(), "s1", []model.Message{{Role: model.RoleUser, Content: "hi"}}, target, nil, "", 0, nil)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	parts := result.Parts
	if len(parts) != 3 {
		t.Fatalf("parts = %+v", parts)
	}
	if parts[0].Type != "reasoning" || parts[0].Text != "think again" {
		t.Fatalf("first part = %+v", parts[0])
	}
	if parts[1].Type != "text" || parts[1].Text != "answer" {
		t.Fatalf("second part = %+v", parts[1])
	}
	if parts[2].Type != "reasoning" || parts[2].Text != "later" {
		t.Fatalf("third part = %+v", parts[2])
	}
	if parts[0].Created == "" || parts[0].Completed == "" || parts[0].Created > parts[0].Completed {
		t.Fatalf("part timestamps = %+v", parts[0])
	}
}

func TestStreamOmitsToolsWhenModelSaysNone(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "ok"}}}}
	a, _ := newTestAgent(t, fake)
	target := provider.Target{Model: "m", RequestModel: "m", NoTools: true, Provider: fake}
	tools := []model.ToolDef{{Name: "echo", Description: "d", Schema: []byte(`{"type":"object"}`)}}
	if result := a.stream(context.Background(), "s1", []model.Message{{Role: model.RoleUser, Content: "hi"}}, target, tools, "", 0, nil); result.Err != nil {
		t.Fatal(result.Err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d", len(fake.requests))
	}
	if len(fake.requests[0].Tools) != 0 {
		t.Fatalf("tools = %+v", fake.requests[0].Tools)
	}
}

func TestRunExecutesToolThenFinishes(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "echo", Arguments: `{"text":"hi"}`}}}},
		{{Text: "done"}},
	}}
	a, store := newTestAgent(t, fake)

	var events []event.Event
	stream, err := a.Run(context.Background(), "s1", "hello", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range stream {
		events = append(events, ev)
	}

	last := events[len(events)-1]
	if last.Type != event.RunEnd || last.Data["reason"] != "done" {
		t.Fatalf("unexpected last event: %+v", last)
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Role{model.RoleUser, model.RoleAssistant, model.RoleTool, model.RoleAssistant}
	if len(messages) != len(want) {
		t.Fatalf("want %d messages, got %d: %+v", len(want), len(messages), messages)
	}
	if messages[2].Content != `echo:{"text":"hi"}` {
		t.Fatalf("tool result mismatch: %q", messages[2].Content)
	}
	if messages[3].Content != "done" {
		t.Fatalf("final assistant message mismatch: %q", messages[3].Content)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("want 2 provider calls, got %d", len(fake.requests))
	}
	if len(fake.requests[0].Tools) != 1 {
		t.Fatalf("tools not passed to provider: %+v", fake.requests[0].Tools)
	}
}

func TestRunExecutesToolCallsConcurrently(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	closed := false
	defer func() {
		if !closed {
			close(release)
		}
	}()

	registry := tool.NewRegistry()
	registry.Add(barrierTool{name: "first", started: started, release: release})
	registry.Add(barrierTool{name: "second", started: started, release: release})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{
			{Index: 0, ID: "call_1", Name: "first", Arguments: `{}`},
			{Index: 1, ID: "call_2", Name: "second", Arguments: `{}`},
		}}},
		{{Text: "done"}},
	}}
	store := testStore(t)
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4})

	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	timeout := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-timeout:
			t.Fatalf("tools did not start concurrently: %v", seen)
		}
	}
	close(release)
	closed = true

	for range stream {
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, message := range messages {
		if message.Role == model.RoleTool {
			results = append(results, message.Content)
		}
	}
	if len(results) != 2 {
		t.Fatalf("want two tool results, got %+v", results)
	}
	if messages[len(messages)-1].Content != "done" {
		t.Fatalf("run should continue after concurrent tools, got %+v", messages)
	}
}

func TestRunStopsAtMaxSteps(t *testing.T) {
	call := provider.Chunk{ToolCalls: []provider.ToolCallDelta{
		{Index: 0, ID: "call_1", Name: "echo", Arguments: `{}`},
	}}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{call}, {call}, {call}, {call}, {call}}}
	a, _ := newTestAgent(t, fake)

	var last event.Event
	stream, err := a.Run(context.Background(), "s1", "loop", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range stream {
		last = ev
	}
	if last.Type != event.RunEnd || last.Data["reason"] != "max_steps" {
		t.Fatalf("want max_steps end, got %+v", last)
	}
}

func TestMaxStepsWrapUpDisablesTools(t *testing.T) {
	call := provider.Chunk{ToolCalls: []provider.ToolCallDelta{
		{Index: 0, ID: "call_1", Name: "echo", Arguments: `{}`},
	}}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{call}, {call}, {{Text: "summary"}}}}
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(echoTool{})
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 2})

	var last event.Event
	stream, err := a.Run(context.Background(), "s1", "loop", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range stream {
		last = ev
	}
	if last.Type != event.RunEnd || last.Data["reason"] != "max_steps" {
		t.Fatalf("want max_steps end, got %+v", last)
	}
	if len(fake.requests) != 3 {
		t.Fatalf("want two tool steps and one wrap-up, got %d calls", len(fake.requests))
	}
	final := fake.requests[len(fake.requests)-1]
	if len(final.Tools) == 0 {
		t.Fatalf("wrap-up keeps tool definitions for the cached prefix: %+v", final.Tools)
	}
	if final.ToolChoice != "none" {
		t.Fatalf("wrap-up must disable tools with tool_choice none: %q", final.ToolChoice)
	}
	lastMessage := final.Messages[len(final.Messages)-1]
	if lastMessage.Role != model.RoleAssistant || !strings.Contains(lastMessage.Content, "MAXIMUM STEPS") {
		t.Fatalf("wrap-up instruction missing: %+v", lastMessage)
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if messages[len(messages)-1].Content != "summary" {
		t.Fatalf("wrap-up answer was not recorded: %+v", messages)
	}
}

func TestRunRetriesRetryableFailureBeforeOutput(t *testing.T) {
	fake := &fakeProvider{
		errs:   []error{&provider.StatusError{Code: 429, Message: "rate limited"}, nil},
		chunks: [][]provider.Chunk{nil, {{Text: "recovered"}}},
	}
	a, store := newTestAgent(t, fake)
	a.retries = 2
	a.retryDelay = time.Millisecond

	var last event.Event
	sawRetry := false
	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range stream {
		last = ev
		if ev.Type == event.RunRetry {
			sawRetry = true
			if ev.Data["attempt"] != 1 || ev.Data["message"] == "" {
				t.Fatalf("unexpected retry event: %+v", ev)
			}
		}
	}
	if !sawRetry {
		t.Fatal("want a run.retry event before the second attempt")
	}
	if last.Type != event.RunEnd || last.Data["reason"] != "done" {
		t.Fatalf("want successful run after retry, got %+v", last)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("want one retry, got %d requests", len(fake.requests))
	}
	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if messages[len(messages)-1].Content != "recovered" {
		t.Fatalf("retry answer was not recorded: %+v", messages)
	}
}

func TestRunDoesNotRetryRejectedRequest(t *testing.T) {
	fake := &fakeProvider{
		errs: []error{&provider.StatusError{Code: 401, Message: "bad key"}},
	}
	a, _ := newTestAgent(t, fake)
	a.retries = 2
	a.retryDelay = time.Millisecond

	var last event.Event
	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range stream {
		last = ev
	}
	if last.Type != event.RunEnd || last.Data["reason"] != "error" {
		t.Fatalf("want error end, got %+v", last)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("non-retryable error was retried: %d requests", len(fake.requests))
	}
}

func TestOutputLimitContinuesWithoutRepeating(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{Text: "first half ", FinishReason: "length"}},
		{{Text: "second half"}},
	}}
	a, store := newTestAgent(t, fake)

	var last event.Event
	stream, err := a.Run(context.Background(), "s1", "write", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range stream {
		last = ev
	}
	if last.Type != event.RunEnd || last.Data["reason"] != "done" {
		t.Fatalf("want completed run, got %+v", last)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("want one output continuation, got %d requests", len(fake.requests))
	}
	second := fake.requests[1]
	notice := second.Messages[len(second.Messages)-1]
	if notice.Role != model.RoleUser || !strings.Contains(notice.Content, "output token limit") {
		t.Fatalf("continuation instruction missing: %+v", notice)
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("continuation notice should be stored as synthetic: %+v", messages)
	}
	if messages[1].Content != "first half " || messages[3].Content != "second half" {
		t.Fatalf("continuation text was not recorded: %+v", messages)
	}
	if !messages[2].Synthetic || !strings.Contains(messages[2].Content, "output token limit") {
		t.Fatalf("synthetic continuation notice missing: %+v", messages[2])
	}
}

func TestOutputLimitGivesUpAfterTwoContinuations(t *testing.T) {
	length := provider.Chunk{Text: "x", FinishReason: "length"}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{length}, {length}, {length}}}
	a, _ := newTestAgent(t, fake)

	var last event.Event
	stream, err := a.Run(context.Background(), "s1", "loop", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range stream {
		last = ev
	}
	if last.Type != event.RunEnd || last.Data["reason"] != "error" {
		t.Fatalf("want output-limit failure, got %+v", last)
	}
	if len(fake.requests) != 3 {
		t.Fatalf("want initial response plus two continuations, got %d", len(fake.requests))
	}
}

func TestInterruptedStreamContinuesFromPartialOutput(t *testing.T) {
	fake := &fakeProvider{
		chunks: [][]provider.Chunk{
			{{Text: "partial "}},
			{{Text: "rest"}},
		},
		after: []error{io.ErrUnexpectedEOF},
	}
	a, store := newTestAgent(t, fake)

	var last event.Event
	stream, err := a.Run(context.Background(), "s1", "write", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range stream {
		last = ev
	}
	if last.Type != event.RunEnd || last.Data["reason"] != "done" {
		t.Fatalf("want resumed run, got %+v", last)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("want one resume, got %d requests", len(fake.requests))
	}
	second := fake.requests[1]
	notice := second.Messages[len(second.Messages)-1]
	if notice.Role != model.RoleUser || !strings.Contains(notice.Content, "interrupted. Continue") {
		t.Fatalf("resume instruction missing: %+v", notice)
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 || messages[1].Content != "partial " || messages[3].Content != "rest" {
		t.Fatalf("partial output was not preserved: %+v", messages)
	}
	if !messages[2].Synthetic || !strings.Contains(messages[2].Content, "interrupted. Continue") {
		t.Fatalf("synthetic resume notice missing: %+v", messages[2])
	}
}

func TestSystemPromptIsRebuiltForEachRequest(t *testing.T) {
	call := provider.Chunk{ToolCalls: []provider.ToolCallDelta{
		{Index: 0, ID: "call_1", Name: "echo", Arguments: `{}`},
	}}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{call}, {{Text: "done"}}}}
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(echoTool{})
	built := 0
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{
		MaxSteps: 4,
		SystemFunc: func(string, provider.Target) string {
			built++
			return fmt.Sprintf("prompt-%d", built)
		},
	})

	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}

	if len(fake.requests) != 2 {
		t.Fatalf("want two model requests, got %d", len(fake.requests))
	}
	for index, request := range fake.requests {
		want := fmt.Sprintf("prompt-%d", index+1)
		if request.Messages[0].Role != model.RoleSystem || request.Messages[0].Content != want {
			t.Fatalf("request %d system prompt = %+v, want %q", index, request.Messages[0], want)
		}
	}
}

func TestInstructionStateFreezesPrefixAndSendsUpdates(t *testing.T) {
	call := provider.Chunk{ToolCalls: []provider.ToolCallDelta{
		{Index: 0, ID: "call_1", Name: "echo", Arguments: `{}`},
	}}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{call}, {{Text: "done"}}}}
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(echoTool{})

	date := "Mon Jan 2 2006"
	agents := "project rules v1"
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{
		MaxSteps: 4,
		SystemPartsFunc: func(string, provider.Target) prompt.Parts {
			return prompt.Parts{
				Base: "base prompt",
				Sections: []prompt.Section{
					{Key: "agents", Text: agents},
					{Key: "date", Text: "Today's date: " + date},
				},
			}
		},
	})

	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if len(fake.requests) != 2 {
		t.Fatalf("want two model requests, got %d", len(fake.requests))
	}
	first := fake.requests[0].Messages[0]
	if first.Role != model.RoleSystem || !strings.Contains(first.Content, "project rules v1") {
		t.Fatalf("baseline system prompt missing instructions: %+v", first)
	}

	// The next turn changes the project instructions and the date. The frozen
	// system prefix must stay byte-identical; the change arrives as a
	// synthetic message instead.
	agents = "project rules v2"
	date = "Tue Jan 3 2006"
	fake.chunks = append(fake.chunks, []provider.Chunk{{Text: "second"}})
	stream, err = a.Run(context.Background(), "s1", "continue", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if len(fake.requests) != 3 {
		t.Fatalf("want a third request, got %d", len(fake.requests))
	}
	second := fake.requests[2]
	if second.Messages[0].Content != first.Content {
		t.Fatalf("system prefix was rewritten:\nfirst:  %q\nsecond: %q", first.Content, second.Messages[0].Content)
	}
	var update string
	for _, message := range second.Messages {
		if message.Synthetic && strings.Contains(message.Content, "project rules v2") {
			update = message.Content
		}
	}
	if update == "" {
		t.Fatalf("instruction update message missing: %+v", second.Messages)
	}
	if !strings.Contains(update, "Tue Jan 3 2006") || strings.Contains(update, "project rules v1") {
		t.Fatalf("update should carry the new sections only: %q", update)
	}

	// The baseline is durable, so a fresh agent on the same store keeps it.
	reloaded := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 2})
	snapshot := reloaded.loadInstructionState("s1")
	if snapshot == nil {
		t.Fatal("instruction state was not persisted")
	}
	if !strings.Contains(snapshot.initial["agents"], "project rules v1") {
		t.Fatalf("baseline should stay frozen: %+v", snapshot.initial)
	}
	if !strings.Contains(snapshot.current["agents"], "project rules v2") {
		t.Fatalf("current values should track the latest sections: %+v", snapshot.current)
	}
}

func TestInstructionUpdateAddsLateCodemodeTool(t *testing.T) {
	call := provider.Chunk{ToolCalls: []provider.ToolCallDelta{
		{Index: 0, ID: "call_1", Name: "echo", Arguments: `{}`},
	}}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{call}, {{Text: "done"}}}}
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(echoTool{})

	added := false
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{
		MaxSteps: 4,
		SystemPartsFunc: func(string, provider.Target) prompt.Parts {
			// The first request starts with no Code Mode tools. The next request
			// sees a tool that arrived while the turn was running.
			if len(fake.requests) > 0 && !added {
				registry.Add(tool.Codemode(lateTool{}))
				added = true
			}
			return prompt.Parts{
				Base: "base prompt",
				Sections: []prompt.Section{
					{Key: "codemode", Text: registry.Catalog()},
				},
			}
		},
	})

	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if len(fake.requests) != 2 {
		t.Fatalf("want two model requests, got %d", len(fake.requests))
	}
	var update string
	for _, message := range fake.requests[1].Messages {
		if message.Synthetic && strings.Contains(message.Content, "tools.late") {
			update = message.Content
		}
	}
	if update == "" {
		t.Fatalf("late Code Mode tool did not produce an instruction update: %+v", fake.requests[1].Messages)
	}
}

func TestRenderInstructionUpdateReportsChangedSections(t *testing.T) {
	text, sources := renderInstructionUpdate(
		map[string]string{"agents": "rules v1", "date": "Mon", "environment": "old"},
		map[string]string{"agents": "rules v2", "date": "Tue"},
	)
	for _, want := range []string{"rules v2", "Tue", "Environment"} {
		if !strings.Contains(text, want) {
			t.Fatalf("update missing %q: %s", want, text)
		}
	}
	if strings.Join(sources, ",") != "agents,date,environment" {
		t.Fatalf("changed sections must keep prompt order, got %v", sources)
	}
}

func TestEnqueueDeliversInputAtNextBoundary(t *testing.T) {
	started := make(chan string, 1)
	release := make(chan struct{})
	closed := false
	defer func() {
		if !closed {
			close(release)
		}
	}()

	registry := tool.NewRegistry()
	registry.Add(barrierTool{name: "hold", started: started, release: release})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{
			{Index: 0, ID: "call_1", Name: "hold", Arguments: `{}`},
		}}},
		{{Text: "done"}},
	}}
	store := testStore(t)
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 1})

	stream, err := a.Run(context.Background(), "s1", "start", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	if _, err := a.Enqueue("s1", "steer now"); err != nil {
		t.Fatal(err)
	}
	close(release)
	closed = true
	for range stream {
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Role{model.RoleUser, model.RoleAssistant, model.RoleTool, model.RoleUser, model.RoleAssistant}
	if len(messages) != len(want) {
		t.Fatalf("want %d messages, got %+v", len(want), messages)
	}
	for index, role := range want {
		if messages[index].Role != role {
			t.Fatalf("message %d role = %s, want %s", index, messages[index].Role, role)
		}
	}
	if messages[3].Content != "steer now" {
		t.Fatalf("steering input missing: %+v", messages)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("want two model calls, got %d", len(fake.requests))
	}
	if len(fake.requests[1].Tools) == 0 {
		t.Fatal("queued input must reset the step budget")
	}
}

// Admission wakes an idle session: nothing else would start the turn that now
// owns the input.
func TestEnqueueWakesIdleSession(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "answer"}}}}
	a, store := newTestAgent(t, fake)
	if _, err := a.Enqueue("s1", "late"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		messages, err := store.Messages("s1")
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) == 2 && messages[0].Content == "late" && messages[1].Content == "answer" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("enqueue did not wake the session: %+v", messages)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A queued prompt must never join a running turn, even when a steer arrives
// before it: the caller asked for a separate turn.
func TestQueuePromptWaitsWhenSteerArrivesFirst(t *testing.T) {
	started := make(chan string, 1)
	release := make(chan struct{})
	closed := false
	defer func() {
		if !closed {
			close(release)
		}
	}()
	registry := tool.NewRegistry()
	registry.Add(barrierTool{name: "hold", started: started, release: release})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "hold", Arguments: `{}`}}}},
		{{Text: "steered"}},
		{{Text: "queued turn"}},
	}}
	store := testStore(t)
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4})

	stream, err := a.Run(context.Background(), "s1", "start", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	if _, err := a.Enqueue("s1", "join now"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.EnqueueDelivery("s1", "own turn", "queue"); err != nil {
		t.Fatal(err)
	}
	close(release)
	closed = true
	for range stream {
	}

	deadline := time.Now().Add(2 * time.Second)
	var messages []model.Message
	for {
		messages, err = store.Messages("s1")
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) >= 6 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The steer joins this turn; the queued prompt becomes a later one.
	var contents []string
	for _, message := range messages {
		if message.Role == model.RoleUser {
			contents = append(contents, message.Content)
		}
	}
	if len(contents) != 3 || contents[1] != "join now" || contents[2] != "own turn" {
		t.Fatalf("promotion order wrong: %+v", contents)
	}
}

// A manual compaction is a control item: prompts queued behind it must not be
// delivered before it, so the checkpoint cannot fold them into its summary.
func TestCompactionControlKeepsLaterPromptsParked(t *testing.T) {
	started := make(chan string, 1)
	release := make(chan struct{})
	closed := false
	defer func() {
		if !closed {
			close(release)
		}
	}()
	registry := tool.NewRegistry()
	registry.Add(barrierTool{name: "hold", started: started, release: release})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "hold", Arguments: `{}`}}}},
		{{Text: "after compact"}},
	}}
	store := testStore(t)
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4})

	stream, err := a.Run(context.Background(), "s1", "start", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	if err := a.Compact(context.Background(), "s1", provider.Selection{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Enqueue("s1", "behind the control"); err != nil {
		t.Fatal(err)
	}
	close(release)
	closed = true
	for range stream {
	}

	// The prompt must still be delivered, and the compaction event must come
	// before it in the transcript.
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	compactionAt, promptAt := -1, -1
	for index, ev := range events {
		switch ev.Type {
		case event.Compaction:
			if compactionAt < 0 {
				compactionAt = index
			}
		case event.UserMessage:
			if text, _ := ev.Data["text"].(string); text == "behind the control" {
				promptAt = index
			}
		}
	}
	if compactionAt < 0 || promptAt < 0 {
		t.Fatalf("missing events: compaction=%d prompt=%d", compactionAt, promptAt)
	}
	if promptAt < compactionAt {
		t.Fatalf("prompt was delivered before the control: prompt=%d compaction=%d", promptAt, compactionAt)
	}
}

// A manual compaction admitted after earlier steers still owns the boundary:
// the checkpoint must run first so the steer text stays verbatim after it
// instead of being folded into the summary.
func TestCompactionAfterSteersRunsFirst(t *testing.T) {
	started := make(chan string, 1)
	release := make(chan struct{})
	closed := false
	defer func() {
		if !closed {
			close(release)
		}
	}()
	registry := tool.NewRegistry()
	registry.Add(barrierTool{name: "hold", started: started, release: release})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "hold", Arguments: `{}`}}}},
		{{Text: "checkpoint done"}},
		{{Text: "steers done"}},
	}}
	store := testStore(t)
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4})

	stream, err := a.Run(context.Background(), "s1", "start", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	if _, err := a.Enqueue("s1", "STEER_A"); err != nil {
		t.Fatal(err)
	}
	if err := a.Compact(context.Background(), "s1", provider.Selection{}); err != nil {
		t.Fatal(err)
	}
	close(release)
	closed = true
	for range stream {
	}

	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	compactionAt, steerAt := -1, -1
	for index, ev := range events {
		switch ev.Type {
		case event.Compaction:
			if compactionAt < 0 {
				compactionAt = index
			}
		case event.UserMessage:
			if text, _ := ev.Data["text"].(string); text == "STEER_A" {
				steerAt = index
			}
		}
	}
	if compactionAt < 0 || steerAt < 0 {
		t.Fatalf("missing events: compaction=%d steer=%d", compactionAt, steerAt)
	}
	if steerAt < compactionAt {
		t.Fatalf("steer was delivered before the control: steer=%d compaction=%d", steerAt, compactionAt)
	}
	if len(fake.requests) < 2 {
		t.Fatalf("want at least two model calls, got %d", len(fake.requests))
	}
	for _, message := range fake.requests[1].Messages {
		if message.Role == model.RoleUser && strings.Contains(message.Content, "STEER_A") {
			t.Fatalf("compaction folded the steer into its summary: %+v", fake.requests[1].Messages)
		}
	}
}

func TestQueueDeliveryStartsTurnAfterFinish(t *testing.T) {
	started := make(chan string, 1)
	release := make(chan struct{})
	registry := tool.NewRegistry()
	registry.Add(barrierTool{name: "hold", started: started, release: release})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "hold", Arguments: `{}`}}}},
		{{Text: "first done"}},
		{{Text: "queued done"}},
	}}
	store := testStore(t)
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4})

	stream, err := a.Run(context.Background(), "s1", "start", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	if _, err := a.EnqueueDelivery("s1", "later", "queue"); err != nil {
		t.Fatal(err)
	}
	close(release)
	for range stream {
	}

	deadline := time.Now().Add(2 * time.Second)
	var messages []model.Message
	for {
		messages, err = store.Messages("s1")
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) >= 6 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := []model.Role{model.RoleUser, model.RoleAssistant, model.RoleTool, model.RoleAssistant, model.RoleUser, model.RoleAssistant}
	if len(messages) != len(want) {
		t.Fatalf("want %d messages, got %+v", len(want), messages)
	}
	if messages[3].Content != "first done" || messages[4].Content != "later" || messages[5].Content != "queued done" {
		t.Fatalf("queued turn did not start after finish: %+v", messages)
	}
	recovery, err := store.Recovery("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Inputs) != 0 || recovery.ClaimID != "" {
		t.Fatalf("queued input should be delivered exactly once: %+v", recovery)
	}
	if len(fake.requests) != 3 {
		t.Fatalf("want three model calls, got %d", len(fake.requests))
	}
}

func TestEnqueueDeliveryRejectsUnknownMode(t *testing.T) {
	a, _ := newTestAgent(t, &fakeProvider{})
	if _, err := a.EnqueueDelivery("s1", "x", "bogus"); err == nil {
		t.Fatal("want an error for an unknown delivery mode")
	}
}

// Stopping must not strand steering input: once the interrupted turn settles,
// the work the user asked to join it continues in a fresh turn and is delivered
// exactly once.
func TestInterruptResumesSteeringInput(t *testing.T) {
	started := make(chan string, 1)
	release := make(chan struct{})
	defer close(release)
	registry := tool.NewRegistry()
	registry.Add(barrierTool{name: "hold", started: started, release: release})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "hold", Arguments: `{}`}}}},
		{{Text: "steered after interrupt"}},
	}}
	store := testStore(t)
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4, Snapshots: fixedSnapshotter{id: "commit123"}})

	stream, err := a.Run(context.Background(), "s1", "start", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	if _, err := a.Enqueue("s1", "STEER_A"); err != nil {
		t.Fatal(err)
	}
	recoveries, err := store.Recoveries()
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveries) != 1 || recoveries[0].ClaimID == "" || len(recoveries[0].Inputs) != 1 {
		t.Fatalf("claim or queued input was not persisted: %+v", recoveries)
	}

	if !a.Interrupt("s1", true) {
		t.Fatal("interrupt should report a running turn")
	}
	for range stream {
	}

	deadline := time.Now().Add(2 * time.Second)
	var messages []model.Message
	for {
		messages, err = store.Messages("s1")
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) > 0 && messages[len(messages)-1].Role == model.RoleAssistant && messages[len(messages)-1].Content == "steered after interrupt" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("interrupt did not resume the steer: %+v", messages)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var users []string
	for _, message := range messages {
		if message.Role == model.RoleUser {
			users = append(users, message.Content)
		}
	}
	if len(users) != 2 || users[0] != "start" || users[1] != "STEER_A" {
		t.Fatalf("steer should be delivered once, after the interrupted prompt: %+v", users)
	}
	recovery, err := store.Recovery("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Inputs) != 0 || recovery.ClaimID != "" {
		t.Fatalf("steer should be delivered exactly once: %+v", recovery)
	}
	// The successor turn still records its own revert point.
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	steerSeq := int64(0)
	snapshotTurn := int64(0)
	for _, ev := range events {
		switch ev.Type {
		case event.UserMessage:
			if ev.Data["text"] == "STEER_A" {
				steerSeq = ev.Seq
			}
		case event.TurnSnapshot:
			switch seq := ev.Data["turnSeq"].(type) {
			case int64:
				snapshotTurn = seq
			case float64:
				snapshotTurn = int64(seq)
			}
		}
	}
	if steerSeq == 0 || snapshotTurn != steerSeq {
		t.Fatalf("resumed turn should have its own snapshot: steer=%d snapshot=%d", steerSeq, snapshotTurn)
	}
}

// Steering input left by an earlier turn is delivered ahead of a fresh prompt,
// matching the idle boundary where steers take priority.
func TestRunDeliversParkedSteerFirst(t *testing.T) {
	store := testStore(t)
	if _, err := store.QueueInputDelivery("s1", "inp_1", "STRANDED", "steer"); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "answer"}}}}
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{MaxSteps: 4})

	stream, err := a.Run(context.Background(), "s1", "fresh prompt", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, message := range messages {
		if message.Role == model.RoleUser {
			users = append(users, message.Content)
		}
	}
	if len(users) != 2 || users[0] != "STRANDED" || users[1] != "fresh prompt" {
		t.Fatalf("parked steer should lead the new prompt: %+v", users)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("want one model call, got %d", len(fake.requests))
	}
	recovery, err := store.Recovery("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Inputs) != 0 || recovery.ClaimID != "" {
		t.Fatalf("adopted steer should be delivered exactly once: %+v", recovery)
	}
}

// An interrupt keeps queued prompts parked: they are not deleted, but nothing
// starts them until a later wake.
func TestInterruptKeepsQueuedPromptParked(t *testing.T) {
	started := make(chan string, 1)
	release := make(chan struct{})
	defer close(release)
	registry := tool.NewRegistry()
	registry.Add(barrierTool{name: "hold", started: started, release: release})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "hold", Arguments: `{}`}}}},
	}}
	store := testStore(t)
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4})

	stream, err := a.Run(context.Background(), "s1", "start", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	if _, err := a.EnqueueDelivery("s1", "later", "queue"); err != nil {
		t.Fatal(err)
	}
	a.Interrupt("s1", false)
	for range stream {
	}
	recovery, err := store.Recovery("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Inputs) != 1 || recovery.Inputs[0].Text != "later" || recovery.Inputs[0].Delivery != "queue" {
		t.Fatalf("queued prompt should survive an interrupt: %+v", recovery)
	}
	if recovery.ClaimID != "" {
		t.Fatalf("interrupt should release the claim: %+v", recovery)
	}
}

func TestResumeDeliversQueuedInputAndReleasesClaim(t *testing.T) {
	store := testStore(t)
	if _, err := store.Append("s1", event.TurnStartedEvent("turn_1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueueInput("s1", "inp_1", "queued while away"); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "resumed"}}}}
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{MaxSteps: 4})

	events, err := a.Resume(context.Background(), "s1", "turn_1", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Content != "queued while away" || messages[1].Content != "resumed" {
		t.Fatalf("unexpected resumed history: %+v", messages)
	}
	recoveries, err := store.Recoveries()
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveries) != 0 {
		t.Fatalf("resume should release the claim: %+v", recoveries)
	}
}

func TestResumeSettlesInterruptedToolCall(t *testing.T) {
	store := testStore(t)
	if _, err := store.Append("s1", event.TurnStartedEvent("turn_1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.ToolCallEvent("call_1", "echo", `{}`)); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "after"}}}}
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{MaxSteps: 4})

	events, err := a.Resume(context.Background(), "s1", "turn_1", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	if len(fake.requests) != 1 {
		t.Fatalf("want one resumed request, got %d", len(fake.requests))
	}
	var found bool
	for _, message := range fake.requests[0].Messages {
		if message.Role == model.RoleTool && message.ToolCallID == "call_1" && strings.Contains(message.Content, "interrupted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("interrupted tool call was not settled: %+v", fake.requests[0].Messages)
	}
}

func TestShutdownKeepsClaimForRecovery(t *testing.T) {
	store := testStore(t)
	base, stop := context.WithCancel(context.Background())
	fake := &waitProvider{started: make(chan struct{}, 1)}
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{MaxSteps: 2, Base: base})

	events, err := a.Run(context.Background(), "s1", "hi", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.started:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not reach the provider")
	}
	stop()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	recoveries, err := store.Recoveries()
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveries) != 1 || recoveries[0].ClaimID == "" {
		t.Fatalf("shutdown should keep the claim: %+v", recoveries)
	}
}

func TestRunRequiresModel(t *testing.T) {
	store := testStore(t)
	a := New(store, testCatalog(&fakeProvider{}, "", 0), tool.NewRegistry(), nil, Options{MaxSteps: 4})
	if _, err := a.Run(context.Background(), "s1", "x", provider.Selection{}); err == nil {
		t.Fatal("want error when model is empty")
	}
}

func TestPermissionRejectInterruptsRun(t *testing.T) {
	rules := permission.Rules{{Action: "echo", Resource: "*", Effect: permission.Ask}}
	perm := permission.New(rules)
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(echoTool{})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "echo", Arguments: `{"text":"hi"}`}}}},
		{{Text: "understood"}},
	}}
	a := New(store, testCatalog(fake, "test-model", 0), registry, perm, Options{MaxSteps: 4})

	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	reason := ""
	for ev := range stream {
		if ev.Type == event.RunEnd {
			reason, _ = ev.Data["reason"].(string)
		}
		if ev.Type != event.PermissionAsked {
			continue
		}
		asked++
		id, _ := ev.Data["id"].(string)
		if err := a.Reply(id, permission.Reject, ""); err != nil {
			t.Fatal(err)
		}
	}
	if asked != 1 {
		t.Fatalf("want 1 permission request, got %d", asked)
	}
	if reason != "interrupted" {
		t.Fatalf("a bare rejection should interrupt the turn, got %q", reason)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("a rejection must not request a follow-up step, got %d requests", len(fake.requests))
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	var toolResult string
	for _, m := range messages {
		if m.Role == model.RoleTool {
			toolResult = m.Content
		}
	}
	if !strings.Contains(toolResult, "permission declined by user") {
		t.Fatalf("model should see the decline, got %q", toolResult)
	}
	for _, m := range messages {
		if m.Content == "understood" {
			t.Fatal("the model must not continue after a bare user rejection")
		}
	}
}

// A rejection that carries instructions must not end the turn: the model reads
// the feedback as the tool result and continues, so one message redirects the
// work instead of stopping it.
func TestPermissionRejectWithFeedbackContinuesTurn(t *testing.T) {
	rules := permission.Rules{{Action: "echo", Resource: "*", Effect: permission.Ask}}
	perm := permission.New(rules)
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(echoTool{})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "echo", Arguments: `{"text":"hi"}`}}}},
		{{Text: "understood"}},
	}}
	a := New(store, testCatalog(fake, "test-model", 0), registry, perm, Options{MaxSteps: 4})

	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	reason := ""
	for ev := range stream {
		if ev.Type == event.RunEnd {
			reason, _ = ev.Data["reason"].(string)
		}
		if ev.Type != event.PermissionAsked {
			continue
		}
		asked++
		id, _ := ev.Data["id"].(string)
		if err := a.Reply(id, permission.Reject, "use read instead"); err != nil {
			t.Fatal(err)
		}
	}
	if asked != 1 {
		t.Fatalf("want 1 permission request, got %d", asked)
	}
	if reason != "done" {
		t.Fatalf("feedback should keep the turn running, got %q", reason)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("feedback should request a follow-up step, got %d requests", len(fake.requests))
	}
	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	var toolResult string
	for _, m := range messages {
		if m.Role == model.RoleTool {
			toolResult = m.Content
		}
	}
	if !strings.Contains(toolResult, "use read instead") {
		t.Fatalf("model should see the feedback, got %q", toolResult)
	}
}

func TestUnattendedAskIsDeniedInsteadOfBlocking(t *testing.T) {
	// A scheduled wake-up has no client to answer a permission prompt, so an ask
	// must settle as a denial instead of stalling the turn.
	rules := permission.Rules{
		{Action: "*", Resource: "*", Effect: permission.Allow},
		{Action: "echo", Resource: "*", Effect: permission.Ask},
	}
	perm := permission.New(rules)
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(echoTool{})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "echo", Arguments: `{"text":"hi"}`}}}},
		{{Text: "done"}},
	}}
	a := New(store, testCatalog(fake, "test-model", 0), registry, perm, Options{MaxSteps: 4})

	stream, err := a.RunUnattended(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range stream {
			if ev.Type == event.PermissionAsked {
				asked++
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an unattended turn must not block on a permission ask")
	}
	if asked != 0 {
		t.Fatalf("an unattended ask must not reach a client, got %d", asked)
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	var toolResult string
	for _, m := range messages {
		if m.Role == model.RoleTool {
			toolResult = m.Content
		}
	}
	if !strings.Contains(toolResult, "nobody is present") {
		t.Fatalf("model should see why the tool was denied, got %q", toolResult)
	}
	if len(perm.Pending("")) != 0 {
		t.Fatal("an unattended ask must not stay pending")
	}
}

func TestScheduledNotifyRunsUnattended(t *testing.T) {
	rules := permission.Rules{
		{Action: "*", Resource: "*", Effect: permission.Allow},
		{Action: "echo", Resource: "*", Effect: permission.Ask},
	}
	perm := permission.New(rules)
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(echoTool{})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "echo", Arguments: `{"text":"hi"}`}}}},
		{{Text: "done"}},
	}}
	a := New(store, testCatalog(fake, "test-model", 0), registry, perm, Options{MaxSteps: 4})

	if err := a.Notify("s1", event.ScheduleFiredEvent("sch_1", "tick", "scheduled work")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Wait(ctx); err != nil {
		t.Fatalf("a scheduled turn must finish without a client: %v", err)
	}
	if len(perm.Pending("")) != 0 {
		t.Fatal("a scheduled turn must not leave a pending ask")
	}
	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	var toolResult string
	for _, m := range messages {
		if m.Role == model.RoleTool {
			toolResult = m.Content
		}
	}
	if !strings.Contains(toolResult, "nobody is present") {
		t.Fatalf("scheduled work should be denied unattended asks, got %q", toolResult)
	}
}

type blockTool struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockTool) Name() string            { return "block" }
func (b *blockTool) Description() string     { return "block" }
func (b *blockTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (b *blockTool) Run(ctx context.Context, _ tool.Call) (string, error) {
	close(b.started)
	select {
	case <-b.release:
		return "blocked done", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestNotifyStartsTurnWithoutUserMessage(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "notified"}}}}
	a, store := newTestAgent(t, fake)

	notification := event.JobCompletedEvent("job_1", "echo", "completed", "out.log", "[job] done", 0)
	if err := a.Notify("s1", notification); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("want 2 messages, got %+v", messages)
	}
	if messages[0].Role != model.RoleUser || messages[0].Content != "[job] done" {
		t.Fatalf("notification not visible to the model: %+v", messages[0])
	}
	if messages[1].Role != model.RoleAssistant || messages[1].Content != "notified" {
		t.Fatalf("unexpected assistant message: %+v", messages[1])
	}
}

func TestNotifyContinuesRunningTurn(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "block", Arguments: `{}`}}}},
		{{Text: "after notice"}},
	}}
	store := testStore(t)
	registry := tool.NewRegistry()
	block := &blockTool{started: make(chan struct{}), release: make(chan struct{})}
	registry.Add(block)
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4})

	stream, err := a.Run(context.Background(), "s1", "hello", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-block.started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not start")
	}

	notification := event.JobCompletedEvent("job_1", "echo", "completed", "out.log", "[job] done", 0)
	if err := a.Notify("s1", notification); err != nil {
		t.Fatal(err)
	}
	close(block.release)
	for range stream {
	}

	if len(fake.requests) < 2 {
		t.Fatalf("expected a follow-up model call, got %d", len(fake.requests))
	}
	found := false
	for _, message := range fake.requests[1].Messages {
		if message.Content == "[job] done" {
			found = true
		}
	}
	if !found {
		t.Fatalf("follow-up request missing notification: %+v", fake.requests[1].Messages)
	}
	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if last := messages[len(messages)-1]; last.Role != model.RoleAssistant || last.Content != "after notice" {
		t.Fatalf("unexpected final message: %+v", last)
	}
}

type editTool struct{}

func (editTool) Name() string            { return "edit" }
func (editTool) Description() string     { return "edit text" }
func (editTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (editTool) Run(ctx context.Context, call tool.Call) (string, error) {
	if err := call.Allow(ctx, "edit", []string{"a.txt"}, ""); err != nil {
		return "", err
	}
	return "written", nil
}

func TestAgentProfileAppliesSystemAndPermissions(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "edit", Arguments: `{}`}}}},
		{{Text: "understood"}},
	}}
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(editTool{})
	perm := permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})
	profiles := agents.New(agents.Config{}, "")
	if err := store.SetAgent("s1", "plan"); err != nil {
		t.Fatal(err)
	}
	a := New(store, testCatalog(fake, "test-model", 0), registry, perm, Options{MaxSteps: 4, Agents: profiles})

	stream, err := a.Run(context.Background(), "s1", "edit it", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	var toolResult string
	for _, message := range messages {
		if message.Role == model.RoleTool {
			toolResult = message.Content
		}
	}
	if !strings.Contains(toolResult, "permission denied") {
		t.Fatalf("plan agent should deny edit, got %q", toolResult)
	}
	if len(fake.requests) == 0 || len(fake.requests[0].Messages) == 0 {
		t.Fatal("no request captured")
	}
	for _, message := range fake.requests[0].Messages {
		if message.Role == model.RoleSystem && strings.Contains(message.Content, "Plan mode") {
			t.Fatalf("plan mode must not be in the system prompt: %+v", message)
		}
	}
	reminder := fake.requests[0].Messages[len(fake.requests[0].Messages)-2]
	if reminder.Role != model.RoleUser || !strings.Contains(reminder.Content, "Plan mode") {
		t.Fatalf("plan reminder missing before the prompt: %+v", fake.requests[0].Messages)
	}
	if last := fake.requests[0].Messages[len(fake.requests[0].Messages)-1]; last.Role != model.RoleUser || last.Content != "edit it" {
		t.Fatalf("plan reminder must precede the prompt: %+v", fake.requests[0].Messages)
	}
}

type fixedSnapshotter struct{ id string }

func (f fixedSnapshotter) Capture(context.Context) (string, error) { return f.id, nil }

func TestRunRecordsTurnSnapshot(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "ok"}}}}
	store := testStore(t)
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{
		MaxSteps:  4,
		Snapshots: fixedSnapshotter{id: "commit123"},
	})
	stream, err := a.Run(context.Background(), "s1", "hello", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	var userSeq int64
	for _, ev := range events {
		if ev.Type == event.UserMessage {
			userSeq = ev.Seq
		}
	}
	for _, ev := range events {
		if ev.Type != event.TurnSnapshot {
			continue
		}
		if ev.Data["snapshot"] != "commit123" {
			t.Fatalf("unexpected snapshot data: %+v", ev.Data)
		}
		if ev.Data["turnSeq"] != float64(userSeq) && ev.Data["turnSeq"] != userSeq {
			t.Fatalf("snapshot not keyed to the user turn: %+v", ev.Data)
		}
		return
	}
	t.Fatal("turn snapshot event missing")
}

type fakeHooks struct {
	created bool
	idle    bool
	after   []string
	rewrite bool
}

func (f *fakeHooks) BeforeTool(_ context.Context, _, tool, _ string, arguments json.RawMessage) (json.RawMessage, string, error) {
	if tool == "echo" {
		return arguments, "blocked by test", nil
	}
	return arguments, "", nil
}

func (f *fakeHooks) AfterTool(_ context.Context, _, tool, _ string, _ string, _ bool) {
	f.after = append(f.after, tool)
}

func (f *fakeHooks) ApplyContext(_ context.Context, event ContextEvent) (ContextEvent, error) {
	if !f.rewrite {
		return event, nil
	}
	for i := range event.Messages {
		if event.Messages[i].Content == "context in" {
			event.Messages[i].Content = "context out"
		}
	}
	if event.Options == nil {
		event.Options = map[string]any{}
	}
	event.Options["temperature"] = 0.2
	return event, nil
}

func (f *fakeHooks) SessionEvent(_ context.Context, event, _ string) {
	if event == "session.created" {
		f.created = true
	}
	if event == "session.idle" {
		f.idle = true
	}
}

func TestPluginHooksBlockToolsAndReceiveLifecycle(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "echo", Arguments: `{"text":"hi"}`}}}},
		{{Text: "done"}},
	}}
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(echoTool{})
	hooks := &fakeHooks{}
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4, Hooks: hooks})
	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	var result string
	for _, message := range messages {
		if message.Role == model.RoleTool {
			result = message.Content
		}
	}
	if !strings.Contains(result, "blocked by plugin") {
		t.Fatalf("tool should be blocked, got %q", result)
	}
	if !hooks.created || !hooks.idle {
		t.Fatalf("session lifecycle missing: %+v", hooks)
	}
	if len(hooks.after) != 1 || hooks.after[0] != "echo" {
		t.Fatalf("after hook missing: %+v", hooks.after)
	}
}

func TestAgentProfileFiltersAndBlocksDisabledTools(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_echo", Name: "echo", Arguments: `{"text":"hi"}`}}}},
		{{Text: "done"}},
	}}
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(echoTool{})
	registry.Add(editTool{})
	profiles := agents.New(agents.Config{Agents: map[string]agents.Definition{
		"limited": {
			Description: "Only edit is available",
			Mode:        agents.ModePrimary,
			Permissions: permission.Rules{{Action: "echo", Resource: "*", Effect: permission.Deny}},
		},
	}}, "limited")
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4, Agents: profiles})

	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if len(fake.requests) == 0 {
		t.Fatal("no request captured")
	}
	names := map[string]bool{}
	for _, def := range fake.requests[0].Tools {
		names[def.Name] = true
	}
	if names["echo"] {
		t.Fatalf("disabled tool was advertised: %v", names)
	}
	if !names["edit"] {
		t.Fatalf("allowed tool missing: %v", names)
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	var result string
	for _, message := range messages {
		if message.Role == model.RoleTool {
			result = message.Content
		}
	}
	if !strings.Contains(result, "disabled for this agent") {
		t.Fatalf("disabled tool should be blocked, got %q", result)
	}
}

func TestPluginContextHookRewritesRequest(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "done"}}}}
	store := testStore(t)
	hooks := &fakeHooks{rewrite: true}
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{MaxSteps: 2, Hooks: hooks, System: "base system"})

	stream, err := a.Run(context.Background(), "s1", "context in", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if len(fake.requests) == 0 {
		t.Fatal("no request captured")
	}
	request := fake.requests[0]
	last := request.Messages[len(request.Messages)-1]
	if last.Content != "context out" {
		t.Fatalf("context hook did not rewrite the user message: %+v", request.Messages)
	}
	if request.Messages[0].Role != model.RoleSystem {
		t.Fatalf("system prompt missing after hook: %+v", request.Messages)
	}
	if value, ok := request.Options["temperature"].(float64); !ok || value != 0.2 {
		t.Fatalf("context hook options missing: %+v", request.Options)
	}
}

type warmingProvider struct {
	mu       sync.Mutex
	requests []model.Request
}

func (p *warmingProvider) Stream(_ context.Context, req model.Request) (provider.Stream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return &fakeStream{chunks: []provider.Chunk{{Text: "ok"}}}, nil
}

func (p *warmingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *warmingProvider) lastPrompt() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 || len(p.requests[len(p.requests)-1].Messages) == 0 {
		return ""
	}
	messages := p.requests[len(p.requests)-1].Messages
	return messages[len(messages)-1].Content
}

func TestWarmingSendsIdleKeepAlive(t *testing.T) {
	store := testStore(t)
	fake := &warmingProvider{}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{
		MaxSteps: 1,
		Base:     base,
		Warming:  warming.Config{Enabled: true, Prompt: "keep warm", Interval: "10ms", Duration: "300ms"},
	})
	stream, err := a.Run(context.Background(), "s1", "hello", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	deadline := time.Now().Add(2 * time.Second)
	for fake.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.count() < 2 {
		t.Fatalf("warming request did not run: %d calls", fake.count())
	}
	if prompt := fake.lastPrompt(); prompt != "keep warm" {
		t.Fatalf("unexpected warming prompt: %q", prompt)
	}
	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Content == "keep warm" {
			t.Fatal("warming prompt should not be persisted")
		}
	}
}

func TestMaxOutputTokensFitsWindow(t *testing.T) {
	if got := maxOutputTokens(provider.Target{MaxOutput: 384000, Window: 1000000}, 0); got != 256000 {
		t.Fatalf("catalog limit should cap at 256k, got %d", got)
	}
	if got := maxOutputTokens(provider.Target{MaxOutput: 128000, Window: 200000}, 100000); got != 98000 {
		t.Fatalf("reply should leave room for the prompt, got %d", got)
	}
	if got := maxOutputTokens(provider.Target{}, 0); got != 0 {
		t.Fatalf("unknown limit should stay unset, got %d", got)
	}
}

func TestMergeOptionsKeepsModelDefaults(t *testing.T) {
	base := map[string]any{"temperature": 0.2, "reasoning_effort": "high"}
	got := mergeOptions(base, map[string]any{"reasoning_effort": "low"})
	if got["temperature"] != 0.2 || got["reasoning_effort"] != "low" {
		t.Fatalf("unexpected merged options: %+v", got)
	}
	if base["reasoning_effort"] != "high" {
		t.Fatalf("base options were mutated: %+v", base)
	}
}

func TestRetryDelayHonorsRetryAfter(t *testing.T) {
	if got := retryDelayAfter(&provider.StatusError{RetryAfter: 5 * time.Second}, time.Second); got != 5*time.Second {
		t.Fatalf("header should win when longer: %v", got)
	}
	if got := retryDelayAfter(&provider.StatusError{RetryAfter: 20 * time.Minute}, time.Second); got != retryAfterMax {
		t.Fatalf("header should be capped: %v", got)
	}
	if got := retryDelayAfter(nil, 2*time.Second); got != 2*time.Second {
		t.Fatalf("fallback should stay: %v", got)
	}
}

func TestCodemodeOnlyExposesOnlyExecute(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Add(echoTool{})
	registry.Add(question.NewTool(question.New()))
	registry.Add(tool.NewExecute(registry, "."))
	a := New(testStore(t), testCatalog(&fakeProvider{}, "test-model", 0), registry, nil, Options{CodemodeOnly: true})
	defs := a.definitions("s1")
	if len(defs) != 1 || defs[0].Name != "execute" {
		t.Fatalf("execute-only switch should hide direct tools: %+v", defs)
	}
}

func TestDefaultExposesDirectTools(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Add(tool.Direct(echoTool{}))
	registry.Add(tool.NewExecute(registry, "."))
	a := New(testStore(t), testCatalog(&fakeProvider{}, "test-model", 0), registry, nil, Options{})
	defs := a.definitions("s1")
	if len(defs) != 2 {
		t.Fatalf("default should keep direct tools: %+v", defs)
	}
}

func TestMergeHeadersKeepsDefaults(t *testing.T) {
	got := mergeHeaders(map[string]string{"X-Base": "1"}, map[string]string{"X-Agent": "2"})
	if got["X-Base"] != "1" || got["X-Agent"] != "2" {
		t.Fatalf("unexpected merged headers: %+v", got)
	}
}

func TestCustomAgentSystemIsNotAddedTwice(t *testing.T) {
	store := testStore(t)
	if err := store.SetAgent("s1", "explore"); err != nil {
		t.Fatal(err)
	}
	profiles := agents.New(agents.Config{}, "")
	a := New(store, testCatalog(&fakeProvider{}, "test-model", 0), tool.NewRegistry(), nil, Options{
		Agents: profiles,
		SystemFunc: func(string, provider.Target) string {
			return "custom base prompt"
		},
	})
	messages := a.withSystem("s1", provider.Target{}, nil)
	if len(messages) != 1 || messages[0].Content != "custom base prompt" {
		t.Fatalf("agent system should arrive from SystemFunc once: %+v", messages)
	}
	if strings.Contains(messages[0].Content, "file search specialist") {
		t.Fatalf("agent system was appended twice: %q", messages[0].Content)
	}
}

func TestFixedSystemStillAppendsCustomAgentSystem(t *testing.T) {
	store := testStore(t)
	if err := store.SetAgent("s1", "explore"); err != nil {
		t.Fatal(err)
	}
	a := New(store, testCatalog(&fakeProvider{}, "test-model", 0), tool.NewRegistry(), nil, Options{
		Agents: agents.New(agents.Config{}, ""),
		System: "fixed base prompt",
	})
	messages := a.withSystem("s1", provider.Target{}, nil)
	if len(messages) != 1 || !strings.Contains(messages[0].Content, "fixed base prompt") || !strings.Contains(messages[0].Content, "file search specialist") {
		t.Fatalf("fixed prompt should append the agent system: %+v", messages)
	}
}

func TestAssistantMessageCarriesModelMetadata(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "reply"}}}}
	a, store := newTestAgent(t, fake)
	a.profiles = agents.New(agents.Config{}, "")
	stream, err := a.Run(context.Background(), "s1", "hello", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type != event.AssistantMessage {
			continue
		}
		if ev.Data["model"] != "test-model" || ev.Data["provider"] != "test" || ev.Data["agent"] != "build" {
			t.Fatalf("assistant metadata missing: %+v", ev.Data)
		}
		return
	}
	t.Fatal("assistant message missing")
}

// A claimless recovery with only queued prompts delivers the first one as the
// turn's prompt instead of spending a model call on no new input.
func TestResumeQueuedOnlyDoesNotSpendAnEmptyCall(t *testing.T) {
	store := testStore(t)
	if _, err := store.QueueInputDelivery("s1", "inp_1", "queued while away", "queue"); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "queued answer"}}}}
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{MaxSteps: 4})

	stream, err := a.Resume(context.Background(), "s1", "", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Content != "queued while away" || messages[1].Content != "queued answer" {
		t.Fatalf("unexpected resumed history: %+v", messages)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("want one model call, got %d", len(fake.requests))
	}
	recovery, err := store.Recovery("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Inputs) != 0 || recovery.ClaimID != "" {
		t.Fatalf("queued input should be delivered exactly once: %+v", recovery)
	}
}

// Synthetic input stays outside model history until its boundary delivery, then
// keeps its original event type so clients render it as before.
func TestSyntheticInputStaysPendingUntilDelivered(t *testing.T) {
	started := make(chan string, 1)
	release := make(chan struct{})
	closed := false
	defer func() {
		if !closed {
			close(release)
		}
	}()
	registry := tool.NewRegistry()
	registry.Add(barrierTool{name: "hold", started: started, release: release})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "hold", Arguments: `{}`}}}},
		{{Text: "noticed"}},
	}}
	store := testStore(t)
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4})

	stream, err := a.Run(context.Background(), "s1", "start", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	if err := a.Notify("s1", event.JobCompletedEvent("job_1", "go test", "done", "", "tests passed", 0)); err != nil {
		t.Fatal(err)
	}
	recovery, err := store.Recovery("s1")
	if err != nil {
		t.Fatal(err)
	}
	pendingSynthetic := false
	for _, item := range recovery.Inputs {
		if item.Kind == session.KindSynthetic {
			pendingSynthetic = true
		}
	}
	if !pendingSynthetic {
		t.Fatalf("synthetic input should be pending: %+v", recovery.Inputs)
	}
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == event.JobCompleted {
			t.Fatalf("synthetic input became visible before delivery: %+v", ev)
		}
	}

	close(release)
	closed = true
	for range stream {
	}
	events, err = store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	delivered := false
	for _, ev := range events {
		if ev.Type == event.JobCompleted {
			delivered = true
			if ev.Data["inputId"] == nil || ev.Data["inputId"] == "" {
				t.Fatalf("delivered synthetic input lost its id: %+v", ev)
			}
		}
	}
	if !delivered {
		t.Fatal("synthetic input was never delivered")
	}
	recovery, err = store.Recovery("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Inputs) != 0 || recovery.ClaimID != "" {
		t.Fatalf("synthetic input should be delivered exactly once: %+v", recovery)
	}
}

// A queued manual compaction carries an inbox identity and can be cancelled
// before the boundary.
func TestQueuedCompactionCarriesIdentityAndCanBeCancelled(t *testing.T) {
	started := make(chan string, 1)
	release := make(chan struct{})
	closed := false
	defer func() {
		if !closed {
			close(release)
		}
	}()
	registry := tool.NewRegistry()
	registry.Add(barrierTool{name: "hold", started: started, release: release})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "hold", Arguments: `{}`}}}},
		{{Text: "after"}},
	}}
	store := testStore(t)
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4})

	stream, err := a.Run(context.Background(), "s1", "start", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	if err := a.CompactDelivery(context.Background(), "s1", provider.Selection{}, "steer"); err != nil {
		t.Fatal(err)
	}
	recovery, err := store.Recovery("s1")
	if err != nil {
		t.Fatal(err)
	}
	compactionID := ""
	for _, item := range recovery.Inputs {
		if item.Kind == session.KindCompaction {
			compactionID = item.ID
		}
	}
	if compactionID == "" {
		t.Fatalf("queued compaction should have an inbox id: %+v", recovery.Inputs)
	}
	if !a.CancelInput("s1", compactionID) {
		t.Fatal("cancelling the live compaction should succeed")
	}
	if _, err := store.CancelInput("s1", compactionID); err != nil {
		t.Fatal(err)
	}
	close(release)
	closed = true
	for range stream {
	}
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == event.Compaction {
			t.Fatalf("cancelled compaction still ran: %+v", ev)
		}
	}
}

// A move control stops the turn at its next boundary and keeps the claim open
// for the destination to continue.
func TestMoveControlHandsOffAtTheBoundary(t *testing.T) {
	started := make(chan string, 1)
	release := make(chan struct{})
	closed := false
	defer func() {
		if !closed {
			close(release)
		}
	}()
	registry := tool.NewRegistry()
	registry.Add(barrierTool{name: "hold", started: started, release: release})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "hold", Arguments: `{}`}}}},
	}}
	store := testStore(t)
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4})

	stream, err := a.Run(context.Background(), "s1", "start", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	moveID, queued, err := a.QueueMove("s1", "http://127.0.0.1:1", "", "steer")
	if err != nil {
		t.Fatal(err)
	}
	if !queued || moveID == "" {
		t.Fatalf("move should be queued: queued=%v id=%q", queued, moveID)
	}
	close(release)
	closed = true
	var last event.Event
	for ev := range stream {
		last = ev
	}
	if last.Type != event.RunEnd {
		t.Fatalf("want a run end at the move boundary, got %+v", last)
	}
	recovery, err := store.Recovery("s1")
	if err != nil {
		t.Fatal(err)
	}
	if recovery.ClaimID == "" {
		t.Fatal("move handoff should keep the turn claim open")
	}
	found := false
	for _, item := range recovery.Inputs {
		if item.ID == moveID && item.Kind == session.KindMove {
			found = true
		}
	}
	if !found {
		t.Fatalf("move control should stay pending until it is applied: %+v", recovery.Inputs)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("the move boundary should not spend another model call: %d", len(fake.requests))
	}
}

// A turn started only to run a queued compaction ends after the checkpoint
// instead of spending a model call on unchanged history.
func TestCompactionOnlyTurnDoesNotSpendAnEmptyCall(t *testing.T) {
	store := testStore(t)
	if _, err := store.Append("s1", event.User("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.Assistant("hi")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueueItem("s1", session.QueuedInput{
		ID:       "cmp_1",
		Kind:     session.KindCompaction,
		Delivery: "steer",
	}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "## Objective\n- done\n\n## Work State\n- done\n\n## Next Move\n- none\n"}}}}
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{MaxSteps: 4})

	stream, err := a.Resume(context.Background(), "s1", "", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if len(fake.requests) != 1 {
		t.Fatalf("compaction-only turn should make one summary call, got %d", len(fake.requests))
	}
	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Role == model.RoleAssistant && message.Content == "hi" {
			continue
		}
		if message.Role == model.RoleAssistant {
			t.Fatalf("compaction-only turn produced an extra reply: %+v", messages)
		}
	}
	recovery, err := store.Recovery("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Inputs) != 0 || recovery.ClaimID != "" {
		t.Fatalf("compaction should settle: %+v", recovery)
	}
}

// Admission is a live fact too, so clients update their pending projection
// without polling for the inbox.
func TestSubmitPublishesInboxFact(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "answer"}}}}
	a, _ := newTestAgent(t, fake)
	sub := a.Subscribe("s1")
	defer sub.Close()
	stream, _, err := a.Submit(context.Background(), "s1", Submission{Text: "hello"}, provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range stream {
		}
	}()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed")
			}
			if ev.Type != event.InputQueued || ev.Data["text"] != "hello" {
				continue
			}
			if ev.Data["id"] == nil || ev.Data["id"] == "" {
				t.Fatalf("queued fact lost its id: %+v", ev)
			}
			if ev.Seq == 0 || ev.Global == 0 {
				t.Fatalf("queued fact is missing its stream position: %+v", ev)
			}
			return
		case <-deadline:
			t.Fatal("admission did not publish an input.queued fact")
		}
	}
}

// A retried prompt id reconciles instead of being admitted twice.
func TestSubmitReconcilesByClientID(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "answer"}}}}
	a, store := newTestAgent(t, fake)
	stream, joined, err := a.Submit(context.Background(), "s1", Submission{Text: "once", ID: "inp_fixed"}, provider.Selection{})
	if err != nil || joined {
		t.Fatalf("first submit should start a turn: joined=%v err=%v", joined, err)
	}
	for range stream {
	}
	stream, joined, err = a.Submit(context.Background(), "s1", Submission{Text: "once", ID: "inp_fixed"}, provider.Selection{})
	if err != nil || joined || stream != nil {
		t.Fatalf("retry should reconcile without a turn: joined=%v err=%v", joined, err)
	}
	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Content != "once" {
		t.Fatalf("retry duplicated the prompt: %+v", messages)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("retry spent another model call: %d", len(fake.requests))
	}
}
