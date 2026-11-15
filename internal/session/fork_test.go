package session

import (
	"os"
	"strings"
	"testing"

	"lingzhu/internal/event"
)

func TestForkCopiesSessionAndMetadata(t *testing.T) {
	store := openStore(t, t.TempDir())
	for _, ev := range []event.Event{
		event.User("hello"),
		event.Assistant("hi"),
		event.TurnStartedEvent("turn_1"),
		event.TurnFinishedEvent("turn_1"),
		event.User("second"),
		event.Assistant("answer two"),
	} {
		if _, err := store.Append("s1", ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetTitle("s1", "Original"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetModel("s1", "work", "fast"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAgent("s1", "plan"); err != nil {
		t.Fatal(err)
	}

	info, err := store.Fork("s1", ForkOptions{ID: "fork_1"})
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "fork_1" || info.Title != "Original (fork)" || info.Provider != "work" || info.Model != "fast" || info.Agent != "plan" {
		t.Fatalf("unexpected fork info: %+v", info)
	}

	source, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	copied, err := store.Messages("fork_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(source) != 4 || len(copied) != 4 {
		t.Fatalf("unexpected message counts: source=%d fork=%d", len(source), len(copied))
	}
	for i := range source {
		if source[i].Content != copied[i].Content || source[i].Role != copied[i].Role {
			t.Fatalf("message %d differs: %+v != %+v", i, source[i], copied[i])
		}
	}

	// The source is untouched and both sessions are independent.
	if _, err := store.Append("fork_1", event.User("only in fork")); err != nil {
		t.Fatal(err)
	}
	source, _ = store.Messages("s1")
	if len(source) != 4 {
		t.Fatalf("fork append leaked into the source: %+v", source)
	}
	if exists, _ := store.HasEvents("s1"); !exists {
		t.Fatal("source session disappeared")
	}
}

func TestForkBeforeSeqAndOverrides(t *testing.T) {
	store := openStore(t, t.TempDir())
	for _, ev := range []event.Event{
		event.User("one"),
		event.Assistant("two"),
		event.User("three"),
		event.Assistant("four"),
	} {
		if _, err := store.Append("s1", ev); err != nil {
			t.Fatal(err)
		}
	}
	info, err := store.Fork("s1", ForkOptions{ID: "fork_2", Title: "Custom", BeforeSeq: 3})
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "Custom" {
		t.Fatalf("title override missing: %+v", info)
	}
	messages, err := store.Messages("fork_2")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Content != "one" || messages[1].Content != "two" {
		t.Fatalf("unexpected truncated fork: %+v", messages)
	}

	if _, err := store.Fork("s1", ForkOptions{ID: "fork_3", BeforeSeq: 1}); err == nil {
		t.Fatal("fork before the first event should fail")
	}
	if _, err := store.Fork("missing", ForkOptions{ID: "fork_4"}); !os.IsNotExist(err) {
		t.Fatalf("missing source should report not found: %v", err)
	}
	if _, err := store.Fork("s1", ForkOptions{ID: "fork_2"}); err == nil {
		t.Fatal("duplicate fork id should fail")
	}
	if _, err := store.Fork("s1", ForkOptions{ID: "bad id"}); err == nil || !strings.Contains(err.Error(), "invalid session id") {
		t.Fatalf("invalid id should fail: %v", err)
	}
	if _, err := store.Fork("s1", ForkOptions{ID: "s1"}); err == nil {
		t.Fatal("forking into the same id should fail")
	}
}

func TestForkDropsOpenTurn(t *testing.T) {
	store := openStore(t, t.TempDir())
	for _, ev := range []event.Event{
		event.User("settled"),
		event.TurnStartedEvent("turn_1"),
		event.TurnFinishedEvent("turn_1"),
		event.User("in flight"),
		event.TurnSnapshotEvent(4, "deadbeef"),
		event.TurnStartedEvent("turn_2"),
		event.Assistant("partial"),
	} {
		if _, err := store.Append("s1", ev); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Fork("s1", ForkOptions{ID: "fork_open"}); err != nil {
		t.Fatal(err)
	}
	events, err := store.Events("fork_open")
	if err != nil {
		t.Fatal(err)
	}
	lastBoundary := -1
	for i, ev := range events {
		if ev.Type == event.TurnFinished {
			lastBoundary = i
		}
		if ev.Type == event.TurnSnapshot {
			t.Fatalf("fork kept a snapshot for the dropped turn: %+v", events)
		}
	}
	for _, ev := range events[lastBoundary+1:] {
		if ev.Type == event.TurnStarted {
			t.Fatalf("fork kept an open turn: %+v", events)
		}
	}
	messages, err := store.Messages("fork_open")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Content != "settled" || messages[1].Content != "in flight" {
		t.Fatalf("unexpected trimmed messages: %+v", messages)
	}
}
