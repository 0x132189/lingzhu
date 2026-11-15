package server

import (
	"testing"

	"lingzhu/internal/event"
)

func switchEvents(t *testing.T, store interface {
	Events(string) ([]event.Event, error)
}, id string) (agent int, model int) {
	t.Helper()
	events, err := store.Events(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		switch ev.Type {
		case event.AgentSelected:
			agent++
		case event.ModelSelected:
			model++
		}
	}
	return agent, model
}

// A session that has not recorded a selection yet answers with the default.
// Adopting that default is not a change, so the transcript must not draw a
// "Switched to ..." divider above the very first prompt.
func TestFirstSelectionDrawsNoSwitch(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")

	srv.announceAgent("s1", "build", srv.sessionAgent("s1"))
	previousProvider, previousModel := srv.sessionModel("s1")
	srv.announceModel("s1", "test", "m", previousProvider, previousModel)

	if agent, model := switchEvents(t, store, "s1"); agent != 0 || model != 0 {
		t.Fatalf("first selection recorded a switch: agent=%d model=%d", agent, model)
	}
}

// A real change is recorded once, and repeating it stays quiet.
func TestSelectionChangeDrawsOneSwitch(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	if err := store.SetAgent("s1", "build"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetModel("s1", "test", "m"); err != nil {
		t.Fatal(err)
	}

	srv.announceAgent("s1", "plan", srv.sessionAgent("s1"))
	srv.announceAgent("s1", "plan", "plan")
	srv.announceModel("s1", "test", "m2", "test", "m")
	srv.announceModel("s1", "test", "m2", "test", "m2")

	if agent, model := switchEvents(t, store, "s1"); agent != 1 || model != 1 {
		t.Fatalf("want one switch each, got agent=%d model=%d", agent, model)
	}
}
