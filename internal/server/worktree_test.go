package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/worktree"
)

func worktreeRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-q", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestWorktreeEndpoints(t *testing.T) {
	repo := worktreeRepo(t)
	manager, err := worktree.New(repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t)
	server.SetWorktrees(manager)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/worktrees", strings.NewReader(`{"name":"feature"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Worktree worktree.Entry `json:"worktree"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Worktree.Branch != "lingzhu/feature" {
		t.Fatalf("unexpected worktree: %+v", created.Worktree)
	}
	if _, err := os.Stat(filepath.Join(created.Worktree.Path, "a.txt")); err != nil {
		t.Fatalf("worktree files missing: %v", err)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/worktrees", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected list status: %d", rec.Code)
	}
	var listed struct {
		Worktrees []worktree.Entry `json:"worktrees"`
		Repo      string           `json:"repo"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Worktrees) != 2 || !listed.Worktrees[0].Main || listed.Repo == "" {
		t.Fatalf("unexpected list: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/worktrees?path="+url.QueryEscape(created.Worktree.Path), nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected remove status: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(created.Worktree.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree should be gone: %v", err)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/worktrees?path="+url.QueryEscape(repo), nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "main worktree") {
		t.Fatalf("main worktree removal should fail: %d %s", rec.Code, rec.Body.String())
	}
}
