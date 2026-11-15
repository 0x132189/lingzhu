package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"lingzhu/internal/config"
	"lingzhu/internal/job"
)

func TestListWorkspaceJobs(t *testing.T) {
	command := "sleep 30"
	if runtime.GOOS == "windows" {
		command = "ping -n 30 127.0.0.1"
	}
	jobs := job.New()
	defer jobs.Close()
	srv, _ := newSessionServer(t, func(s *Server) {
		s.SetJobs(jobs)
	})

	dir := t.TempDir()
	first, err := jobs.Start("s1", config.DefaultShell(), dir, command, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := jobs.Start("s2", config.DefaultShell(), dir, command, 0)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/background", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Jobs []struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionId"`
			Status    string `json:"status"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Jobs) != 2 {
		t.Fatalf("want two jobs, got %+v", payload.Jobs)
	}
	sessions := map[string]bool{}
	for _, item := range payload.Jobs {
		sessions[item.SessionID] = true
		if item.Status != string(job.Running) {
			t.Fatalf("unexpected job status: %+v", item)
		}
	}
	if !sessions["s1"] || !sessions["s2"] {
		t.Fatalf("missing cross-session jobs: %+v", payload.Jobs)
	}
	if payload.Jobs[0].ID != first.ID && payload.Jobs[1].ID != first.ID {
		t.Fatalf("first job missing: %+v", payload.Jobs)
	}
	if payload.Jobs[0].ID != second.ID && payload.Jobs[1].ID != second.ID {
		t.Fatalf("second job missing: %+v", payload.Jobs)
	}
}
