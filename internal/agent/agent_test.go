package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/permission"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
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

func newTestAgent(t *testing.T, fake *fakeProvider) (*Agent, *session.Store) {
	t.Helper()
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	if len(final.Tools) != 0 {
		t.Fatalf("wrap-up must not offer tools: %+v", final.Tools)
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
	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range stream {
		last = ev
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
	if len(messages) != 3 {
		t.Fatalf("synthetic notice must not be stored: %+v", messages)
	}
	if messages[1].Content != "first half " || messages[2].Content != "second half" {
		t.Fatalf("continuation text was not recorded: %+v", messages)
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
	if notice.Role != model.RoleUser || !strings.Contains(notice.Content, "interrupted mid-stream") {
		t.Fatalf("resume instruction missing: %+v", notice)
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[1].Content != "partial " || messages[2].Content != "rest" {
		t.Fatalf("partial output was not preserved: %+v", messages)
	}
}

func TestSystemPromptIsRebuiltForEachRequest(t *testing.T) {
	call := provider.Chunk{ToolCalls: []provider.ToolCallDelta{
		{Index: 0, ID: "call_1", Name: "echo", Arguments: `{}`},
	}}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{call}, {{Text: "done"}}}}
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	registry.Add(echoTool{})
	built := 0
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{
		MaxSteps: 4,
		SystemFunc: func() string {
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
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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

func TestEnqueueRequiresRunningTurn(t *testing.T) {
	a, _ := newTestAgent(t, &fakeProvider{})
	if _, err := a.Enqueue("s1", "late"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("want ErrNotRunning, got %v", err)
	}
}

func TestRunClaimsTurnAndInterruptCancelsQueue(t *testing.T) {
	started := make(chan string, 1)
	release := make(chan struct{})
	defer close(release)
	registry := tool.NewRegistry()
	registry.Add(barrierTool{name: "hold", started: started, release: release})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "hold", Arguments: `{}`}}}},
	}}
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := a.Enqueue("s1", "queued"); err != nil {
		t.Fatal(err)
	}
	recoveries, err := store.Recoveries()
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveries) != 1 || recoveries[0].ClaimID == "" || len(recoveries[0].Inputs) != 1 {
		t.Fatalf("claim or queued input was not persisted: %+v", recoveries)
	}

	a.Interrupt("s1")
	for range stream {
	}
	recoveries, err = store.Recoveries()
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveries) != 0 {
		t.Fatalf("interrupt should close the claim and queue: %+v", recoveries)
	}
}

func TestResumeDeliversQueuedInputAndReleasesClaim(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := New(store, testCatalog(&fakeProvider{}, "", 0), tool.NewRegistry(), nil, Options{MaxSteps: 4})
	if _, err := a.Run(context.Background(), "s1", "x", provider.Selection{}); err == nil {
		t.Fatal("want error when model is empty")
	}
}

func TestPermissionRejectContinuesRun(t *testing.T) {
	rules := permission.Rules{{Action: "echo", Resource: "*", Effect: permission.Ask}}
	perm := permission.New(rules)
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	for ev := range stream {
		if ev.Type != event.PermissionAsked {
			continue
		}
		asked++
		id, _ := ev.Data["id"].(string)
		if err := a.Reply(id, permission.Reject, "not allowed"); err != nil {
			t.Fatal(err)
		}
	}
	if asked != 1 {
		t.Fatalf("want 1 permission request, got %d", asked)
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
	if !strings.Contains(toolResult, "permission denied") || !strings.Contains(toolResult, "not allowed") {
		t.Fatalf("model should see the denial and feedback, got %q", toolResult)
	}
	if messages[len(messages)-1].Content != "understood" {
		t.Fatalf("run should continue after denial, got %+v", messages[len(messages)-1])
	}
}
