package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"lingzhu/internal/event"
)

func TestRevertStateListsTurns(t *testing.T) {
	server := newTestServer(t)
	first, err := server.store.Append("s1", event.User("first prompt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.Append("s1", event.TurnSnapshotEvent(first, "snap1")); err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.Append("s1", event.Assistant("first answer")); err != nil {
		t.Fatal(err)
	}
	second, err := server.store.Append("s1", event.User("second prompt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.Append("s1", event.Assistant("second answer")); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/revert", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected revert state status: %d %s", rec.Code, rec.Body.String())
	}
	var state struct {
		Active bool `json:"active"`
		Turns  []struct {
			Seq         int64  `json:"seq"`
			Text        string `json:"text"`
			HasSnapshot bool   `json:"hasSnapshot"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Active {
		t.Fatalf("no revert should be staged: %s", rec.Body.String())
	}
	if len(state.Turns) != 2 {
		t.Fatalf("unexpected turns: %+v", state.Turns)
	}
	if state.Turns[0].Seq != second || state.Turns[0].Text != "second prompt" || state.Turns[0].HasSnapshot {
		t.Fatalf("unexpected newest turn: %+v", state.Turns[0])
	}
	if state.Turns[1].Seq != first || state.Turns[1].Text != "first prompt" || !state.Turns[1].HasSnapshot {
		t.Fatalf("unexpected oldest turn: %+v", state.Turns[1])
	}
}
