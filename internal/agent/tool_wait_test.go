package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/permission"
	"lingzhu/internal/provider"
	"lingzhu/internal/question"
	"lingzhu/internal/tool"
)

// longTool finishes only after a delay longer than any fixed grace period.
type longTool struct {
	delay time.Duration
}

func (longTool) Name() string            { return "long" }
func (longTool) Description() string     { return "slow tool" }
func (longTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t longTool) Run(ctx context.Context, _ tool.Call) (string, error) {
	select {
	case <-time.After(t.delay):
		return "finished", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// toolResults returns every tool result the turn persisted.
func toolResults(t *testing.T, a *Agent) []event.Event {
	t.Helper()
	events, err := a.store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	var results []event.Event
	for _, ev := range events {
		if ev.Type == event.ToolResult {
			results = append(results, ev)
		}
	}
	return results
}

func runTurn(t *testing.T, a *Agent) {
	t.Helper()
	stream, err := a.Run(context.Background(), "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
}

// A tool that outlives the old five-second grace must still settle with its own
// result: the turn is live and nothing cancelled it.
func TestToolOutlivingTheGracePeriodIsNotAborted(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "long", Arguments: `{}`}}}},
		{{Text: "done"}},
	}}
	registry := tool.NewRegistry()
	registry.Add(longTool{delay: 6 * time.Second})
	perm := permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})
	a := New(testStore(t), testCatalog(fake, "test-model", 0), registry, perm, Options{MaxSteps: 4})

	runTurn(t, a)

	results := toolResults(t, a)
	if len(results) != 1 {
		t.Fatalf("want one tool result, got %d", len(results))
	}
	if results[0].Data["output"] != "finished" || results[0].Data["failed"] != false {
		t.Fatalf("a live tool must settle with its own result: %+v", results[0].Data)
	}
}

// A question is a tool that waits for a person, so the turn must stay open for
// as long as the answer takes.
func TestQuestionWaitsForALateAnswer(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "question", Arguments: `{"questions":[{"question":"Which?","header":"Pick","options":[{"label":"A","description":"a"}]}]}`}}}},
		{{Text: "done"}},
	}}
	manager := question.New()
	registry := tool.NewRegistry()
	registry.Add(question.NewTool(manager))
	perm := permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})
	a := New(testStore(t), testCatalog(fake, "test-model", 0), registry, perm, Options{MaxSteps: 4, Questions: manager})

	answered := make(chan struct{})
	go func() {
		defer close(answered)
		// Reading the card takes longer than the old grace period.
		time.Sleep(6 * time.Second)
		for _, pending := range manager.Pending("s1") {
			_, _ = manager.Reply(pending.ID, question.Response{Answers: [][]string{{"A"}}})
		}
	}()

	runTurn(t, a)
	<-answered

	results := toolResults(t, a)
	if len(results) != 1 {
		t.Fatalf("want one tool result, got %d", len(results))
	}
	if results[0].Data["failed"] != false {
		t.Fatalf("the question was aborted while the user was answering: %+v", results[0].Data)
	}
	output, _ := results[0].Data["output"].(string)
	if output == "aborted" || output == "" {
		t.Fatalf("want the formatted answer, got %q", output)
	}
}

// A cancelled turn still bounds a tool that ignores cancellation, so a stuck
// call cannot hold the session open.
func TestCancelledTurnSettlesAStuckTool(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "stuck", Arguments: `{}`}}}},
	}}
	registry := tool.NewRegistry()
	registry.Add(stuckTool{started: make(chan struct{}, 1)})
	perm := permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})
	a := New(testStore(t), testCatalog(fake, "test-model", 0), registry, perm, Options{MaxSteps: 4})

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := a.Run(ctx, "s1", "go", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range stream {
		}
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()

	deadline := time.Now().Add(abortGrace + 5*time.Second)
	for time.Now().Before(deadline) {
		if results := toolResults(t, a); len(results) > 0 {
			if results[0].Data["output"] != "aborted" {
				t.Fatalf("want an aborted settlement, got %+v", results[0].Data)
			}
			if results[0].Data["cancelled"] != true {
				t.Fatalf("aborted tools must be marked cancelled: %+v", results[0].Data)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a cancelled turn never settled its stuck tool")
}

// stuckTool ignores cancellation entirely.
type stuckTool struct {
	started chan struct{}
}

func (stuckTool) Name() string            { return "stuck" }
func (stuckTool) Description() string     { return "ignores cancellation" }
func (stuckTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t stuckTool) Run(context.Context, tool.Call) (string, error) {
	t.started <- struct{}{}
	select {}
}
