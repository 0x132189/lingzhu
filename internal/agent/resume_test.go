package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"lingzhu/internal/event"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
)

// TestResumeStopsAfterTheAttemptBudget proves a turn that crashes in the same
// place cannot restart with every process launch.
func TestResumeStopsAfterTheAttemptBudget(t *testing.T) {
	store := testStore(t)
	if _, err := store.Append("s1", event.TurnStartedEvent("turn_1")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < session.MaxResumeAttempts; i++ {
		if _, err := store.Append("s1", event.TurnResumedEvent("turn_1")); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{{Text: "should not run"}}}}
	a := New(store, testCatalog(fake, "test-model", 0), tool.NewRegistry(), nil, Options{MaxSteps: 4})

	if _, err := a.Resume(context.Background(), "s1", "turn_1", provider.Selection{}); err == nil {
		t.Fatal("a turn past its attempt budget must not resume")
	}
	if len(fake.requests) != 0 {
		t.Fatalf("no model request should run, got %d", len(fake.requests))
	}
	recoveries, err := store.Recoveries()
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveries) != 0 {
		t.Fatalf("an abandoned turn must release its claim: %+v", recoveries)
	}
	// The reason is durable, so the transcript shows why the turn stopped.
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	var reported bool
	for _, ev := range events {
		if ev.Type == event.Synthetic && strings.Contains(fmt.Sprint(ev.Data["text"]), "abandoned") {
			reported = true
		}
	}
	if !reported {
		t.Fatal("an abandoned turn should record why it stopped")
	}
}

// TestResumeRecordsEachAttempt proves the counter advances before the turn
// runs, so a crash during the resume still spends budget.
func TestResumeRecordsEachAttempt(t *testing.T) {
	store := testStore(t)
	if _, err := store.Append("s1", event.TurnStartedEvent("turn_1")); err != nil {
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
	// The attempt is recorded before the turn runs, so it survives a crash
	// during the resume. A finished turn releases its claim afterwards.
	stored, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	var attempts int
	for _, ev := range stored {
		if ev.Type == event.TurnResumed {
			attempts++
		}
	}
	if attempts != 1 {
		t.Fatalf("want one recorded attempt, got %d", attempts)
	}
	recovery, err := store.Recovery("s1")
	if err != nil {
		t.Fatal(err)
	}
	if recovery.ClaimID != "" {
		t.Fatalf("a completed resume releases the claim: %+v", recovery)
	}
}

// hugeTool returns an output far past the tool result limit. It stands in for
// an MCP server or plugin, which do not bound their own output.
type hugeTool struct{ size int }

func (b hugeTool) Name() string            { return "huge" }
func (b hugeTool) Description() string     { return "huge output" }
func (b hugeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (b hugeTool) Run(ctx context.Context, call tool.Call) (string, error) {
	return strings.Repeat("line of output\n", b.size), nil
}

// TestToolResultIsBoundedForEveryTool proves the agent caps a result it did
// not produce, so an unbounded tool cannot fill the context window.
func TestToolResultIsBoundedForEveryTool(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Add(hugeTool{size: 20000})
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "huge", Arguments: `{}`}}}},
		{{Text: "done"}},
	}}
	store := testStore(t)
	a := New(store, testCatalog(fake, "test-model", 0), registry, nil, Options{MaxSteps: 4})

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
		if message.Role == "tool" {
			result = message.Content
		}
	}
	if result == "" {
		t.Fatal("the tool result is missing")
	}
	if len(result) > 64*1024 {
		t.Fatalf("the result should be bounded, got %d bytes", len(result))
	}
	if !strings.Contains(result, "full output saved to") {
		t.Fatalf("a bounded result names the saved file:\n%s", result[len(result)-200:])
	}
}
