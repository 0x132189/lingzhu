package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/vcs"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func vcsRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "checkout", "-q", "-b", "main")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func TestVCSEndpoints(t *testing.T) {
	dir := vcsRepo(t)
	manager, err := vcs.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t)
	server.SetVCS(manager)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vcs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected info status: %d %s", rec.Code, rec.Body.String())
	}
	var info struct {
		Info vcs.Info `json:"info"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Info.Provider != "git" || info.Info.Branch.Current != "main" || info.Info.Branch.Default != "main" {
		t.Fatalf("unexpected info: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vcs/branch?search=ma", nil))
	var branches struct {
		Branches []string `json:"branches"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &branches); err != nil {
		t.Fatal(err)
	}
	if len(branches.Branches) != 1 || branches.Branches[0] != "main" {
		t.Fatalf("unexpected branches: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vcs/base", nil))
	var base struct {
		Base *vcs.Base `json:"base"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &base); err != nil {
		t.Fatal(err)
	}
	if base.Base == nil || base.Base.Name != "main" {
		t.Fatalf("unexpected base: %s", rec.Body.String())
	}

	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("one\nTWO\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vcs/status", nil))
	var status struct {
		Files []vcs.FileStatus `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	byFile := map[string]vcs.FileStatus{}
	for _, item := range status.Files {
		byFile[item.File] = item
	}
	if item := byFile["app.txt"]; item.Status != "modified" || item.Additions != 2 || item.Deletions != 1 {
		t.Fatalf("unexpected status: %s", rec.Body.String())
	}
	if item := byFile["new.txt"]; item.Status != "added" || item.Additions != 1 {
		t.Fatalf("unexpected new status: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vcs/diff?mode=working&context=1", nil))
	var diffs struct {
		Diffs []vcs.FileDiff `json:"diffs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &diffs); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range diffs.Diffs {
		if item.File == "app.txt" && strings.Contains(item.Patch, "+TWO") {
			found = true
		}
	}
	if !found {
		t.Fatalf("working diff missing: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vcs/diff?mode=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown mode should 400, got %d", rec.Code)
	}
}

func TestVCSNotRepositoryAndDisabled(t *testing.T) {
	server := newTestServer(t)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vcs", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("disabled vcs should 501, got %d", rec.Code)
	}

	manager, err := vcs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server.SetVCS(manager)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vcs/status", nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not a git repository") {
		t.Fatalf("unexpected non-repo response: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vcs/init", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("init should succeed: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vcs/init", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second init should conflict: %d %s", rec.Code, rec.Body.String())
	}
}
