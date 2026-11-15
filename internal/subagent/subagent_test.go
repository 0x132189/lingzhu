package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"lingzhu/internal/agents"
	"lingzhu/internal/event"
	"lingzhu/internal/permission"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
)

type runRecord struct {
	sessionID  string
	prompt     string
	selection  provider.Selection
	unattended bool
}

type fakeRunner struct {
	mu       sync.Mutex
	store    *session.Store
	runs     []runRecord
	notified []event.Event
	text     string
	reason   string
	errText  string
	runErr   error
	notifyCh chan event.Event
}

func (f *fakeRunner) Run(_ context.Context, sessionID, prompt string, selection provider.Selection) (<-chan event.Event, error) {
	return f.run(false, sessionID, prompt, selection)
}

func (f *fakeRunner) RunUnattended(_ context.Context, sessionID, prompt string, selection provider.Selection) (<-chan event.Event, error) {
	return f.run(true, sessionID, prompt, selection)
}

func (f *fakeRunner) run(unattended bool, sessionID, prompt string, selection provider.Selection) (<-chan event.Event, error) {
	f.mu.Lock()
	f.runs = append(f.runs, runRecord{sessionID: sessionID, prompt: prompt, selection: selection, unattended: unattended})
	f.mu.Unlock()
	if f.runErr != nil {
		return nil, f.runErr
	}
	if f.store != nil {
		if _, err := f.store.Append(sessionID, event.User(prompt)); err != nil {
			return nil, err
		}
	}
	ch := make(chan event.Event, 3)
	ch <- event.Assistant(f.text)
	if f.errText != "" {
		ch <- event.ErrorEvent(f.errText)
	}
	reason := f.reason
	if reason == "" {
		reason = "done"
	}
	ch <- event.End(reason)
	close(ch)
	return ch, nil
}

func (f *fakeRunner) Notify(sessionID string, ev event.Event) error {
	f.mu.Lock()
	f.notified = append(f.notified, ev)
	f.mu.Unlock()
	if f.notifyCh != nil {
		f.notifyCh <- ev
	}
	return nil
}

func (f *fakeRunner) Resolve(selection provider.Selection) (provider.Target, error) {
	target := provider.Target{ProviderID: "test", Model: "test-model"}
	if selection.Provider != "" {
		target.ProviderID = selection.Provider
	}
	if selection.Model != "" {
		target.Model = selection.Model
	}
	return target, nil
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

func allowAll() *permission.Manager {
	return permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})
}

func call(sessionID, input string) tool.Call {
	return tool.Call{SessionID: sessionID, Name: "subagent", Input: json.RawMessage(input), Perm: allowAll()}
}

func TestRecoverDeliversPendingBackgroundCompletion(t *testing.T) {
	store := testStore(t)
	fake := &fakeRunner{store: store, text: "finished", reason: "done", notifyCh: make(chan event.Event, 1)}
	manager := New(fake, store, t.Logf)
	if _, err := store.Append("parent", event.User("launch")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("parent", event.SubagentStartedEvent("sub_child", "child work", "general", 0)); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []event.Event{
		event.User("do it"),
		event.Assistant("finished"),
		event.TurnStartedEvent("turn-1"),
		event.TurnFinishedEvent("turn-1"),
	} {
		if _, err := store.Append("sub_child", ev); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Recover(ctx)

	select {
	case notification := <-fake.notifyCh:
		if notification.Type != event.SubagentCompleted {
			t.Fatalf("notification type = %s", notification.Type)
		}
		if notification.Data["id"] != "sub_child" || notification.Data["status"] != "completed" {
			t.Fatalf("unexpected notification: %+v", notification.Data)
		}
		if text, _ := notification.Data["text"].(string); !strings.Contains(text, "finished") {
			t.Fatalf("completion text missing: %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background completion was not delivered after recovery")
	}
}

func TestRecoverIgnoresAlreadyCompletedBackgroundChild(t *testing.T) {
	store := testStore(t)
	fake := &fakeRunner{store: store, notifyCh: make(chan event.Event, 1)}
	manager := New(fake, store, t.Logf)
	if _, err := store.Append("parent", event.SubagentStartedEvent("sub_child", "child work", "general", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("parent", event.SubagentCompletedEvent("sub_child", "child work", "completed", "done", "")); err != nil {
		t.Fatal(err)
	}
	manager.Recover(context.Background())
	select {
	case notification := <-fake.notifyCh:
		t.Fatalf("completed child notified twice: %+v", notification)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestChildrenListsCreatedChild(t *testing.T) {
	store := testStore(t)
	fake := &fakeRunner{store: store, text: "child answer"}
	manager := New(fake, store, t.Logf)

	if _, err := manager.Tool().Run(context.Background(), call("root", `{"description":"child work","prompt":"do it"}`)); err != nil {
		t.Fatal(err)
	}
	children := manager.Children("root")
	if len(children) != 1 {
		t.Fatalf("want one child, got %+v", children)
	}
	child := children[0]
	if child.ID != fake.runs[0].sessionID || child.Agent != "general" || child.Title != "[subagent] child work" {
		t.Fatalf("unexpected child: %+v", child)
	}
	if child.Started.IsZero() {
		t.Fatalf("child start time missing: %+v", child)
	}
	if other := manager.Children("another"); len(other) != 0 {
		t.Fatalf("another parent must not see the child: %+v", other)
	}
}

func TestForegroundSubagentCreatesChild(t *testing.T) {
	store := testStore(t)
	fake := &fakeRunner{store: store, text: "child answer"}
	manager := New(fake, store, t.Logf)

	out, err := manager.Tool().Run(context.Background(), call("root", `{"description":"child work","prompt":"do it"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "child answer") || !strings.Contains(out, "(subagent session: sub_") {
		t.Fatalf("unexpected output: %q", out)
	}
	if len(fake.runs) != 1 || fake.runs[0].prompt != "do it" || fake.runs[0].selection.Model != "test-model" {
		t.Fatalf("unexpected run: %+v", fake.runs)
	}
	child := fake.runs[0].sessionID
	if !strings.HasPrefix(child, "sub_") {
		t.Fatalf("unexpected child id: %q", child)
	}
	meta, err := store.Meta(child)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Provider != "test" || meta.Model != "test-model" {
		t.Fatalf("child model not stored: %+v", meta)
	}
	info, err := store.Info(child)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(info.Title, "[subagent] child work") {
		t.Fatalf("child title missing: %q", info.Title)
	}
}

func TestBackgroundSubagentNotifiesParent(t *testing.T) {
	store := testStore(t)
	fake := &fakeRunner{store: store, text: "background answer", notifyCh: make(chan event.Event, 1)}
	manager := New(fake, store, t.Logf)

	out, err := manager.Tool().Run(context.Background(), call("root", `{"description":"bg work","prompt":"do bg","background":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "working in the background") || !strings.Contains(out, "sessionID: sub_") {
		t.Fatalf("unexpected background output: %q", out)
	}

	select {
	case ev := <-fake.notifyCh:
		if ev.Type != event.SubagentCompleted {
			t.Fatalf("unexpected event type: %s", ev.Type)
		}
		text, _ := ev.Data["text"].(string)
		if !strings.Contains(text, "background answer") || !strings.Contains(text, `state="completed"`) {
			t.Fatalf("unexpected notification: %q", text)
		}
		if ev.Data["agent"] != "general" || ev.Data["source"] != "subagent" {
			t.Fatalf("notification metadata missing: %+v", ev.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background notification missing")
	}
}

func TestSubagentDescriptionFiltersDeniedAgents(t *testing.T) {
	store := testStore(t)
	fake := &fakeRunner{store: store, text: "ok"}
	manager := New(fake, store, t.Logf)
	manager.SetAgents(agents.New(agents.Config{}, ""))

	// An agent that may not spawn subagents should not be offered any.
	rules := permission.Rules{{Action: "subagent", Resource: "*", Effect: permission.Deny}}
	description := manager.Tool().DescriptionFor(rules)
	if strings.Contains(description, "Available agents:") {
		t.Fatalf("a denied caller should not see an agent list:\n%s", description)
	}
	// An allow lists the usable subagents, one per line, sorted by id.
	allow := permission.Rules{{Action: "subagent", Resource: "*", Effect: permission.Allow}}
	description = manager.Tool().DescriptionFor(allow)
	if !strings.Contains(description, "- explore:") || !strings.Contains(description, "- general:") {
		t.Fatalf("an allowed caller should see the subagents:\n%s", description)
	}
	if strings.Index(description, "- explore:") > strings.Index(description, "- general:") {
		t.Fatalf("the agent list should be sorted by id:\n%s", description)
	}
	// A narrowed deny removes just that agent.
	narrowed := permission.Rules{
		{Action: "subagent", Resource: "*", Effect: permission.Allow},
		{Action: "subagent", Resource: "general", Effect: permission.Deny},
	}
	description = manager.Tool().DescriptionFor(narrowed)
	if strings.Contains(description, "- general:") || !strings.Contains(description, "- explore:") {
		t.Fatalf("a narrowed deny should hide only that agent:\n%s", description)
	}
}

func TestSubagentContinuationSwitchesModelWithAgent(t *testing.T) {
	store := testStore(t)
	fake := &fakeRunner{store: store, text: "first"}
	manager := New(fake, store, t.Logf)
	manager.SetAgents(agents.New(agents.Config{Agents: map[string]agents.Definition{
		"general": {Model: "test/general-model"},
		"helper":  {Mode: agents.ModeSubagent, Model: "test/helper-model"},
	}}, ""))

	first, err := manager.Tool().Run(context.Background(), call("root", `{"description":"a","prompt":"one"}`))
	if err != nil {
		t.Fatal(err)
	}
	child := fake.runs[0].sessionID
	if !strings.Contains(first, child) {
		t.Fatalf("unexpected first result: %q", first)
	}
	if got := fake.runs[0].selection; got.Provider != "test" || got.Model != "general-model" {
		t.Fatalf("new child should use its agent model, got %+v", got)
	}
	// Switching agents on a continuation must carry the new agent's model, not
	// silently keep running on the old one.
	if _, err := manager.Tool().Run(context.Background(), call("root", `{"description":"a","prompt":"two","agent":"helper","sessionID":"`+child+`"}`)); err != nil {
		t.Fatal(err)
	}
	if got := fake.runs[1].selection; got.Model != "helper-model" {
		t.Fatalf("a switched agent should change the model, got %+v", got)
	}
	meta, err := store.Meta(child)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Model != "helper-model" || meta.Agent != "helper" {
		t.Fatalf("the child should record the switched agent and model: %+v", meta)
	}
	// A continuation with the same agent keeps the child's own model.
	if _, err := manager.Tool().Run(context.Background(), call("root", `{"description":"a","prompt":"three","agent":"helper","sessionID":"`+child+`"}`)); err != nil {
		t.Fatal(err)
	}
	if got := fake.runs[2].selection; got.Model != "helper-model" {
		t.Fatalf("an unchanged agent should keep the child model, got %+v", got)
	}
}

func TestSubagentContinuationAndParentOwnership(t *testing.T) {
	store := testStore(t)
	fake := &fakeRunner{store: store, text: "first"}
	manager := New(fake, store, t.Logf)

	first, err := manager.Tool().Run(context.Background(), call("root", `{"description":"a","prompt":"one"}`))
	if err != nil {
		t.Fatal(err)
	}
	child := fake.runs[0].sessionID
	second, err := manager.Tool().Run(context.Background(), call("root", `{"description":"a","prompt":"two","sessionID":"`+child+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, child) || !strings.Contains(second, child) {
		t.Fatalf("continuation did not reuse child: %q / %q", first, second)
	}
	if len(fake.runs) != 2 || fake.runs[1].sessionID != child || fake.runs[1].prompt != "two" {
		t.Fatalf("unexpected continuation run: %+v", fake.runs)
	}

	if _, err := manager.Tool().Run(context.Background(), call("other", `{"description":"a","prompt":"three","sessionID":"`+child+`"}`)); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("want ownership error, got %v", err)
	}
}

func TestSubagentDepthAndValidation(t *testing.T) {
	store := testStore(t)
	fake := &fakeRunner{store: store, text: "no"}
	manager := New(fake, store, t.Logf)

	if _, err := manager.Tool().Run(context.Background(), call("sub_parent", `{"description":"a","prompt":"x"}`)); err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("want depth error, got %v", err)
	}
	if _, err := manager.Tool().Run(context.Background(), call("root", `{"description":"a","prompt":"x","agent":"unknown"}`)); err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("want agent error, got %v", err)
	}
	if _, err := manager.Tool().Run(context.Background(), call("root", `{"description":"a","prompt":"x","model":"bad"}`)); err == nil || !strings.Contains(err.Error(), "provider/model") {
		t.Fatalf("want model error, got %v", err)
	}
	if _, err := manager.Tool().Run(context.Background(), call("root", `{"description":"a","prompt":""}`)); err == nil || !strings.Contains(err.Error(), "prompt is empty") {
		t.Fatalf("want prompt error, got %v", err)
	}
}

func TestSubagentPermissionAndFailure(t *testing.T) {
	store := testStore(t)
	fake := &fakeRunner{store: store, text: "partial", reason: "error", errText: "boom"}
	manager := New(fake, store, t.Logf)

	deny := tool.Call{
		SessionID: "root",
		Name:      "subagent",
		Input:     json.RawMessage(`{"description":"a","prompt":"x"}`),
		Perm:      permission.New(permission.Rules{{Action: "subagent", Resource: "*", Effect: permission.Deny}}),
	}
	if _, err := manager.Tool().Run(context.Background(), deny); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("want permission denial, got %v", err)
	}

	out, err := manager.Tool().Run(context.Background(), call("root", `{"description":"a","prompt":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want failure, got %v", err)
	}
	if !strings.Contains(out, "partial") {
		t.Fatalf("partial output missing: %q", out)
	}
}

func TestCompletionNoticeMatchesReference(t *testing.T) {
	completed := completionNotice("sub_1", "task", "completed", "", "")
	if !strings.Contains(completed, `state="completed"`) || !strings.Contains(completed, "Subagent completed without a text response.") {
		t.Fatalf("unexpected completed notice: %s", completed)
	}
	failed := completionNotice("sub_1", "task", "error", "", "boom")
	if !strings.Contains(failed, `state="error"`) || !strings.Contains(failed, "boom") {
		t.Fatalf("unexpected error notice: %s", failed)
	}
	cancelled := completionNotice("sub_1", "task", "cancelled", "ignored", "")
	if !strings.Contains(cancelled, `state="cancelled"`) || !strings.Contains(cancelled, "Subagent cancelled") {
		t.Fatalf("unexpected cancelled notice: %s", cancelled)
	}
}
