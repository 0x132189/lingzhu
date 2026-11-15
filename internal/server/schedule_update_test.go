package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/schedule"
)

func TestScheduleUpdatePauseResume(t *testing.T) {
	manager := newScheduleManager(t)
	srv, store := newSessionServer(t, func(s *Server) { s.SetSchedules(manager) })
	seedSession(t, store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	created, err := manager.Create("s1", schedule.CreateRequest{
		Name:   "nightly",
		Prompt: "run the suite",
		When:   schedule.When{Type: "in", In: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}

	patch := func(body string) (*http.Response, schedule.Item) {
		req, err := http.NewRequest(http.MethodPatch, httpSrv.URL+"/schedules/"+created.ID, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Schedule schedule.Item `json:"schedule"`
		}
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
		}
		resp.Body.Close()
		return resp, payload.Schedule
	}

	resp, item := patch(`{"sessionId":"s1","name":"renamed","prompt":"new prompt"}`)
	if resp.StatusCode != http.StatusOK || item.Name != "renamed" || item.Prompt != "new prompt" {
		t.Fatalf("unexpected update: %d %+v", resp.StatusCode, item)
	}

	resp, item = patch(`{"sessionId":"s1","action":"pause"}`)
	if resp.StatusCode != http.StatusOK || item.Status != schedule.StatusPaused {
		t.Fatalf("unexpected pause: %d %+v", resp.StatusCode, item)
	}
	resp, item = patch(`{"sessionId":"s1","action":"resume"}`)
	if resp.StatusCode != http.StatusOK || item.Status != schedule.StatusActive {
		t.Fatalf("unexpected resume: %d %+v", resp.StatusCode, item)
	}

	if resp, _ := patch(`{"sessionId":"s2","action":"pause"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign session pause should 404, got %d", resp.StatusCode)
	}
	if resp, _ := patch(`{"action":"pause"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing sessionId should 400, got %d", resp.StatusCode)
	}
	if resp, _ := patch(`{"sessionId":"s1","action":"explode"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown action should 400, got %d", resp.StatusCode)
	}

	srv2, _ := newSessionServer(t, nil)
	rec := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/schedules/x", strings.NewReader(`{"sessionId":"s1"}`)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("missing manager should 501, got %d", rec.Code)
	}
}
