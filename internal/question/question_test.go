package question

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/permission"
	"lingzhu/internal/tool"
)

func allowAll() *permission.Manager {
	return permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})
}

func input(questions string) json.RawMessage {
	return json.RawMessage(`{"questions":` + questions + `}`)
}

func testCall(sessionID string, questions string) (tool.Call, *[]event.Event) {
	var mu sync.Mutex
	var emitted []event.Event
	return tool.Call{
		SessionID: sessionID,
		Name:      "question",
		Input:     input(questions),
		Perm:      allowAll(),
		Metadata:  map[string]any{},
		Emit: func(ev event.Event) error {
			mu.Lock()
			emitted = append(emitted, ev)
			mu.Unlock()
			return nil
		},
	}, &emitted
}

// answer waits for the pending question and replies with response.
func answer(t *testing.T, manager *Manager, sessionID string, response Response) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pending := manager.Pending(sessionID)
		if len(pending) > 0 {
			if _, err := manager.Reply(pending[0].ID, response); err != nil {
				t.Errorf("reply: %v", err)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("question never became pending")
}

func TestToolAsksAndFormatsAnswers(t *testing.T) {
	manager := New()
	call, emitted := testCall("s1", `[
		{"question":"Pick a color","header":"Color","options":[{"label":"Red","description":"warm"},{"label":"Blue","description":"cool"}]},
		{"question":"Pick sizes","header":"Size","options":[{"label":"S","description":"small"},{"label":"L","description":"large"}],"multiple":true}
	]`)
	go answer(t, manager, "s1", Response{Answers: [][]string{{"Red"}, {"S", "L"}}})

	out, err := NewTool(manager).Run(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"Pick a color"="Red"`) || !strings.Contains(out, `"Pick sizes"="S, L"`) {
		t.Fatalf("unexpected answer text: %q", out)
	}
	if len(*emitted) != 2 || (*emitted)[0].Type != event.QuestionAsked || (*emitted)[1].Type != event.QuestionReplied {
		t.Fatalf("unexpected events: %+v", *emitted)
	}
	answers, ok := call.Metadata["answers"].([][]string)
	if !ok || len(answers) != 2 || answers[0][0] != "Red" || answers[1][1] != "L" {
		t.Fatalf("unexpected answer metadata: %#v", call.Metadata["answers"])
	}
}

func TestToolCancelledWithMessage(t *testing.T) {
	manager := New()
	call, _ := testCall("s1", `[{"question":"Continue?","header":"Confirm","options":[{"label":"Yes","description":"go"}]}]`)
	go answer(t, manager, "s1", Response{Cancelled: true, Message: "no questions available"})

	_, err := NewTool(manager).Run(context.Background(), call)
	if err == nil || !strings.Contains(err.Error(), "no questions available") {
		t.Fatalf("want dismissal message, got %v", err)
	}
	if pending := manager.Pending(""); len(pending) != 0 {
		t.Fatalf("pending request leaked: %+v", pending)
	}
}

func TestReplyValidatesAnswerCountAndOwnership(t *testing.T) {
	manager := New()
	questions := []Prompt{{Question: "Q", Header: "H", Options: []Option{{Label: "A", Description: "a"}}}}
	done := make(chan Response, 1)
	go func() {
		response, _ := manager.Ask(context.Background(), "s1", questions, nil)
		done <- response
	}()
	var request Request
	for i := 0; i < 100 && request.ID == ""; i++ {
		if pending := manager.Pending("s1"); len(pending) > 0 {
			request = pending[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	if request.ID == "" {
		t.Fatal("request never pending")
	}
	if _, err := manager.Reply(request.ID, Response{Answers: [][]string{{"A"}, {"B"}}}); err == nil {
		t.Fatal("want answer count error")
	}
	if _, err := manager.Reply("missing", Response{}); err == nil {
		t.Fatal("want not found error")
	} else {
		var missing ErrNotFound
		if !errors.As(err, &missing) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	}
	if _, err := manager.Reply(request.ID, Response{Answers: [][]string{{"A"}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-done:
		if response.Cancelled || len(response.Answers) != 1 || response.Answers[0][0] != "A" {
			t.Fatalf("unexpected response: %+v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("ask did not return")
	}
}

func TestToolRejectsInvalidInputAndPermission(t *testing.T) {
	manager := New()
	call, _ := testCall("s1", `[]`)
	if _, err := NewTool(manager).Run(context.Background(), call); err == nil {
		t.Fatal("want empty questions error")
	}

	call, _ = testCall("s1", `[{"question":"Q","header":"H","options":[]}]`)
	if _, err := NewTool(manager).Run(context.Background(), call); err == nil {
		t.Fatal("want missing options error")
	}

	call, _ = testCall("s1", `[{"question":"Q","header":"H","options":[{"label":"A","description":"a"}]}]`)
	call.Perm = permission.New(permission.Rules{{Action: "question", Resource: "*", Effect: permission.Deny}})
	if _, err := NewTool(manager).Run(context.Background(), call); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("want permission denial, got %v", err)
	}
}

func TestToolRequiresEmitter(t *testing.T) {
	manager := New()
	call, _ := testCall("s1", `[{"question":"Q","header":"H","options":[{"label":"A","description":"a"}]}]`)
	call.Emit = nil
	if _, err := NewTool(manager).Run(context.Background(), call); err == nil || !strings.Contains(err.Error(), "emitter") {
		t.Fatalf("want emitter error, got %v", err)
	}
	if pending := manager.Pending(""); len(pending) != 0 {
		t.Fatalf("pending request leaked: %+v", pending)
	}
}
