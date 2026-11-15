package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"lingzhu/internal/job"
	"lingzhu/internal/pty"
)

// TestShellOutputFallsBackToJobs is the regression for a transcript card that
// asks the shell endpoint for a background job's output. The agent's shell tool
// registers the command with the job manager, not the pty manager, so the
// endpoint used to answer 404 even while the job was running.
func TestShellOutputFallsBackToJobs(t *testing.T) {
	dir := t.TempDir()
	jobs, err := job.Open(filepath.Join(dir, "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "jobs", "job_1_1.log")
	if err := os.WriteFile(output, []byte("vite v5 ready\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	created := jobs.Create("s1", "npm run dev", output)
	if created.ID == "" {
		t.Fatal("the job must have an id")
	}

	terminals, err := pty.New(dir, []string{"cmd", "/c"}, dir)
	if err != nil {
		t.Fatal(err)
	}

	srv, _ := newSessionServer(t, func(s *Server) {
		s.SetPTY(terminals)
		s.SetJobs(jobs)
	})
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/shell/" + created.ID + "/output")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a background job must be readable on the shell endpoint, got %d", resp.StatusCode)
	}
	var payload struct {
		Output  string `json:"output"`
		Running bool   `json:"running"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Output != "vite v5 ready\n" {
		t.Fatalf("unexpected output: %q", payload.Output)
	}
	if !payload.Running {
		t.Fatal("a fresh job must report running")
	}
}

// TestShellOutputKeepsMissingCommandsMissing proves the fallback does not turn
// a genuinely unknown id into an empty page.
func TestShellOutputKeepsMissingCommandsMissing(t *testing.T) {
	dir := t.TempDir()
	jobs, err := job.Open(filepath.Join(dir, "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	terminals, err := pty.New(dir, []string{"cmd", "/c"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := newSessionServer(t, func(s *Server) {
		s.SetPTY(terminals)
		s.SetJobs(jobs)
	})
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/shell/job_missing/output")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown id must stay 404, got %d", resp.StatusCode)
	}
}
