package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/config"
	"lingzhu/internal/job"
)

func TestBackgroundJobOutput(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	jobs := job.New()
	defer jobs.Close()
	srv.SetJobs(jobs)
	seedSession(t, store, "s1")
	seedSession(t, store, "s2")

	path := filepath.Join(t.TempDir(), "job.log")
	if err := os.WriteFile(path, []byte("old line\nnew line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	item := jobs.Create("s1", "cat job.log", path)

	srvHandler := srv.Handler()
	rec := httptest.NewRecorder()
	srvHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/background/"+item.ID+"/output", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Output  string `json:"output"`
		Running bool   `json:"running"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Running || !strings.Contains(payload.Output, "new line") {
		t.Fatalf("unexpected output payload: %+v", payload)
	}

	rec = httptest.NewRecorder()
	srvHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s2/background/"+item.ID+"/output", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("another session should 404, got %d", rec.Code)
	}
}

func TestCancelBackgroundJob(t *testing.T) {
	command := "sleep 30"
	if runtime.GOOS == "windows" {
		command = "ping -n 30 127.0.0.1"
	}
	jobs := job.New()
	defer jobs.Close()
	srv, _ := newSessionServer(t, func(s *Server) {
		s.SetConfig(config.Config{Shell: config.DefaultShell(), Workdir: t.TempDir()})
		s.SetJobs(jobs)
	})
	seedSession(t, srv.store, "s1")

	payload, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/background", strings.NewReader(string(payload))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected start status: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatalf("missing job id: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sessions/s1/background/"+created.ID, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("unexpected cancel: %d %s", rec.Code, rec.Body.String())
	}
	item, ok := jobs.Get(created.ID)
	if !ok || item.Status != job.Cancelled {
		t.Fatalf("job was not cancelled: %+v", item)
	}

	deadline := time.Now().Add(5 * time.Second)
	for item.Status == job.Running && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		item, _ = jobs.Get(created.ID)
	}
	if item.Status != job.Cancelled {
		t.Fatalf("cancelled job changed status: %+v", item)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sessions/s1/background/"+created.ID, nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second cancel should 409, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sessions/s1/background/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job should 404, got %d", rec.Code)
	}
}
