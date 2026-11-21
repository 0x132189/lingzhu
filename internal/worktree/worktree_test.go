package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.name", "test")
	runGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func TestCreateListAndRemove(t *testing.T) {
	repo := initRepo(t)
	manager, err := New(repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	entry, err := manager.Create(ctx, "feature", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Branch != "lingzhu/feature" {
		t.Fatalf("unexpected branch: %+v", entry)
	}
	if !entry.Managed {
		t.Fatalf("created worktree should be managed: %+v", entry)
	}
	if _, err := os.Stat(filepath.Join(entry.Path, "a.txt")); err != nil {
		t.Fatalf("worktree files missing: %v", err)
	}
	if !strings.Contains(filepath.ToSlash(entry.Path), "/worktrees/") {
		t.Fatalf("worktree is not under the data directory: %s", entry.Path)
	}

	entries, err := manager.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || !entries[0].Main || entries[1].Path != entry.Path {
		t.Fatalf("unexpected worktree list: %+v", entries)
	}
	if !entries[1].Managed {
		t.Fatalf("created worktree should be managed: %+v", entries[1])
	}

	if _, err := manager.Create(ctx, "feature", "", ""); err == nil {
		t.Fatal("duplicate worktree should fail")
	}

	if err := manager.Remove(ctx, entry.Path, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree directory should be gone: %v", err)
	}
	entries, err = manager.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Main {
		t.Fatalf("unexpected worktrees after remove: %+v", entries)
	}
	if err := manager.Remove(ctx, entries[0].Path, false); err == nil {
		t.Fatal("main worktree must not be removable")
	}
}

func TestRejectsInvalidNamesAndNonRepository(t *testing.T) {
	repo := initRepo(t)
	manager, err := New(repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "../escape", "a/b", "-bad", strings.Repeat("x", 80)} {
		if _, err := manager.Create(context.Background(), name, "", ""); err == nil {
			t.Fatalf("name %q should be rejected", name)
		}
	}
	if _, err := New(t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("non-git directory should be rejected")
	}
}

func TestLinkedWorktreeUsesMainRepository(t *testing.T) {
	repo := initRepo(t)
	dataDir := t.TempDir()
	manager, err := New(repo, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	entry, err := manager.Create(ctx, "feature", "", "")
	if err != nil {
		t.Fatal(err)
	}

	linked, err := New(entry.Path, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(linked.Repo(), repo) {
		t.Fatalf("linked manager repo = %s, want %s", linked.Repo(), repo)
	}
	entries, err := linked.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || !entries[0].Main || !samePath(entries[0].Path, repo) {
		t.Fatalf("linked manager should still mark the main checkout: %+v", entries)
	}
	if entries[1].Main || !entries[1].Managed {
		t.Fatalf("linked checkout should be a managed worktree: %+v", entries[1])
	}

	second, err := linked.Create(ctx, "second", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Managed {
		t.Fatalf("worktree created from a linked checkout should be managed: %+v", second)
	}
}
