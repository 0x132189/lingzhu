package permission

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEvaluatorDenyOverridesAllow(t *testing.T) {
	manager := New(Rules{{Action: "*", Resource: "*", Effect: Allow}})
	manager.SetEvaluator(func(_ context.Context, action string, _ []string) (Effect, string) {
		if action == "shell" {
			return Deny, "policy forbids shell"
		}
		return "", ""
	})
	err := manager.Check(context.Background(), Request{Action: "shell", Resources: []string{"ls"}}, nil)
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want denial, got %v", err)
	}
	if denied.Reason != "denied by plugin: policy forbids shell" {
		t.Fatalf("unexpected reason: %q", denied.Reason)
	}
	if err := manager.Check(context.Background(), Request{Action: "glob", Resources: []string{"*"}}, nil); err != nil {
		t.Fatalf("deferred action should keep the rule allow: %v", err)
	}
}

func TestEvaluatorAllowOverridesAsk(t *testing.T) {
	manager := New(Rules{{Action: "*", Resource: "*", Effect: Ask}})
	manager.SetEvaluator(func(_ context.Context, _ string, _ []string) (Effect, string) { return Allow, "" })
	announced := false
	if err := manager.Check(context.Background(), Request{Action: "read", Resources: []string{"a.txt"}}, func(Request) {
		announced = true
	}); err != nil {
		t.Fatal(err)
	}
	if announced {
		t.Fatal("evaluator allow should skip the prompt")
	}
}

func TestEvaluatorAskPrompts(t *testing.T) {
	manager := New(Rules{{Action: "*", Resource: "*", Effect: Allow}})
	manager.SetEvaluator(func(_ context.Context, _ string, _ []string) (Effect, string) { return Ask, "confirm with plugin" })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	asked := make(chan Request, 1)
	err := manager.Check(ctx, Request{Action: "read", Resources: []string{"a.txt"}}, func(req Request) {
		asked <- req
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the prompt to wait for an answer, got %v", err)
	}
	select {
	case req := <-asked:
		if req.Message != "confirm with plugin" {
			t.Fatalf("unexpected ask message: %q", req.Message)
		}
	default:
		t.Fatal("evaluator ask should announce a request")
	}
}

func TestConfiguredDenyBeatsEvaluator(t *testing.T) {
	manager := New(Rules{{Action: "shell", Resource: "*", Effect: Deny}})
	called := false
	manager.SetEvaluator(func(_ context.Context, _ string, _ []string) (Effect, string) {
		called = true
		return Allow, ""
	})
	err := manager.Check(context.Background(), Request{Action: "shell", Resources: []string{"ls"}}, nil)
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want configured denial, got %v", err)
	}
	if called {
		t.Fatal("a configured deny should not consult the evaluator")
	}
}

func TestSessionRulesApplyToOnlyOneSession(t *testing.T) {
	manager := New(Rules{{Action: "*", Resource: "*", Effect: Allow}})
	manager.SetSessionRules("plan-session", Rules{{Action: "edit", Resource: "*", Effect: Deny}})
	err := manager.Check(context.Background(), Request{SessionID: "plan-session", Action: "edit", Resources: []string{"a.txt"}}, nil)
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want session denial, got %v", err)
	}
	if err := manager.Check(context.Background(), Request{SessionID: "build-session", Action: "edit", Resources: []string{"a.txt"}}, nil); err != nil {
		t.Fatalf("other session should stay allowed: %v", err)
	}
	manager.ClearSessionRules("plan-session")
	if err := manager.Check(context.Background(), Request{SessionID: "plan-session", Action: "edit", Resources: []string{"a.txt"}}, nil); err != nil {
		t.Fatalf("cleared rules should be gone: %v", err)
	}
}
