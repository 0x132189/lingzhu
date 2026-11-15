package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"lingzhu/internal/config"
)

type projectPayload struct {
	Directory    string `json:"directory"`
	Name         string `json:"name"`
	Config       string `json:"config"`
	Instructions string `json:"instructions"`
	Sessions     int    `json:"sessions"`
	VCS          any    `json:"vcs"`
}

type locationPayload struct {
	SessionID   string        `json:"sessionId"`
	Directory   string        `json:"directory"`
	Project     string        `json:"project"`
	Worktree    string        `json:"worktree"`
	Parent      string        `json:"parent"`
	ParentTitle string        `json:"parentTitle"`
	Provider    string        `json:"provider"`
	Model       string        `json:"model"`
	Agent       string        `json:"agent"`
	Permissions []config.Rule `json:"permissions"`
}

func TestProjectAndLocation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("project rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "lingzhu.json")
	srv, store := newSessionServer(t, func(s *Server) {
		s.SetWorkdir(root)
		s.SetConfig(config.Config{
			Path:       configPath,
			Permission: []config.Rule{{Action: "read", Resource: "*", Effect: "allow"}},
		})
	})
	seedSession(t, store, "s1")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/project", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for project, got %d: %s", rec.Code, rec.Body.String())
	}
	var project projectPayload
	if err := json.NewDecoder(rec.Body).Decode(&project); err != nil {
		t.Fatal(err)
	}
	if project.Directory != root || project.Name != filepath.Base(root) {
		t.Fatalf("unexpected project directory: %+v", project)
	}
	if project.Config != configPath {
		t.Fatalf("unexpected config path: %+v", project)
	}
	if project.Instructions != filepath.Join(root, "AGENTS.md") {
		t.Fatalf("unexpected instructions path: %+v", project)
	}
	if project.Sessions != 1 || project.VCS != nil {
		t.Fatalf("unexpected project metadata: %+v", project)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/location", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for location, got %d: %s", rec.Code, rec.Body.String())
	}
	var location locationPayload
	if err := json.NewDecoder(rec.Body).Decode(&location); err != nil {
		t.Fatal(err)
	}
	if location.SessionID != "s1" || location.Directory != root || location.Project != filepath.Base(root) {
		t.Fatalf("unexpected location: %+v", location)
	}
	if location.Provider != "test" || location.Model != "m" {
		t.Fatalf("unexpected model location: %+v", location)
	}
	if len(location.Permissions) != 1 || location.Permissions[0].Action != "read" {
		t.Fatalf("unexpected permissions: %+v", location.Permissions)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/missing/location", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing session should 404, got %d", rec.Code)
	}
}

func TestSessionLocationReportsParent(t *testing.T) {
	root := t.TempDir()
	srv, store := newSessionServer(t, func(s *Server) { s.SetWorkdir(root) })
	seedSession(t, store, "parent")
	seedSession(t, store, "child")
	if err := store.SetTitle("parent", "Root task"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetParent("child", "parent"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/child/location", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for location, got %d: %s", rec.Code, rec.Body.String())
	}
	var location locationPayload
	if err := json.NewDecoder(rec.Body).Decode(&location); err != nil {
		t.Fatal(err)
	}
	if location.Parent != "parent" || location.ParentTitle != "Root task" {
		t.Fatalf("unexpected parent lineage: %+v", location)
	}
	if location.Worktree != "" {
		t.Fatalf("main checkout should not report a worktree: %+v", location)
	}
}

func TestProjectRequiresWorkspace(t *testing.T) {
	srv, _ := newSessionServer(t, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/project", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing workspace should 400, got %d", rec.Code)
	}
}
