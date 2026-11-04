package session

import (
	"errors"
	"os"
	"testing"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
)

func TestAppendAndRebuild(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	events := []event.Event{
		event.User("hi"),
		event.Assistant("let me look"),
		event.ToolCallEvent("call_1", "bash", `{"command":"ls"}`),
		event.ToolResultEvent("call_1", "bash", "a.txt", false),
		event.Assistant("done"),
	}
	for _, ev := range events {
		if _, err := store.Append("s1", ev); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Append("s1", event.Delta("ignored")); err != nil {
		t.Fatal(err)
	}

	stored, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(events) {
		t.Fatalf("want %d durable events, got %d", len(events), len(stored))
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Role{model.RoleUser, model.RoleAssistant, model.RoleTool, model.RoleAssistant}
	if len(messages) != len(want) {
		t.Fatalf("want %d messages, got %d: %+v", len(want), len(messages), messages)
	}
	for i, role := range want {
		if messages[i].Role != role {
			t.Fatalf("message %d: want role %q, got %q", i, role, messages[i].Role)
		}
	}
	if got := messages[1].ToolCalls; len(got) != 1 || got[0].Name != "bash" || got[0].ID != "call_1" {
		t.Fatalf("tool call not attached to assistant: %+v", got)
	}
	if messages[2].ToolCallID != "call_1" || messages[2].Content != "a.txt" {
		t.Fatalf("tool result mismatch: %+v", messages[2])
	}
}

func TestRejectsInvalidID(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("../escape", event.User("x")); err == nil {
		t.Fatal("want error for path traversal id")
	}
}

func TestList(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"b", "a"} {
		if _, err := store.Append(id, event.User("x")); err != nil {
			t.Fatal(err)
		}
	}
	infos, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].ID != "a" || infos[1].ID != "b" {
		t.Fatalf("want sorted [a b], got %+v", infos)
	}
	if infos[0].Title != "x" {
		t.Fatalf("title should fall back to the first user message, got %q", infos[0].Title)
	}
	if _, err := store.Events("missing"); err != nil {
		t.Fatalf("missing session should read as empty: %v", err)
	}
}

func TestAppendAssignsSequence(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []event.Event{event.User("a"), event.Assistant("b")} {
		if _, err := store.Append("s1", ev); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Append("s1", event.Delta("not durable")); err != nil {
		t.Fatal(err)
	}
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("unexpected sequence numbers: %+v", events)
	}
	after, err := store.EventsAfter("s1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Seq != 2 {
		t.Fatalf("unexpected replay: %+v", after)
	}
}

func TestSequenceSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.User("a")); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Append("s1", event.Assistant("b")); err != nil {
		t.Fatal(err)
	}
	events, err := reopened.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Seq != 2 {
		t.Fatalf("sequence should continue after reopen: %+v", events)
	}
}

func TestSetTitleAndDelete(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.User("hello")); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTitle("s1", "My session"); err != nil {
		t.Fatal(err)
	}
	info, err := store.Info("s1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "My session" {
		t.Fatalf("unexpected title: %+v", info)
	}
	if err := store.Delete("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Info("s1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted session should be missing, got %v", err)
	}
}

func TestSetModelPreservesTitleAndInfo(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.User("hello")); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTitle("s1", "Keep me"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetModel("s1", "work", "fast-model"); err != nil {
		t.Fatal(err)
	}
	info, err := store.Info("s1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "Keep me" || info.Provider != "work" || info.Model != "fast-model" {
		t.Fatalf("unexpected info: %+v", info)
	}
	meta, err := store.Meta("s1")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Provider != "work" || meta.Model != "fast-model" {
		t.Fatalf("unexpected meta: %+v", meta)
	}
}

func TestRecoveriesTracksClaimsAndInput(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueueInput("s1", "inp_1", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.TurnStartedEvent("turn_1")); err != nil {
		t.Fatal(err)
	}

	list, err := store.Recoveries()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ClaimID != "turn_1" || len(list[0].Inputs) != 1 {
		t.Fatalf("unexpected recovery: %+v", list)
	}
	if list[0].Inputs[0].ID != "inp_1" || list[0].Inputs[0].Text != "hello" {
		t.Fatalf("unexpected queued input: %+v", list[0].Inputs)
	}

	if _, err := store.Append("s1", event.UserInput("hello", "inp_1")); err != nil {
		t.Fatal(err)
	}
	list, _ = store.Recoveries()
	if len(list) != 1 || len(list[0].Inputs) != 0 {
		t.Fatalf("delivered input should not recover: %+v", list)
	}

	if err := store.FinishTurn("s1", "turn_1"); err != nil {
		t.Fatal(err)
	}
	list, _ = store.Recoveries()
	if len(list) != 0 {
		t.Fatalf("finished turn should not recover: %+v", list)
	}

	if _, err := store.QueueInput("s1", "inp_2", "cancelled"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.TurnStartedEvent("turn_2")); err != nil {
		t.Fatal(err)
	}
	if err := store.CancelTurn("s1", "turn_2"); err != nil {
		t.Fatal(err)
	}
	list, _ = store.Recoveries()
	if len(list) != 0 {
		t.Fatalf("cancelled turn should clear its input: %+v", list)
	}

	if _, err := store.QueueInput("s1", "inp_3", "after cancel"); err != nil {
		t.Fatal(err)
	}
	list, _ = store.Recoveries()
	if len(list) != 1 || list[0].ClaimID != "" || len(list[0].Inputs) != 1 {
		t.Fatalf("input after cancel should recover: %+v", list)
	}
}

func TestSettleToolsOnlyAfterLatestCompaction(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []event.Event{
		event.ToolCallEvent("old", "bash", `{}`),
		event.CompactionEvent("summary", 1),
		event.ToolCallEvent("new", "read_file", `{}`),
	} {
		if _, err := store.Append("s1", ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SettleTools("s1"); err != nil {
		t.Fatal(err)
	}
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, ev := range events {
		if ev.Type == event.ToolResult {
			id, _ := ev.Data["id"].(string)
			results = append(results, id)
		}
	}
	if len(results) != 1 || results[0] != "new" {
		t.Fatalf("only the post-compaction call should settle, got %+v", results)
	}
}

func TestSetModelBeforeLogExists(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetModel("draft", "p", "m"); err != nil {
		t.Fatal(err)
	}
	meta, err := store.Meta("draft")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Provider != "p" || meta.Model != "m" {
		t.Fatalf("draft selection was not stored: %+v", meta)
	}
}
