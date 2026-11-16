package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"lingzhu/internal/agents"
	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/provider"
	"lingzhu/internal/tool"
)

// bigTool returns 8 KB so one call exceeds a small context window.
type bigTool struct{}

func (bigTool) Name() string            { return "big" }
func (bigTool) Description() string     { return "returns a large output" }
func (bigTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (bigTool) Run(ctx context.Context, call tool.Call) (string, error) {
	if err := call.Allow(ctx, "big", []string{"*"}, ""); err != nil {
		return "", err
	}
	return strings.Repeat("x", 8000), nil
}

func collect(t *testing.T, a *Agent, sessionID, prompt string) []event.Event {
	t.Helper()
	stream, err := a.Run(context.Background(), sessionID, prompt, provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	var events []event.Event
	for ev := range stream {
		events = append(events, ev)
	}
	return events
}

func find(events []event.Event, kind event.Type) (event.Event, bool) {
	for _, ev := range events {
		if ev.Type == kind {
			return ev, true
		}
	}
	return event.Event{}, false
}

func TestNativeCompactionUsesProviderCheckpoint(t *testing.T) {
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(bigTool{})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "big", Arguments: `{}`}}}},
		{{Compaction: "provider checkpoint"}},
		{{Text: "done"}},
	}}
	models := []provider.ModelSpec{{
		ID:                "test-model",
		Name:              "test-model",
		ContextWindow:     1000,
		NativeCompaction:  true,
		CompactionTrigger: 900,
	}}
	catalog := provider.Static("test", "Test", fake, models, "test-model")
	a := New(store, catalog, registry, nil, Options{MaxSteps: 4, CompactKeep: 100})

	collect(t, a, "s1", "go")
	if len(fake.requests) < 3 {
		t.Fatalf("want a compaction request, got %d requests", len(fake.requests))
	}
	compactionRequest := fake.requests[1]
	if !compactionRequest.NativeCompaction {
		t.Fatal("the compaction request should be marked native")
	}
	management, ok := compactionRequest.Options["context_management"].(map[string]any)
	if !ok {
		t.Fatalf("context_management missing: %+v", compactionRequest.Options)
	}
	edits, _ := management["edits"].([]map[string]any)
	if len(edits) != 1 || edits[0]["type"] != "compact_20260112" {
		t.Fatalf("unexpected edits: %+v", management["edits"])
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) == 0 || !strings.Contains(messages[0].Content, "provider checkpoint") {
		t.Fatalf("native checkpoint was not stored: %+v", messages)
	}
}

func TestCompactsWhenOverBudget(t *testing.T) {
	store := testStore(t)
	registry := tool.NewRegistry()
	registry.Add(bigTool{})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "big", Arguments: `{}`}}}},
		{{Text: "## Objective\n- resume the task\n\n## Work State\n### Active\n- resume\n\n## Next Move\n1. resume"}},
		{{Text: "done"}},
	}}
	a := New(store, testCatalog(fake, "test-model", 2000), registry, nil, Options{
		MaxSteps:    4,
		CompactKeep: 100,
	})

	events := collect(t, a, "s1", "go")
	if last := events[len(events)-1]; last.Type != event.RunEnd || last.Data["reason"] != "done" {
		t.Fatalf("unexpected end: %+v", last)
	}
	if _, ok := find(events, event.Compaction); !ok {
		t.Fatalf("no compaction event: %+v", events)
	}
	if len(fake.requests) != 3 {
		t.Fatalf("want 3 provider calls, got %d", len(fake.requests))
	}
	summaryCall := fake.requests[1]
	if len(summaryCall.Tools) != 0 {
		t.Fatalf("summary call should not offer tools: %+v", summaryCall.Tools)
	}
	if !strings.Contains(summaryCall.Messages[len(summaryCall.Messages)-1].Content, "summarize the conversation above") {
		t.Fatalf("summary prompt missing: %+v", summaryCall.Messages)
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || !strings.Contains(messages[0].Content, "<conversation-checkpoint>") {
		t.Fatalf("history was not collapsed into a checkpoint: %+v", messages)
	}
}

func TestOverflowCompactsAndRetries(t *testing.T) {
	store := testStore(t)
	if _, err := store.Append("s1", event.User("a"+strings.Repeat("x", 4000))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.Assistant("b"+strings.Repeat("y", 4000))); err != nil {
		t.Fatal(err)
	}

	fake := &fakeProvider{
		errs: []error{fmt.Errorf("%w: maximum context length", provider.ErrContextOverflow)},
		chunks: [][]provider.Chunk{
			nil,
			{{Text: "## Objective\n- continue\n\n## Work State\n### Active\n- continue\n\n## Next Move\n1. continue"}},
			{{Text: "recovered"}},
		},
	}
	a := New(store, testCatalog(fake, "test-model", 8000), tool.NewRegistry(), nil, Options{
		MaxSteps:    4,
		CompactKeep: 100,
	})

	events := collect(t, a, "s1", "c")
	if last := events[len(events)-1]; last.Type != event.RunEnd || last.Data["reason"] != "done" {
		t.Fatalf("run did not recover: %+v", events)
	}
	if len(fake.requests) != 3 {
		t.Fatalf("want overflow, summary and retry calls, got %d", len(fake.requests))
	}
	if !strings.Contains(fake.requests[1].Messages[len(fake.requests[1].Messages)-1].Content, "summarize the conversation above") {
		t.Fatalf("second call should be the summary: %+v", fake.requests[1].Messages)
	}
}

func TestClipKeepsValidUTF8(t *testing.T) {
	text := strings.Repeat("中文", maxToolOutput)
	out := clip(text)
	if !utf8.ValidString(out) {
		t.Fatal("clip produced invalid UTF-8")
	}
	if !strings.Contains(out, "chars omitted") {
		t.Fatalf("long text should be clipped: %q", out)
	}
}

// waitProvider blocks until the run is cancelled, which lets tests interrupt it.
type waitProvider struct {
	started chan struct{}
}

func (p *waitProvider) Stream(ctx context.Context, _ model.Request) (provider.Stream, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	return &waitStream{ctx: ctx}, nil
}

type waitStream struct {
	ctx context.Context
}

func (s *waitStream) Recv() (provider.Chunk, error) {
	<-s.ctx.Done()
	return provider.Chunk{}, s.ctx.Err()
}

func (s *waitStream) Close() error { return nil }

func TestConcurrentRunJoinsLiveTurnAndInterrupts(t *testing.T) {
	store := testStore(t)
	fake := &waitProvider{started: make(chan struct{}, 1)}
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{MaxSteps: 2})

	events, err := a.Run(context.Background(), "s1", "hi", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fake.started:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not reach the provider")
	}

	// A prompt admitted into a live turn joins it instead of failing.
	joined, err := a.Run(context.Background(), "s1", "again", provider.Selection{})
	if err != nil {
		t.Fatalf("want the prompt to join the live turn, got %v", err)
	}
	for range joined {
	}
	if !a.Interrupt("s1", false) {
		t.Fatal("interrupt should report a running turn")
	}

	var last event.Event
	for ev := range events {
		last = ev
	}
	if last.Type != event.RunEnd || last.Data["reason"] != "interrupted" {
		t.Fatalf("want interrupted end, got %+v", last)
	}
	if a.Running("s1") {
		t.Fatal("session should not be running after the turn ends")
	}
	if a.Interrupt("s1", false) {
		t.Fatal("interrupt after the turn ends should report false")
	}
}

func TestWaitReturnsWhenBaseContextIsCancelled(t *testing.T) {
	store := testStore(t)
	base, stopBase := context.WithCancel(context.Background())
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

	waited := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		waited <- a.Wait(ctx)
	}()
	select {
	case <-waited:
		t.Fatal("Wait returned while a run was still active")
	case <-time.After(100 * time.Millisecond):
	}

	// Shutdown: stop the base context, then drain the run.
	stopBase()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("Wait failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after shutdown")
	}

	var last event.Event
	for ev := range events {
		last = ev
	}
	if last.Type != event.RunEnd || last.Data["reason"] != "interrupted" {
		t.Fatalf("want interrupted end, got %+v", last)
	}
}

func TestWaitHonoursItsDeadline(t *testing.T) {
	store := testStore(t)
	fake := &waitProvider{started: make(chan struct{}, 1)}
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{MaxSteps: 2})

	if _, err := a.Run(context.Background(), "s1", "hi", provider.Selection{}); err != nil {
		t.Fatal(err)
	}
	<-fake.started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := a.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
	a.Interrupt("s1", false)
}

func TestSystemPromptIsSentWithoutPersisting(t *testing.T) {
	store := testStore(t)
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "ok"}}}}
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{
		MaxSteps: 2, System: "you are a test agent",
	})

	collect(t, a, "s1", "hi")
	if len(fake.requests) != 1 {
		t.Fatalf("want 1 provider call, got %d", len(fake.requests))
	}
	first := fake.requests[0].Messages[0]
	if first.Role != model.RoleSystem || first.Content != "you are a test agent" {
		t.Fatalf("system prompt not first: %+v", fake.requests[0].Messages)
	}
	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if m.Role == model.RoleSystem {
			t.Fatalf("system prompt must not be persisted: %+v", messages)
		}
	}
}

func TestUsePrecomputedAppliesWaitingSummary(t *testing.T) {
	a, store := newTestAgent(t, &fakeProvider{})
	if _, err := store.Append("s1", event.User("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.User("second")); err != nil {
		t.Fatal(err)
	}
	current, err := store.Context("s1")
	if err != nil {
		t.Fatal(err)
	}
	a.keep = 1
	split := keepTail(current.Messages, a.keep)
	if split <= 0 || split >= len(current.Messages) {
		t.Fatalf("unexpected split: %d", split)
	}
	a.precomps["s1"] = &precompaction{
		split:       split,
		fingerprint: messageFingerprint(current.Messages[:split]),
		summary:     "## Objective\n- test",
	}
	if !a.usePrecomputed("s1", provider.Target{Window: 0}, current) {
		t.Fatal("waiting summary was not applied")
	}
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1].Type != event.Compaction {
		t.Fatalf("compaction event missing: %+v", events)
	}
}

func TestUsePrecomputedRejectsChangedHistory(t *testing.T) {
	a, store := newTestAgent(t, &fakeProvider{})
	if _, err := store.Append("s1", event.User("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.User("second")); err != nil {
		t.Fatal(err)
	}
	current, err := store.Context("s1")
	if err != nil {
		t.Fatal(err)
	}
	a.keep = 1
	split := keepTail(current.Messages, a.keep)
	a.precomps["s1"] = &precompaction{split: split, fingerprint: "changed", summary: "## Objective\n- test"}
	if a.usePrecomputed("s1", provider.Target{Window: 0}, current) {
		t.Fatal("stale summary must not be applied")
	}
}

func TestPrecomputeAllowedHonorsCacheAndFailures(t *testing.T) {
	a, _ := newTestAgent(t, &fakeProvider{})
	a.keep = 1
	target := provider.Target{Window: 100_000, CacheTTL: 5 * time.Minute}
	if a.precomputeAllowed("s1", target, time.Now()) {
		t.Fatal("no previous response should skip precompute")
	}
	a.markResponse("s1")
	if !a.precomputeAllowed("s1", target, time.Now()) {
		t.Fatal("a warm cache should allow precompute")
	}
	a.preMu.Lock()
	a.lastResponse["s1"] = time.Now().Add(-5 * time.Minute)
	a.preMu.Unlock()
	if a.precomputeAllowed("s1", target, time.Now()) {
		t.Fatal("an expired cache should skip precompute")
	}
	a.preMu.Lock()
	a.lastResponse["s1"] = time.Now()
	a.preFails["s1"] = 2
	a.preMu.Unlock()
	if a.precomputeAllowed("s1", target, time.Now()) {
		t.Fatal("the failure cap should skip precompute")
	}
}

func TestUsePrecomputedRejectsLongTail(t *testing.T) {
	a, store := newTestAgent(t, &fakeProvider{})
	if _, err := store.Append("s1", event.User("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.User(strings.Repeat("x", 4000))); err != nil {
		t.Fatal(err)
	}
	current, err := store.Context("s1")
	if err != nil {
		t.Fatal(err)
	}
	a.keep = 10
	split := keepTail(current.Messages, a.keep)
	a.precomps["s1"] = &precompaction{
		split:       split,
		fingerprint: messageFingerprint(current.Messages[:split]),
		summary:     "## Objective\n- test",
	}
	if a.usePrecomputed("s1", provider.Target{Window: 1000}, current) {
		t.Fatal("a tail over 40% of the ceiling must force a fresh summary")
	}
}

func TestCompactionUsesCompactionAgentModel(t *testing.T) {
	store := testStore(t)
	if _, err := store.Append("s1", event.User("first message")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.Assistant("answer")); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{Text: "## Objective\n- keep going\n\n## Work State\n### Active\n- keep going\n\n## Next Move\n1. keep going"}},
	}}
	catalog := provider.Static("test", "Test", fake, []provider.ModelSpec{
		{ID: "session-model", ContextWindow: 100000},
		{ID: "compact-model", ContextWindow: 100000},
	}, "session-model")
	profiles := agents.New(agents.Config{Agents: map[string]agents.Definition{
		"compaction": {Model: "test/compact-model"},
	}}, "")
	a := New(store, catalog, tool.NewRegistry(), nil, Options{Agents: profiles, CompactKeep: 10})

	current, err := store.Context("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.compact(context.Background(), "s1", catalog.Default(), current, 0, "manual", ""); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("want one summary call, got %d", len(fake.requests))
	}
	if got := fake.requests[0].Model; got != "compact-model" {
		t.Fatalf("compaction should use the compaction agent model, got %q", got)
	}
}

func TestCompactionFallsBackToSessionModel(t *testing.T) {
	store := testStore(t)
	if _, err := store.Append("s1", event.User("first message")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.Assistant("answer")); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{Text: "## Objective\n- keep going\n\n## Work State\n### Active\n- keep going\n\n## Next Move\n1. keep going"}},
	}}
	catalog := testCatalog(fake, "session-model", 100000)
	profiles := agents.New(agents.Config{}, "")
	a := New(store, catalog, tool.NewRegistry(), nil, Options{Agents: profiles, CompactKeep: 10})

	current, err := store.Context("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.compact(context.Background(), "s1", catalog.Default(), current, 0, "manual", ""); err != nil {
		t.Fatal(err)
	}
	if got := fake.requests[0].Model; got != "session-model" {
		t.Fatalf("compaction should fall back to the session model, got %q", got)
	}
}
