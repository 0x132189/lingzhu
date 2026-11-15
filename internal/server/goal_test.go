package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/event"
)

// goalServer builds a server with one idle session so the goal routes resolve.
func goalServer(t *testing.T) (*Server, string) {
	t.Helper()
	server := newTestServer(t)
	sessionID := "goal1"
	if _, err := server.store.Append(sessionID, event.User("hello")); err != nil {
		t.Fatal(err)
	}
	return server, sessionID
}

func TestGetGoalReturnsNullWhenUnset(t *testing.T) {
	server, sessionID := goalServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/sessions/"+sessionID+"/goal", nil)
	request.SetPathValue("id", sessionID)
	server.getGoal(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Goal any `json:"goal"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Goal != nil {
		t.Fatalf("want null goal, got %v", body.Goal)
	}
}

func TestGoalBareCommandExplainsUsage(t *testing.T) {
	server, sessionID := goalServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/sessions/"+sessionID+"/goal", strings.NewReader(`{}`))
	request.SetPathValue("id", sessionID)
	server.handleGoalPrompt(recorder, request, "", "", "")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a bare /goal, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "Usage: /goal") {
		t.Fatalf("usage hint missing: %s", recorder.Body.String())
	}
}

func TestGoalPauseWithoutGoalConflicts(t *testing.T) {
	server, sessionID := goalServer(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/sessions/"+sessionID+"/goal", strings.NewReader(`{"action":"pause"}`))
	request.SetPathValue("id", sessionID)
	server.setGoal(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestGoalObjectiveTooLongRejected(t *testing.T) {
	server, sessionID := goalServer(t)
	objective := strings.Repeat("x", 4001)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/sessions/"+sessionID+"/goal", strings.NewReader(`{"objective":"`+objective+`"}`))
	request.SetPathValue("id", sessionID)
	server.setGoal(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}
