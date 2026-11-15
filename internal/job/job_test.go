package job

import (
	"strings"
	"testing"
	"time"
)

func TestFinishNotifiesOnce(t *testing.T) {
	manager := New()
	done := make(chan Job, 2)
	manager.SetNotifier(func(item Job) { done <- item })

	item := manager.Create("s1", "echo hi", "out.log")
	if item.Status != Running || item.ExitCode != -1 {
		t.Fatalf("unexpected new job: %+v", item)
	}
	manager.Finish(item.ID, Completed, 0, "", "hi\n")

	select {
	case got := <-done:
		if got.Status != Completed || got.ExitCode != 0 || got.Tail != "hi\n" {
			t.Fatalf("unexpected notification: %+v", got)
		}
		if !got.Finished.After(got.Started.Add(-time.Second)) {
			t.Fatalf("finished time missing: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("notifier did not run")
	}

	manager.Finish(item.ID, Failed, 1, "late", "")
	select {
	case extra := <-done:
		t.Fatalf("duplicate notification: %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestListFiltersBySession(t *testing.T) {
	manager := New()
	a := manager.Create("s1", "a", "a.log")
	b := manager.Create("s2", "b", "b.log")

	items := manager.List("s1")
	if len(items) != 1 || items[0].ID != a.ID {
		t.Fatalf("unexpected session listing: %+v", items)
	}
	if all := manager.List(""); len(all) != 2 {
		t.Fatalf("unexpected full listing: %+v", all)
	}
	if _, ok := manager.Get(b.ID); !ok {
		t.Fatal("get missing job")
	}
}

func TestNotificationText(t *testing.T) {
	item := Job{ID: "job_1", Command: "make", File: "out.log", Status: Completed, ExitCode: 0, Tail: "ok"}
	text := item.Notification()
	for _, want := range []string{"[Background shell completed]", "job_1", "make", "exit 0", "out.log", "ok"} {
		if !strings.Contains(text, want) {
			t.Fatalf("notification missing %q: %s", want, text)
		}
	}
	failed := Job{ID: "job_2", Command: "bad", File: "err.log", Status: Failed, ExitCode: 2, Error: "boom"}
	if !strings.Contains(failed.Notification(), "boom") {
		t.Fatalf("failure detail missing: %s", failed.Notification())
	}
}
