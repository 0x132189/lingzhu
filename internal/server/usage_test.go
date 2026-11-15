package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"lingzhu/internal/event"
)

func TestWorkspaceUsage(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	seedSession(t, store, "s2")
	usage := func(input, output int) event.Event {
		return event.New(event.Usage, map[string]any{
			"input":      input,
			"output":     output,
			"cacheRead":  1,
			"cacheWrite": 2,
		})
	}
	if _, err := store.Append("s1", usage(10, 5)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", usage(3, 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s2", usage(7, 1)); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected usage status: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Calls      int `json:"calls"`
		Input      int `json:"input"`
		Output     int `json:"output"`
		CacheRead  int `json:"cacheRead"`
		CacheWrite int `json:"cacheWrite"`
		Total      int `json:"total"`
		Sessions   []struct {
			SessionID string `json:"sessionId"`
			Calls     int    `json:"calls"`
			Total     int    `json:"total"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Calls != 3 || payload.Input != 20 || payload.Output != 8 {
		t.Fatalf("unexpected totals: %+v", payload)
	}
	if payload.CacheRead != 3 || payload.CacheWrite != 6 || payload.Total != 37 {
		t.Fatalf("unexpected cache totals: %+v", payload)
	}
	if len(payload.Sessions) != 2 || payload.Sessions[0].SessionID != "s1" || payload.Sessions[0].Calls != 2 {
		t.Fatalf("unexpected session rows: %+v", payload.Sessions)
	}
}
