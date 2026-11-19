package skill

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPullMarkdownSkill(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/skills/demo/SKILL.md" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, "---\nname: Remote Demo\ndescription: pulled skill\n---\nREMOTE_SKILL_MARKER\n")
	}))
	defer server.Close()

	roots := Pull(context.Background(), []string{server.URL + "/skills/demo/SKILL.md"}, t.TempDir(), t.Logf)
	if len(roots) != 1 {
		t.Fatalf("unexpected roots: %v", roots)
	}
	manager := Discover(roots, t.Logf)
	item, ok := manager.Get("demo")
	if !ok {
		t.Fatalf("pulled skill not discovered: %+v", manager.List())
	}
	if item.Name != "Remote Demo" || !strings.Contains(item.Content, "REMOTE_SKILL_MARKER") {
		t.Fatalf("unexpected pulled skill: %+v", item)
	}
}

func TestPullIndexAndUpdate(t *testing.T) {
	var mu sync.Mutex
	version := "1"
	counts := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current := version
		counts[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/index.json":
			fmt.Fprintf(w, `{"skills":[{"name":"demo","version":%q,"files":["SKILL.md","scripts/run.sh"]}]}`, current)
		case "/demo/SKILL.md":
			fmt.Fprintf(w, "---\ndescription: demo v%s\n---\nbody v%s\n", current, current)
		case "/demo/scripts/run.sh":
			fmt.Fprint(w, "echo run")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cache := t.TempDir()
	urls := []string{server.URL + "/index.json"}
	roots := Pull(context.Background(), urls, cache, t.Logf)
	if len(roots) != 1 {
		t.Fatalf("unexpected roots: %v", roots)
	}
	item, ok := Discover(roots, t.Logf).Get("demo")
	if !ok || !strings.Contains(item.Content, "body v1") {
		t.Fatalf("unexpected skill: %+v", item)
	}
	if len(item.Files) != 1 || !strings.HasSuffix(filepath.ToSlash(item.Files[0]), "scripts/run.sh") {
		t.Fatalf("unexpected files: %v", item.Files)
	}

	mu.Lock()
	first := counts["/demo/SKILL.md"]
	mu.Unlock()
	Pull(context.Background(), urls, cache, t.Logf)
	mu.Lock()
	if counts["/demo/SKILL.md"] != first {
		t.Fatalf("cached skill was downloaded again: %d -> %d", first, counts["/demo/SKILL.md"])
	}
	version = "2"
	mu.Unlock()

	roots = Pull(context.Background(), urls, cache, t.Logf)
	item, ok = Discover(roots, t.Logf).Get("demo")
	if !ok || !strings.Contains(item.Content, "body v2") {
		t.Fatalf("version bump did not refresh the skill: %+v", item)
	}
}

func TestPullBackfillsMissingFilesWithoutVersionBump(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			fmt.Fprintf(w, `{"skills":[{"name":"demo","version":"1","files":["SKILL.md","scripts/run.sh"]}]}`)
		case "/demo/SKILL.md":
			fmt.Fprint(w, "---\ndescription: demo\n---\nbody")
		case "/demo/scripts/run.sh":
			fmt.Fprint(w, "echo run")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cache := t.TempDir()
	urls := []string{server.URL + "/index.json"}
	roots := Pull(context.Background(), urls, cache, t.Logf)
	if len(roots) != 1 {
		t.Fatalf("unexpected roots: %v", roots)
	}
	// Simulate a cache written before the index gained the script. The
	// version is unchanged, so the missing file must still be filled in.
	_ = os.Remove(filepath.Join(roots[0], "scripts", "run.sh"))
	roots = Pull(context.Background(), urls, cache, t.Logf)
	if len(roots) != 1 {
		t.Fatalf("unexpected roots after backfill: %v", roots)
	}
	if _, err := os.Stat(filepath.Join(roots[0], "scripts", "run.sh")); err != nil {
		t.Fatalf("missing file was not backfilled: %v", err)
	}
}

func TestPullRejectsUnsafeEntries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			fmt.Fprint(w, `{"skills":[
				{"name":"../escape","files":["SKILL.md"]},
				{"name":"bad","files":["../secret.md"]},
				{"name":"remote","files":["https://evil.example/SKILL.md"]},
				{"name":"good","files":["SKILL.md"]}
			]}`)
		case "/index.json/good/SKILL.md", "/good/SKILL.md":
			fmt.Fprint(w, "---\ndescription: good\n---\ngood body")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cache := t.TempDir()
	roots := Pull(context.Background(), []string{server.URL + "/index.json"}, cache, t.Logf)
	if len(roots) != 1 {
		t.Fatalf("only the safe skill should be pulled: %v", roots)
	}
	if !strings.HasSuffix(filepath.ToSlash(roots[0]), "/good") {
		t.Fatalf("unexpected safe root: %v", roots[0])
	}
	if _, err := os.Stat(filepath.Join(cache, "escape")); err == nil {
		t.Fatal("unsafe skill escaped the cache directory")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cache), "secret.md")); err == nil {
		t.Fatal("unsafe file escaped the skill directory")
	}
}

func TestPullEnforcesSizeLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("a", maxRemoteFileBytes+1))
	}))
	defer server.Close()

	roots := Pull(context.Background(), []string{server.URL + "/big/SKILL.md"}, t.TempDir(), t.Logf)
	if len(roots) != 0 {
		t.Fatalf("oversized skill should be rejected: %v", roots)
	}
}

func TestPullHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	roots := Pull(ctx, []string{"http://127.0.0.1:1/skills/demo/SKILL.md"}, t.TempDir(), t.Logf)
	if len(roots) != 0 {
		t.Fatalf("canceled pull should return nothing: %v", roots)
	}
}

func TestSafeRelativePath(t *testing.T) {
	for _, value := range []string{"SKILL.md", "scripts/run.sh", "a/b/c.txt"} {
		if !safeRelative(value) {
			t.Fatalf("expected safe path: %q", value)
		}
	}
	for _, value := range []string{"", "../x", "a/../b", "/abs", "a\\b", "a//b", "x?y", "https://example.com/x"} {
		if safeRelative(value) {
			t.Fatalf("expected unsafe path: %q", value)
		}
	}
}
