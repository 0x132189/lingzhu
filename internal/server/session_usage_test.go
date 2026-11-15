package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lingzhu/internal/event"
)

func TestSessionUsage(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	seedSession(t, store, "s2")

	first := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)
	one := event.UsageEvent(100, 10, 20, 5)
	one.At = first
	two := event.UsageEvent(200, 20, 0, 0)
	two.At = second
	for _, ev := range []event.Event{one, two} {
		if _, err := store.Append("s1", ev); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for usage, got %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		SessionID  string       `json:"sessionId"`
		Calls      int          `json:"calls"`
		Input      int          `json:"input"`
		Output     int          `json:"output"`
		CacheRead  int          `json:"cacheRead"`
		CacheWrite int          `json:"cacheWrite"`
		Total      int          `json:"total"`
		Last       *usageTotals `json:"last"`
		LastAt     string       `json:"lastAt"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.SessionID != "s1" || payload.Calls != 2 {
		t.Fatalf("unexpected usage header: %+v", payload)
	}
	if payload.Input != 300 || payload.Output != 30 || payload.CacheRead != 20 || payload.CacheWrite != 5 {
		t.Fatalf("unexpected totals: %+v", payload)
	}
	if payload.Total != 355 {
		t.Fatalf("unexpected grand total: %+v", payload)
	}
	if payload.Last == nil || payload.Last.Input != 200 || payload.Last.Output != 20 {
		t.Fatalf("unexpected last usage: %+v", payload.Last)
	}
	lastAt, err := time.Parse(time.RFC3339Nano, payload.LastAt)
	if err != nil || !lastAt.Equal(second) {
		t.Fatalf("unexpected lastAt %q: %v", payload.LastAt, err)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s2/usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for empty usage, got %d", rec.Code)
	}
	var empty struct {
		Calls  int     `json:"calls"`
		Total  int     `json:"total"`
		Last   any     `json:"last"`
		LastAt *string `json:"lastAt"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&empty); err != nil {
		t.Fatal(err)
	}
	if empty.Calls != 0 || empty.Total != 0 || empty.Last != nil || empty.LastAt != nil {
		t.Fatalf("empty session should have no usage: %+v", empty)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/missing/usage", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing session should 404, got %d", rec.Code)
	}
}
