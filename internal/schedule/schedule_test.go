package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"lingzhu/internal/permission"
	"lingzhu/internal/tool"
)

type testClock struct {
	at time.Time
}

type fired struct {
	mu    sync.Mutex
	items []Item
}

func (f *fired) record(item Item) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, item)
	return nil
}

func (f *fired) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items)
}

func newManager(t *testing.T, clock *testClock) (*Manager, *fired) {
	t.Helper()
	manager, err := New(t.TempDir(), Config{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return clock.at }
	f := &fired{}
	manager.SetFire(f.record)
	return manager, f
}

func baseTime() time.Time {
	return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
}

func TestCreateListAndPersist(t *testing.T) {
	clock := &testClock{at: baseTime()}
	manager, _ := newManager(t, clock)
	item, err := manager.Create("s1", CreateRequest{Name: "ping", Prompt: "check tests", When: When{Type: "in", In: "5m"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(item.ID, "sch_") || item.Status != StatusActive || item.RunCount != 0 {
		t.Fatalf("unexpected item: %+v", item)
	}
	if !item.NextDueAt.Equal(clock.at.Add(5 * time.Minute)) {
		t.Fatalf("unexpected next due: %v", item.NextDueAt)
	}
	if got := manager.List("s1"); len(got) != 1 || got[0].ID != item.ID {
		t.Fatalf("unexpected list: %+v", got)
	}
	if got := manager.List("s2"); len(got) != 0 {
		t.Fatalf("other session should see nothing: %+v", got)
	}
	if got := manager.All(); len(got) != 1 {
		t.Fatalf("unexpected all: %+v", got)
	}

	reopened, err := New(dirOf(manager.file), Config{}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	items := reopened.All()
	if len(items) != 1 || items[0].Prompt != "check tests" || items[0].Name != "ping" {
		t.Fatalf("schedule did not persist: %+v", items)
	}
}

func dirOf(file string) string {
	index := strings.LastIndexAny(file, `/\`)
	if index < 0 {
		return "."
	}
	return file[:index]
}

func TestOneShotFiresAndBecomesDone(t *testing.T) {
	clock := &testClock{at: baseTime()}
	manager, recorder := newManager(t, clock)
	item, err := manager.Create("s1", CreateRequest{Prompt: "wake up", When: When{Type: "in", In: "1s"}})
	if err != nil {
		t.Fatal(err)
	}

	manager.Tick()
	if recorder.count() != 0 {
		t.Fatalf("schedule fired too early")
	}

	clock.at = clock.at.Add(time.Second)
	manager.Tick()
	if recorder.count() != 1 {
		t.Fatalf("schedule did not fire: %d", recorder.count())
	}
	stored, _ := manager.Get(item.ID, "s1")
	if stored.Status != StatusDone || stored.RunCount != 1 || !stored.NextDueAt.IsZero() || stored.LastFiredAt.IsZero() {
		t.Fatalf("unexpected fired item: %+v", stored)
	}

	manager.Tick()
	if recorder.count() != 1 {
		t.Fatalf("one-shot fired twice")
	}
}

func TestEveryRepeatsOnCadence(t *testing.T) {
	clock := &testClock{at: baseTime()}
	manager, recorder := newManager(t, clock)
	item, err := manager.Create("s1", CreateRequest{Prompt: "tick", When: When{Type: "every", Every: "1s"}})
	if err != nil {
		t.Fatal(err)
	}

	clock.at = clock.at.Add(time.Second)
	manager.Tick()
	stored, _ := manager.Get(item.ID, "s1")
	if stored.RunCount != 1 || stored.Status != StatusActive {
		t.Fatalf("unexpected first run: %+v", stored)
	}
	if !stored.NextDueAt.Equal(clock.at.Add(time.Second)) {
		t.Fatalf("cadence drifted: %v want %v", stored.NextDueAt, clock.at.Add(time.Second))
	}

	clock.at = clock.at.Add(time.Second)
	manager.Tick()
	stored, _ = manager.Get(item.ID, "s1")
	if stored.RunCount != 2 || recorder.count() != 2 {
		t.Fatalf("second run missing: %+v fired=%d", stored, recorder.count())
	}
}

func TestPauseResume(t *testing.T) {
	clock := &testClock{at: baseTime()}
	manager, recorder := newManager(t, clock)
	item, err := manager.Create("s1", CreateRequest{Prompt: "once", When: When{Type: "in", In: "1s"}})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := manager.Pause(item.ID, "s1")
	if err != nil || paused.Status != StatusPaused {
		t.Fatalf("pause failed: %+v %v", paused, err)
	}
	clock.at = clock.at.Add(2 * time.Second)
	manager.Tick()
	if recorder.count() != 0 {
		t.Fatal("paused schedule fired")
	}

	resumed, err := manager.Resume(item.ID, "s1")
	if err != nil || resumed.Status != StatusActive {
		t.Fatalf("resume failed: %+v %v", resumed, err)
	}
	manager.Tick()
	if recorder.count() != 1 {
		t.Fatalf("resumed one-shot should fire immediately: %d", recorder.count())
	}
	if _, err := manager.Resume(item.ID, "s1"); err == nil {
		t.Fatal("resuming a done schedule should fail")
	}
}

func TestUpdateDeleteAndOwnership(t *testing.T) {
	clock := &testClock{at: baseTime()}
	manager, _ := newManager(t, clock)
	item, err := manager.Create("s1", CreateRequest{Prompt: "old", When: When{Type: "in", In: "1m"}})
	if err != nil {
		t.Fatal(err)
	}
	title := "renamed"
	prompt := "new prompt"
	when := When{Type: "every", Every: "2m"}
	updated, err := manager.Update(item.ID, "s1", UpdateRequest{Name: &title, Prompt: &prompt, When: &when})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "renamed" || updated.Prompt != "new prompt" || updated.When.Type != "every" {
		t.Fatalf("update not applied: %+v", updated)
	}
	if !updated.NextDueAt.Equal(clock.at.Add(2 * time.Minute)) {
		t.Fatalf("unexpected reschedule: %v", updated.NextDueAt)
	}

	if _, err := manager.Pause(item.ID, "s2"); err == nil {
		t.Fatal("other session should not pause the schedule")
	}
	if _, err := manager.Update(item.ID, "s2", UpdateRequest{Prompt: &prompt}); err == nil {
		t.Fatal("other session should not update the schedule")
	}
	if err := manager.Delete(item.ID, "s2"); err == nil {
		t.Fatal("other session should not delete the schedule")
	}
	if err := manager.Delete(item.ID, "s1"); err != nil {
		t.Fatal(err)
	}
	if got := manager.List("s1"); len(got) != 0 {
		t.Fatalf("schedule not deleted: %+v", got)
	}
	if err := manager.Delete(item.ID, "s1"); err == nil {
		t.Fatal("second delete should fail")
	}
}

func TestDeliveryFailureKeepsScheduleActive(t *testing.T) {
	clock := &testClock{at: baseTime()}
	manager, _ := newManager(t, clock)
	manager.SetFire(func(Item) error { return errors.New("session unavailable") })
	item, err := manager.Create("s1", CreateRequest{Prompt: "retry", When: When{Type: "in", In: "1s"}})
	if err != nil {
		t.Fatal(err)
	}
	clock.at = clock.at.Add(time.Second)
	manager.Tick()
	stored, _ := manager.Get(item.ID, "s1")
	if stored.Status != StatusActive || !strings.Contains(stored.LastError, "session unavailable") {
		t.Fatalf("unexpected failure state: %+v", stored)
	}
	if !stored.NextDueAt.Equal(clock.at.Add(retryDelay)) {
		t.Fatalf("retry not scheduled: %v", stored.NextDueAt)
	}
	if stored.RunCount != 0 {
		t.Fatalf("failed delivery counted a run: %+v", stored)
	}
}

func TestWhenValidation(t *testing.T) {
	clock := &testClock{at: baseTime()}
	manager, _ := newManager(t, clock)
	if _, err := manager.Create("s1", CreateRequest{Prompt: "x", When: When{Type: "in", In: "5 minutes"}}); err != nil {
		t.Fatalf("word duration should work: %v", err)
	}
	if _, err := manager.Create("s1", CreateRequest{Prompt: "x", When: When{Type: "every", Every: "2h"}}); err != nil {
		t.Fatalf("hour interval should work: %v", err)
	}
	if _, err := manager.Create("s1", CreateRequest{Prompt: "x", When: When{Type: "at", At: "2026-11-06T13:30:00Z"}}); err != nil {
		t.Fatalf("at time should work: %v", err)
	}
	if _, err := manager.Create("s1", CreateRequest{Prompt: "x", When: When{Type: "at", At: "2020-01-01T00:00:00Z"}}); err == nil {
		t.Fatal("past time should be rejected")
	}
	if _, err := manager.Create("s1", CreateRequest{Prompt: "x", When: When{Type: "cron", At: "* * * * *"}}); err == nil {
		t.Fatal("cron should be rejected with a clear error")
	}
	if _, err := manager.Create("s1", CreateRequest{Prompt: "x", When: When{Type: "every", Every: "0s"}}); err == nil {
		t.Fatal("zero interval should be rejected")
	}
	if _, err := manager.Create("s1", CreateRequest{Prompt: " ", When: When{Type: "in", In: "1s"}}); err == nil {
		t.Fatal("empty prompt should be rejected")
	}
	if _, err := manager.Create("", CreateRequest{Prompt: "x", When: When{Type: "in", In: "1s"}}); err == nil {
		t.Fatal("missing session should be rejected")
	}
}

func TestDisabledManager(t *testing.T) {
	off := false
	if manager, err := New(t.TempDir(), Config{Enabled: &off}, t.Logf); err != nil || manager != nil {
		t.Fatalf("disabled config should yield nil manager: %v %v", manager, err)
	}
	var manager *Manager
	if _, err := manager.Create("s1", CreateRequest{Prompt: "x"}); err == nil {
		t.Fatal("nil manager should reject create")
	}
}

func TestTools(t *testing.T) {
	clock := &testClock{at: baseTime()}
	manager, recorder := newManager(t, clock)
	set := map[string]tool.Tool{}
	for _, item := range Tools(manager) {
		set[item.Name()] = item
	}
	allow := permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})

	out, err := set["lingzhu_schedule_create"].Run(context.Background(), tool.Call{
		Name:      "lingzhu_schedule_create",
		SessionID: "s1",
		Input:     json.RawMessage(`{"name":"wake","prompt":"check","when":{"type":"in","in":"1s"}}`),
		Perm:      allow,
	})
	if err != nil || !strings.Contains(out, "Scheduled: sch_") || !strings.Contains(out, "wake") {
		t.Fatalf("unexpected create: %q %v", out, err)
	}
	id := manager.All()[0].ID

	out, err = set["lingzhu_schedule_list"].Run(context.Background(), tool.Call{
		Name: "lingzhu_schedule_list", SessionID: "s1", Input: json.RawMessage(`{}`), Perm: allow,
	})
	if err != nil || !strings.Contains(out, "Now: ") || !strings.Contains(out, id) {
		t.Fatalf("unexpected list: %q %v", out, err)
	}

	out, err = set["lingzhu_schedule_pause"].Run(context.Background(), tool.Call{
		Name: "lingzhu_schedule_pause", SessionID: "s1", Input: json.RawMessage(`{"id":"` + id + `"}`), Perm: allow,
	})
	if err != nil || !strings.Contains(out, "Paused: ") || !strings.Contains(out, StatusPaused) {
		t.Fatalf("unexpected pause: %q %v", out, err)
	}

	out, err = set["lingzhu_schedule_resume"].Run(context.Background(), tool.Call{
		Name: "lingzhu_schedule_resume", SessionID: "s1", Input: json.RawMessage(`{"id":"` + id + `"}`), Perm: allow,
	})
	if err != nil || !strings.Contains(out, "Resumed: ") {
		t.Fatalf("unexpected resume: %q %v", out, err)
	}

	deny := permission.New(permission.Rules{{Action: "lingzhu_schedule_delete", Resource: "*", Effect: permission.Deny}})
	if _, err := set["lingzhu_schedule_delete"].Run(context.Background(), tool.Call{
		Name: "lingzhu_schedule_delete", SessionID: "s1", Input: json.RawMessage(`{"id":"` + id + `"}`), Perm: deny,
	}); err == nil {
		t.Fatal("denied delete should fail")
	}

	out, err = set["lingzhu_schedule_delete"].Run(context.Background(), tool.Call{
		Name: "lingzhu_schedule_delete", SessionID: "s1", Input: json.RawMessage(`{"id":"` + id + `"}`), Perm: allow,
	})
	if err != nil || !strings.Contains(out, "Deleted schedule") {
		t.Fatalf("unexpected delete: %q %v", out, err)
	}

	// The fake clock never advanced, so nothing fired during the tool round trip.
	if recorder.count() != 0 {
		t.Fatalf("unexpected fires: %d", recorder.count())
	}
}
