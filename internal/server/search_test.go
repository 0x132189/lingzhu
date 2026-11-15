package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/event"
)

func TestSearchEvents(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	seedSession(t, store, "s2")
	if _, err := store.Append("s1", event.Assistant("The QUARTZ marker is here")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s2", event.Assistant("nothing to see")); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search?q=quartz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected search status: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Hits []struct {
			SessionID string `json:"sessionId"`
			Type      string `json:"type"`
			Snippet   string `json:"snippet"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Hits) != 1 || payload.Hits[0].SessionID != "s1" || !strings.Contains(payload.Hits[0].Snippet, "QUARTZ") {
		t.Fatalf("unexpected hits: %+v", payload.Hits)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search?q=missing", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"hits":[]`) {
		t.Fatalf("unexpected empty search: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search?q=", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty query should 400, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search?q=x&limit=0", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit should 400, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search?q=marker&limit=1", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Hits) != 1 {
		t.Fatalf("limit should cap hits: %+v", payload.Hits)
	}
}
