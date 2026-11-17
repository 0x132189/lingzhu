package session

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
)

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestAppendAndRebuild(t *testing.T) {
	store := openStore(t, t.TempDir())
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
	store := openStore(t, t.TempDir())
	if _, err := store.Append("../escape", event.User("x")); err == nil {
		t.Fatal("want error for path traversal id")
	}
}

func TestList(t *testing.T) {
	store := openStore(t, t.TempDir())
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
		t.Fatalf("title should be stored from the first user message, got %q", infos[0].Title)
	}
	if _, err := store.Events("missing"); err != nil {
		t.Fatalf("missing session should read as empty: %v", err)
	}
}

func TestListEmptyIsAnEmptySlice(t *testing.T) {
	store := openStore(t, t.TempDir())
	infos, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if infos == nil {
		t.Fatal("want an empty slice so JSON encodes as []")
	}
	if len(infos) != 0 {
		t.Fatalf("new store should be empty: %+v", infos)
	}
}

func TestAppendAssignsSequence(t *testing.T) {
	store := openStore(t, t.TempDir())
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
	store := openStore(t, dir)
	if _, err := store.Append("s1", event.User("a")); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, dir)
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

// TestAppendHandlesLargeEvent guards against a single write_file tool call
// carrying a payload larger than the old JSONL scanner allowed.
func TestAppendHandlesLargeEvent(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, dir)
	big := strings.Repeat("x", 9*1024*1024)
	if _, err := store.Append("s1", event.ToolResultEvent("call_1", "write_file", big, false)); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, dir)
	seq, err := reopened.Append("s1", event.User("after"))
	if err != nil {
		t.Fatal(err)
	}
	if seq != 2 {
		t.Fatalf("want seq 2, got %d", seq)
	}
	events, err := reopened.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Seq != 2 {
		t.Fatalf("unexpected events after large payload: %d", len(events))
	}
}

func TestConcurrentAppend(t *testing.T) {
	store := openStore(t, t.TempDir())
	const count = 32
	seqs := make(chan int64, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			seq, err := store.Append("s1", event.Assistant("reply"))
			if err != nil {
				t.Errorf("append %d: %v", n, err)
				return
			}
			seqs <- seq
		}(i)
	}
	wg.Wait()
	close(seqs)

	seen := map[int64]bool{}
	for seq := range seqs {
		if seq < 1 || seq > count {
			t.Fatalf("sequence out of range: %d", seq)
		}
		if seen[seq] {
			t.Fatalf("duplicate sequence: %d", seq)
		}
		seen[seq] = true
	}
	if len(seen) != count {
		t.Fatalf("want %d sequences, got %d", count, len(seen))
	}

	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != count {
		t.Fatalf("want %d events, got %d", count, len(events))
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has sequence %d", i, ev.Seq)
		}
	}
}

func TestSetTitleAndDelete(t *testing.T) {
	store := openStore(t, t.TempDir())
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
	store := openStore(t, t.TempDir())
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
	store := openStore(t, t.TempDir())
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
	store := openStore(t, t.TempDir())
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
	store := openStore(t, t.TempDir())
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
	if _, err := store.Info("draft"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a draft without events should not be listed: %v", err)
	}
	infos, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 {
		t.Fatalf("a draft without events should not be listed: %+v", infos)
	}
}

func TestSetParentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, dir)
	if _, err := store.Append("parent", event.User("hi")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("child", event.User("go")); err != nil {
		t.Fatal(err)
	}
	if err := store.SetParent("child", "parent"); err != nil {
		t.Fatal(err)
	}
	meta, err := store.Meta("child")
	if err != nil || meta.Parent != "parent" {
		t.Fatalf("unexpected meta: %+v %v", meta, err)
	}
	info, err := store.Info("child")
	if err != nil || info.Parent != "parent" {
		t.Fatalf("unexpected info: %+v %v", info, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openStore(t, dir)
	meta, err = reopened.Meta("child")
	if err != nil || meta.Parent != "parent" {
		t.Fatalf("parent did not persist: %+v %v", meta, err)
	}
}

func TestSetAgentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, dir)
	if _, err := store.Append("s1", event.User("hi")); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAgent("s1", "plan"); err != nil {
		t.Fatal(err)
	}
	meta, err := store.Meta("s1")
	if err != nil || meta.Agent != "plan" {
		t.Fatalf("unexpected meta: %+v %v", meta, err)
	}
	info, err := store.Info("s1")
	if err != nil || info.Agent != "plan" {
		t.Fatalf("unexpected info: %+v %v", info, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openStore(t, dir)
	meta, err = reopened.Meta("s1")
	if err != nil || meta.Agent != "plan" {
		t.Fatalf("agent did not survive reopen: %+v %v", meta, err)
	}
}

func TestExportImportAndMove(t *testing.T) {
	source := openStore(t, t.TempDir())
	target := openStore(t, t.TempDir())
	events := []event.Event{event.User("hello"), event.Assistant("world")}
	for _, ev := range events {
		if _, err := source.Append("s1", ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.SetTitle("s1", "Moved session"); err != nil {
		t.Fatal(err)
	}
	if err := source.SetModel("s1", "work", "fast"); err != nil {
		t.Fatal(err)
	}
	if err := source.SetAgent("s1", "plan"); err != nil {
		t.Fatal(err)
	}

	bundle, err := source.Export("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Events) != 2 || bundle.Title != "Moved session" || bundle.Agent != "plan" {
		t.Fatalf("unexpected bundle: %+v", bundle)
	}
	if _, err := target.Import(bundle); err != nil {
		t.Fatal(err)
	}
	info, err := target.Info("s1")
	if err != nil || info.Title != "Moved session" {
		t.Fatalf("unexpected target info: %+v %v", info, err)
	}
	meta, _ := target.Meta("s1")
	if meta.Provider != "work" || meta.Model != "fast" || meta.Agent != "plan" {
		t.Fatalf("metadata not transferred: %+v", meta)
	}
	messages, err := target.Messages("s1")
	if err != nil || len(messages) != 2 || messages[0].Content != "hello" || messages[1].Content != "world" {
		t.Fatalf("history not transferred: %+v %v", messages, err)
	}
	if _, err := target.Import(bundle); err == nil {
		t.Fatal("duplicate import should fail")
	}

	if _, err := source.Append("s2", event.User("move me")); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Move("s2", target); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Info("s2"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("moved session should leave the source: %v", err)
	}
	messages, err = target.Messages("s2")
	if err != nil || len(messages) != 1 || messages[0].Content != "move me" {
		t.Fatalf("move lost history: %+v %v", messages, err)
	}
	if _, err := source.Move("s1", source); err == nil {
		t.Fatal("moving into the same store should fail")
	}
}

func TestInputDeliveryAndCancellation(t *testing.T) {
	store := openStore(t, t.TempDir())
	if _, err := store.Append("s1", event.User("start")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueueInputDelivery("s1", "inp_1", "first", "queue"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueueInput("s1", "inp_2", "second"); err != nil {
		t.Fatal(err)
	}
	recovery, err := store.Recovery("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Inputs) != 2 {
		t.Fatalf("unexpected inputs: %+v", recovery.Inputs)
	}
	if recovery.Inputs[0].Delivery != "queue" || recovery.Inputs[0].Created.IsZero() {
		t.Fatalf("unexpected first input: %+v", recovery.Inputs[0])
	}
	if recovery.Inputs[1].Delivery != "steer" {
		t.Fatalf("default delivery should be steer: %+v", recovery.Inputs[1])
	}

	if _, err := store.UpdateInputDelivery("s1", "inp_1", "steer"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CancelInput("s1", "inp_2"); err != nil {
		t.Fatal(err)
	}
	recovery, _ = store.Recovery("s1")
	if len(recovery.Inputs) != 1 || recovery.Inputs[0].ID != "inp_1" || recovery.Inputs[0].Delivery != "steer" {
		t.Fatalf("unexpected pending inputs: %+v", recovery.Inputs)
	}

	// Delivery still clears the item from the pending set.
	if _, err := store.Append("s1", event.UserInput("first", "inp_1")); err != nil {
		t.Fatal(err)
	}
	recovery, _ = store.Recovery("s1")
	if len(recovery.Inputs) != 0 {
		t.Fatalf("delivered input should be cleared: %+v", recovery.Inputs)
	}
}
func TestEventCacheStaysInSync(t *testing.T) {
	store := openStore(t, t.TempDir())
	if _, err := store.Append("s1", event.User("first")); err != nil {
		t.Fatal(err)
	}
	// Populate the in-memory cache.
	first, err := store.Events("s1")
	if err != nil || len(first) != 1 {
		t.Fatalf("first read: %v %d", err, len(first))
	}
	if _, err := store.Append("s1", event.Assistant("second")); err != nil {
		t.Fatal(err)
	}
	again, err := store.Events("s1")
	if err != nil || len(again) != 2 {
		t.Fatalf("cached read: %v %d", err, len(again))
	}
	if again[1].Seq <= again[0].Seq {
		t.Fatalf("sequence order lost: %d then %d", again[0].Seq, again[1].Seq)
	}
	after, err := store.EventsAfter("s1", again[0].Seq)
	if err != nil || len(after) != 1 || after[0].Seq != again[1].Seq {
		t.Fatalf("events after: %v %d", err, len(after))
	}
	if context, err := store.Context("s1"); err != nil || len(context.Messages) != 2 {
		t.Fatalf("context after cached appends: %v %d", err, len(context.Messages))
	}
	if err := store.Delete("s1"); err != nil {
		t.Fatal(err)
	}
	empty, err := store.Events("s1")
	if err != nil || len(empty) != 0 {
		t.Fatalf("delete did not clear the cache: %v %d", err, len(empty))
	}
}
