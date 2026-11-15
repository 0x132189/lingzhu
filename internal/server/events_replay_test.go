package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/event"
)

func TestSessionEventsReplay(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	first, err := store.Append("s1", event.Assistant("first answer"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.Assistant("second answer")); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/events?after="+itoa(first), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected replay status: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "second answer") || strings.Contains(body, "first answer") {
		t.Fatalf("replay should only contain newer events: %s", body)
	}
	if !strings.Contains(body, "data: ") {
		t.Fatalf("expected SSE data lines: %s", body)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/events?after=9999", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "second answer") {
		t.Fatalf("empty replay should be empty: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/missing/events", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing session should 404, got %d", rec.Code)
	}
}
