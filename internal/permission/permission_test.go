package permission

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEvaluateLastMatchWins(t *testing.T) {
	rules := Rules{
		{Action: "*", Resource: "*", Effect: Ask},
		{Action: "read", Resource: "*", Effect: Allow},
		{Action: "read", Resource: "*.env", Effect: Deny},
	}
	cases := []struct {
		action, resource string
		want             Effect
	}{
		{"read", "src/a.go", Allow},
		{"read", ".env", Deny},
		{"bash", "ls", Ask},
		{"write", "a.txt", Ask},
	}
	for _, tc := range cases {
		if got := rules.Evaluate(tc.action, tc.resource); got != tc.want {
			t.Fatalf("%s %s: want %s, got %s", tc.action, tc.resource, tc.want, got)
		}
	}
}

func TestEvaluateAllDenyWins(t *testing.T) {
	rules := Rules{
		{Action: "*", Resource: "*", Effect: Allow},
		{Action: "bash", Resource: "rm *", Effect: Deny},
	}
	if got := rules.EvaluateAll("bash", []string{"ls", "rm -rf /"}); got != Deny {
		t.Fatalf("want deny, got %s", got)
	}
	if got := rules.EvaluateAll("bash", []string{"ls", "pwd"}); got != Allow {
		t.Fatalf("want allow, got %s", got)
	}
}

func TestDefaultAsksUnknownActions(t *testing.T) {
	if got := Default().Evaluate("write", "a.txt"); got != Ask {
		t.Fatalf("want ask, got %s", got)
	}
	if got := Default().Evaluate("read", "a.txt"); got != Allow {
		t.Fatalf("want allow, got %s", got)
	}
}

func TestCheckWaitsForReply(t *testing.T) {
	m := New(Rules{{Action: "bash", Resource: "*", Effect: Ask}})
	asked := make(chan Request, 1)
	done := make(chan error, 1)

	go func() {
		done <- m.Check(context.Background(), Request{SessionID: "s1", Action: "bash", Resources: []string{"ls"}}, func(r Request) {
			asked <- r
		})
	}()

	var req Request
	select {
	case req = <-asked:
	case <-time.After(time.Second):
		t.Fatal("no permission event")
	}
	if len(m.Pending("s1")) != 1 {
		t.Fatalf("want 1 pending, got %d", len(m.Pending("s1")))
	}
	if err := m.Reply(req.ID, Once, ""); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("want allowed, got %v", err)
	}
	if len(m.Pending("")) != 0 {
		t.Fatal("pending should be cleared")
	}
}

func TestRejectReturnsDeniedError(t *testing.T) {
	m := New(Rules{{Action: "bash", Resource: "*", Effect: Ask}})
	asked := make(chan Request, 1)
	done := make(chan error, 1)
	go func() {
		done <- m.Check(context.Background(), Request{Action: "bash", Resources: []string{"rm -rf /"}}, func(r Request) { asked <- r })
	}()
	req := <-asked
	if err := m.Reply(req.ID, Reject, "too dangerous"); err != nil {
		t.Fatal(err)
	}
	err := <-done
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want DeniedError, got %v", err)
	}
	if denied.Reason != "too dangerous" {
		t.Fatalf("feedback lost: %q", denied.Reason)
	}
}

func TestAlwaysSavesRule(t *testing.T) {
	m := New(Rules{{Action: "bash", Resource: "*", Effect: Ask}})
	asked := make(chan Request, 1)
	done := make(chan error, 1)
	go func() {
		done <- m.Check(context.Background(), Request{Action: "bash", Resources: []string{"git status"}}, func(r Request) { asked <- r })
	}()
	req := <-asked
	if err := m.Reply(req.ID, Always, ""); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("want allowed, got %v", err)
	}
	if got := m.Rules().Evaluate("bash", "git status"); got != Allow {
		t.Fatalf("always should save an allow rule, got %s", got)
	}
}

func TestCancelUnblocks(t *testing.T) {
	m := New(Rules{{Action: "bash", Resource: "*", Effect: Ask}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- m.Check(ctx, Request{Action: "bash", Resources: []string{"ls"}}, func(Request) { cancel() })
	}()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
