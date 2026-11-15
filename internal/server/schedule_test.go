package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/schedule"
)

func newScheduleManager(t *testing.T) *schedule.Manager {
	t.Helper()
	manager, err := schedule.New(t.TempDir(), schedule.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return manager
}

func postSchedule(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestScheduleCreateListDelete(t *testing.T) {
	manager := newScheduleManager(t)
	srv, store := newSessionServer(t, func(s *Server) { s.SetSchedules(manager) })
	seedSession(t, store, "s1")
	seedSession(t, store, "s2")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	created := postSchedule(t, httpSrv.URL+"/schedules", `{
		"sessionId":"s1",
		"name":"hourly check",
		"prompt":"check the build",
		"when":{"type":"in","in":"1h"}
	}`)
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("want 201 for create, got %d", created.StatusCode)
	}
	var payload struct {
		Schedule schedule.Item `json:"schedule"`
	}
	if err := json.NewDecoder(created.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	item := payload.Schedule
	if item.ID == "" || item.SessionID != "s1" || item.Prompt != "check the build" || item.Status != schedule.StatusActive {
		t.Fatalf("unexpected schedule: %+v", item)
	}
	if item.NextDueAt.IsZero() {
		t.Fatalf("next due time missing: %+v", item)
	}

	listed, err := http.Get(httpSrv.URL + "/schedules?sessionId=s1")
	if err != nil {
		t.Fatal(err)
	}
	var schedules struct {
		Schedules []schedule.Item `json:"schedules"`
	}
	if err := json.NewDecoder(listed.Body).Decode(&schedules); err != nil {
		t.Fatal(err)
	}
	listed.Body.Close()
	if len(schedules.Schedules) != 1 || schedules.Schedules[0].ID != item.ID {
		t.Fatalf("unexpected schedule list: %+v", schedules.Schedules)
	}

	all, err := http.Get(httpSrv.URL + "/schedules")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(all.Body).Decode(&schedules); err != nil {
		t.Fatal(err)
	}
	all.Body.Close()
	if len(schedules.Schedules) != 1 {
		t.Fatalf("workspace listing should include the schedule: %+v", schedules.Schedules)
	}

	for _, body := range []string{
		`{"prompt":"x","when":{"type":"in","in":"1h"}}`,
		`{"sessionId":"missing","prompt":"x","when":{"type":"in","in":"1h"}}`,
		`{"sessionId":"s1","prompt":" ","when":{"type":"in","in":"1h"}}`,
		`{"sessionId":"s1","prompt":"x","when":{"type":"cron","every":"1h"}}`,
	} {
		resp := postSchedule(t, httpSrv.URL+"/schedules", body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated {
			t.Fatalf("invalid schedule should not be created: %s", body)
		}
	}

	request := func(method, target string) int {
		req, err := http.NewRequest(method, httpSrv.URL+target, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := request(http.MethodDelete, "/schedules/"+item.ID+"?sessionId=s2"); code != http.StatusNotFound {
		t.Fatalf("foreign session delete should 404, got %d", code)
	}
	if code := request(http.MethodDelete, "/schedules/unknown?sessionId=s1"); code != http.StatusNotFound {
		t.Fatalf("unknown schedule delete should 404, got %d", code)
	}
	if code := request(http.MethodDelete, "/schedules/"+item.ID); code != http.StatusBadRequest {
		t.Fatalf("missing sessionId should 400, got %d", code)
	}
	if code := request(http.MethodDelete, "/schedules/"+item.ID+"?sessionId=s1"); code != http.StatusNoContent {
		t.Fatalf("want 204 for delete, got %d", code)
	}
	if code := request(http.MethodDelete, "/schedules/"+item.ID+"?sessionId=s1"); code != http.StatusNotFound {
		t.Fatalf("second delete should 404, got %d", code)
	}
}

func TestScheduleEndpointsDisabled(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/schedules",
		strings.NewReader(`{"sessionId":"s1","prompt":"x","when":{"type":"in","in":"1h"}}`)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("missing manager should 501, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/schedules/x?sessionId=s1", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("missing manager delete should 501, got %d", rec.Code)
	}
}
