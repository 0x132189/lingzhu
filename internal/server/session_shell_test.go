package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/agent"
	"lingzhu/internal/config"
	"lingzhu/internal/event"
	"lingzhu/internal/job"
	"lingzhu/internal/model"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/skill"
	"lingzhu/internal/tool"
)

func newSessionServer(t *testing.T, mutate func(*Server)) (*Server, *session.Store) {
	t.Helper()
	store := testStore(t)
	catalog := provider.Static("test", "Test", staticProvider{}, []provider.ModelSpec{{ID: "m", Name: "m"}}, "m")
	srv := New(agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 2}), store)
	if mutate != nil {
		mutate(srv)
	}
	return srv, store
}

func seedSession(t *testing.T, store *session.Store, id string) {
	t.Helper()
	if _, err := store.Append(id, event.User("seed")); err != nil {
		t.Fatal(err)
	}
}

func TestSkillEndpointStartsTurn(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: Demo\ndescription: demo skill\n---\nDEMO_SKILL_MARKER body\n"
	if err := os.WriteFile(filepath.Join(root, "demo", "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, store := newSessionServer(t, func(s *Server) {
		s.SetSkills(skill.Discover([]string{root}, nil))
	})
	seedSession(t, store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	resp, err := http.Post(httpSrv.URL+"/sessions/s1/skill", "application/json", strings.NewReader(`{"name":"demo","arguments":"focus on tests"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for skill, got %d", resp.StatusCode)
	}
	var accepted struct {
		Accepted bool   `json:"accepted"`
		Skill    string `json:"skill"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	if !accepted.Accepted || accepted.Skill != "demo" {
		t.Fatalf("unexpected accepted payload: %+v", accepted)
	}

	var messages []model.Message
	deadline := time.Now().Add(2 * time.Second)
	for {
		messages, err = store.Messages("s1")
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(messages) != 3 {
		t.Fatalf("unexpected skill turn history: %+v", messages)
	}
	if !strings.Contains(messages[1].Content, "<skill_content") || !strings.Contains(messages[1].Content, "DEMO_SKILL_MARKER") {
		t.Fatalf("skill content missing: %q", messages[1].Content)
	}
	if !strings.Contains(messages[1].Content, "focus on tests") {
		t.Fatalf("skill arguments missing: %q", messages[1].Content)
	}
	if messages[2].Content != "hi!" {
		t.Fatalf("skill turn should get an assistant reply: %+v", messages)
	}
}

func TestSkillEndpointErrors(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/skill", strings.NewReader(`{"name":"demo"}`)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("missing skill manager should 501, got %d", rec.Code)
	}

	srv, store = newSessionServer(t, func(s *Server) { s.SetSkills(skill.Discover(nil, nil)) })
	seedSession(t, store, "s1")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/skill", strings.NewReader(`{"name":"missing"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing skill should 404, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/skill", strings.NewReader(`{"name":" "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty skill name should 400, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/missing/skill", strings.NewReader(`{"name":"demo"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing session should 404, got %d", rec.Code)
	}
}

func TestSessionShellForeground(t *testing.T) {
	workdir := t.TempDir()
	srv, store := newSessionServer(t, func(s *Server) {
		s.SetConfig(config.Config{Shell: config.DefaultShell(), Workdir: workdir})
	})
	seedSession(t, srv.store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	resp, err := http.Post(httpSrv.URL+"/sessions/s1/shell", "application/json", strings.NewReader(`{"command":"echo SHELL_MARKER"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for foreground shell, got %d", resp.StatusCode)
	}
	var result struct {
		Output   string `json:"output"`
		Exit     int    `json:"exit"`
		TimedOut bool   `json:"timedOut"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Exit != 0 || result.TimedOut || !strings.Contains(result.Output, "SHELL_MARKER") {
		t.Fatalf("unexpected shell result: %+v", result)
	}

	// The run joins the conversation as a shell message for the next prompt.
	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	last := messages[len(messages)-1]
	if last.Shell == nil || last.Shell.Command != "echo SHELL_MARKER" || !strings.Contains(last.Shell.Output, "SHELL_MARKER") {
		t.Fatalf("shell run was not recorded: %+v", last)
	}
	if !strings.Contains(last.Content, "executed by the user") || !strings.Contains(last.Content, "SHELL_MARKER") {
		t.Fatalf("shell context text missing: %q", last.Content)
	}
}

func TestSessionShellNonZeroExit(t *testing.T) {
	workdir := t.TempDir()
	srv, _ := newSessionServer(t, func(s *Server) {
		s.SetConfig(config.Config{Shell: config.DefaultShell(), Workdir: workdir})
	})
	seedSession(t, srv.store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	resp, err := http.Post(httpSrv.URL+"/sessions/s1/shell", "application/json", strings.NewReader(`{"command":"exit 3"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for a non-zero exit, got %d", resp.StatusCode)
	}
	var result struct {
		Exit     int  `json:"exit"`
		TimedOut bool `json:"timedOut"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Exit != 3 || result.TimedOut {
		t.Fatalf("unexpected shell result: %+v", result)
	}
}

func TestSessionBackgroundJobs(t *testing.T) {
	workdir := t.TempDir()
	jobs := job.New()
	defer jobs.Close()
	srv, _ := newSessionServer(t, func(s *Server) {
		s.SetConfig(config.Config{Shell: config.DefaultShell(), Workdir: workdir})
		s.SetJobs(jobs)
	})
	seedSession(t, srv.store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	resp, err := http.Post(httpSrv.URL+"/sessions/s1/background", "application/json", strings.NewReader(`{"command":"echo BG_MARKER"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for background job, got %d", resp.StatusCode)
	}
	var created struct {
		ID      string `json:"id"`
		Command string `json:"command"`
		Status  string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Command != "echo BG_MARKER" || created.Status != "running" {
		t.Fatalf("unexpected job payload: %+v", created)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		item, ok := jobs.Get(created.ID)
		if ok && item.Status != job.Running {
			if item.Status != job.Completed || !strings.Contains(item.Tail, "BG_MARKER") {
				t.Fatalf("unexpected job completion: %+v", item)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background job did not finish: %+v", item)
		}
		time.Sleep(25 * time.Millisecond)
	}

	list, err := http.Get(httpSrv.URL + "/sessions/s1/shell")
	if err != nil {
		t.Fatal(err)
	}
	defer list.Body.Close()
	var listed struct {
		Jobs []struct {
			ID string `json:"id"`
		} `json:"jobs"`
	}
	if err := json.NewDecoder(list.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Jobs) != 1 || listed.Jobs[0].ID != created.ID {
		t.Fatalf("unexpected job listing: %+v", listed.Jobs)
	}
}
