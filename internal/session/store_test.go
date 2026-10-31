package session

import (
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
		if err := store.Append("s1", ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Append("s1", event.Delta("ignored")); err != nil {
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
	if err := store.Append("../escape", event.User("x")); err == nil {
		t.Fatal("want error for path traversal id")
	}
}

func TestList(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"b", "a"} {
		if err := store.Append(id, event.User("x")); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("want sorted [a b], got %v", ids)
	}
	if _, err := store.Events("missing"); err != nil {
		t.Fatalf("missing session should read as empty: %v", err)
	}
}
