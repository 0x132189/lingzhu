package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestMigratesLegacyLog(t *testing.T) {
	dir := t.TempDir()
	legacyDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLegacy(t, legacyDir, "s1", "Old title", []event.Event{
		event.User("legacy hello"),
		event.Assistant("legacy reply"),
	})
	writeLegacy(t, legacyDir, "s2", "", []event.Event{
		event.User("first words become the title"),
		event.Assistant("ok"),
	})
	draftMeta, err := json.Marshal(Meta{Provider: "draft-provider", Model: "draft-model"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "draft.meta.json"), draftMeta, 0o644); err != nil {
		t.Fatal(err)
	}

	store := openStore(t, dir)

	draft, err := store.Meta("draft")
	if err != nil {
		t.Fatal(err)
	}
	if draft.Provider != "draft-provider" || draft.Model != "draft-model" {
		t.Fatalf("draft metadata was not migrated: %+v", draft)
	}
	infos, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("draft without events should not be listed: %+v", infos)
	}

	info, err := store.Info("s1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "Old title" || info.Provider != "test-provider" || info.Model != "test-model" {
		t.Fatalf("metadata was not migrated: %+v", info)
	}
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("legacy events were not assigned sequences: %+v", events)
	}
	migrated, err := store.Info("s2")
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Title != "first words become the title" {
		t.Fatalf("missing title was not derived: %+v", migrated)
	}
	if _, err := os.Stat(filepath.Join(legacyDir, "s1.jsonl")); err != nil {
		t.Fatalf("legacy log should stay as a backup: %v", err)
	}

	// Reopening must not import the same files a second time.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, dir)
	events, err = reopened.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("migration ran twice: %d events", len(events))
	}
}

func TestMigrationToleratesTornTail(t *testing.T) {
	dir := t.TempDir()
	legacyDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	if err := enc.Encode(event.User("complete")); err != nil {
		t.Fatal(err)
	}
	b.WriteString(`{"type":"assistant.message"`)
	if err := os.WriteFile(filepath.Join(legacyDir, "s1.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openStore(t, dir)
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("torn tail should be dropped: %+v", events)
	}
}

func TestMigrationSkipsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	legacyDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "bad.jsonl"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeLegacy(t, legacyDir, "good", "", []event.Event{event.User("survives")})

	store := openStore(t, dir)
	if _, err := store.Info("bad"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt log should be skipped, got %v", err)
	}
	if _, err := store.Info("good"); err != nil {
		t.Fatalf("a corrupt file must not block others: %v", err)
	}
}

func writeLegacy(t *testing.T, dir, id, title string, events []event.Event) {
	t.Helper()
	var b strings.Builder
	enc := json.NewEncoder(&b)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := Meta{Title: title, Provider: "test-provider", Model: "test-model"}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".meta.json"), data, 0o644); err != nil {
		t.Fatal(err)
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
