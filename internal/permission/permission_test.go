package permission

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/policy"
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

func TestDefaultPolicy(t *testing.T) {
	if got := Default().Evaluate("write", "a.txt"); got != Allow {
		t.Fatalf("want allow for write, got %s", got)
	}
	if got := Default().Evaluate("bash", "ls"); got != Allow {
		t.Fatalf("want allow for bash, got %s", got)
	}
	if got := Default().Evaluate("read", "a.txt"); got != Allow {
		t.Fatalf("want allow for read, got %s", got)
	}
	if got := Default().Evaluate("read", "/work/.env"); got != Ask {
		t.Fatalf("want ask for .env, got %s", got)
	}
	if got := Default().Evaluate("read", "/work/.env.local"); got != Ask {
		t.Fatalf("want ask for .env.local, got %s", got)
	}
	if got := Default().Evaluate("read", "/work/.env.example"); got != Allow {
		t.Fatalf("want allow for .env.example, got %s", got)
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

// A rejection with a message is guidance, so the turn continues and the model
// reads the text as the tool result.
func TestRejectWithFeedbackReturnsCorrectedError(t *testing.T) {
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
	var corrected *CorrectedError
	if !errors.As(err, &corrected) {
		t.Fatalf("want CorrectedError, got %v", err)
	}
	if corrected.Feedback != "too dangerous" {
		t.Fatalf("feedback lost: %q", corrected.Feedback)
	}
}

// A rejection with no message is a plain stop, so the turn ends.
func TestRejectWithoutFeedbackReturnsDeclinedError(t *testing.T) {
	m := New(Rules{{Action: "bash", Resource: "*", Effect: Ask}})
	asked := make(chan Request, 1)
	done := make(chan error, 1)
	go func() {
		done <- m.Check(context.Background(), Request{Action: "bash", Resources: []string{"rm -rf /"}}, func(r Request) { asked <- r })
	}()
	req := <-asked
	if err := m.Reply(req.ID, Reject, ""); err != nil {
		t.Fatal(err)
	}
	err := <-done
	var declined *DeclinedError
	if !errors.As(err, &declined) {
		t.Fatalf("want DeclinedError, got %v", err)
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

func TestAlwaysPersistsLearnedRulesOnly(t *testing.T) {
	m := New(Rules{
		{Action: "*", Resource: "*", Effect: Allow},
		{Action: "bash", Resource: "*", Effect: Ask},
	})
	var saved Rules
	m.SetSaver(func(rules Rules) error {
		saved = append(Rules(nil), rules...)
		return nil
	})
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
	// Only the learned allow is persisted; configured rules stay untouched so a
	// saved approval can never be confused with a deliberate setting.
	if len(saved) != 1 {
		t.Fatalf("saver should receive only the learned rules, got %+v", saved)
	}
	if last := saved[0]; last.Action != "bash" || last.Resource != "git status" || last.Effect != Allow {
		t.Fatalf("unexpected saved rule: %+v", last)
	}
	if got := len(m.Rules()); got != 3 {
		t.Fatalf("configured rules must stay in place, got %d rules", got)
	}
}

func TestSavedAllowCannotLiftConfiguredDeny(t *testing.T) {
	m := New(Rules{
		{Action: "*", Resource: "*", Effect: Allow},
		{Action: "shell", Resource: "*", Effect: Deny},
	})
	m.SetSaved(Rules{{Action: "shell", Resource: "*", Effect: Allow}})
	err := m.Check(context.Background(), Request{Action: "shell", Resources: []string{"ls"}}, nil)
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("a saved allow must not lift a configured deny, got %v", err)
	}
}

func TestSavedAllowAnswersAsk(t *testing.T) {
	m := New(Rules{
		{Action: "*", Resource: "*", Effect: Allow},
		{Action: "shell", Resource: "*", Effect: Ask},
	})
	m.SetSaved(Rules{{Action: "shell", Resource: "git status", Effect: Allow}})
	if err := m.Check(context.Background(), Request{Action: "shell", Resources: []string{"git status"}}, nil); err != nil {
		t.Fatalf("a saved allow should answer the ask: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- m.Check(ctx, Request{Action: "shell", Resources: []string{"rm -rf /"}}, nil)
	}()
	// The unmatched command still asks, so cancel proves it reached the ask path
	// instead of returning a decision.
	for len(m.Pending("")) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("an unrelated command must still ask, got %v", err)
	}
}

func TestUnattendedAskIsDenied(t *testing.T) {
	m := New(Rules{{Action: "shell", Resource: "*", Effect: Ask}})
	announced := false
	err := m.Check(context.Background(), Request{Action: "shell", Resources: []string{"ls"}, Unattended: true}, func(Request) {
		announced = true
	})
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("an unattended ask must be denied, got %v", err)
	}
	if announced {
		t.Fatal("an unattended ask must not reach a client")
	}
	if !strings.Contains(err.Error(), "nobody is present") {
		t.Fatalf("reason should explain the denial, got %v", err)
	}
	if len(m.Pending("")) != 0 {
		t.Fatal("an unattended ask must not stay pending")
	}
}

func TestAlwaysSurvivesSaveFailure(t *testing.T) {
	m := New(Rules{{Action: "bash", Resource: "*", Effect: Ask}})
	m.SetSaver(func(Rules) error { return errors.New("disk full") })
	asked := make(chan Request, 1)
	done := make(chan error, 1)
	go func() {
		done <- m.Check(context.Background(), Request{Action: "bash", Resources: []string{"ls"}}, func(r Request) { asked <- r })
	}()
	req := <-asked
	if err := m.Reply(req.ID, Always, ""); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("a failed write must not reject the approved tool: %v", err)
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

func TestPoliciesBlockTools(t *testing.T) {
	manager := New(Rules{{Action: "*", Resource: "*", Effect: Allow}})
	manager.SetPolicies(policy.Normalize([]policy.Statement{
		{Action: "tool.use", Resource: "shell:git push *", Effect: policy.Deny},
	}))
	if err := manager.Check(context.Background(), Request{Action: "shell", Resources: []string{"git push origin"}}, nil); err == nil || !strings.Contains(err.Error(), "configuration policy") {
		t.Fatalf("policy should block shell push, got %v", err)
	}
	if err := manager.Check(context.Background(), Request{Action: "shell", Resources: []string{"git status"}}, nil); err != nil {
		t.Fatalf("unrelated shell command should pass: %v", err)
	}
}
