package vcs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "checkout", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "Test")
	writeFile(t, dir, "committed.txt", "one\ntwo\n")
	writeFile(t, dir, "gone.txt", "remove me\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStatusReportsCountsAndKinds(t *testing.T) {
	dir := newRepo(t)
	manager, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "committed.txt", "one\nTWO\nthree\n")
	writeFile(t, dir, "new.txt", "hello\nworld\n")
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byFile := map[string]FileStatus{}
	for _, item := range status {
		byFile[item.File] = item
	}
	if item := byFile["committed.txt"]; item.Status != "modified" || item.Additions != 2 || item.Deletions != 1 {
		t.Fatalf("unexpected modified entry: %+v", item)
	}
	if item := byFile["new.txt"]; item.Status != "added" || item.Additions != 2 || item.Deletions != 0 {
		t.Fatalf("unexpected added entry: %+v", item)
	}
	if item := byFile["gone.txt"]; item.Status != "deleted" || item.Deletions != 1 {
		t.Fatalf("unexpected deleted entry: %+v", item)
	}
}

func TestWorkingDiffIncludesTrackedAndUntracked(t *testing.T) {
	dir := newRepo(t)
	manager, _ := New(dir)
	writeFile(t, dir, "committed.txt", "one\nTWO\n")
	writeFile(t, dir, "dir/new.txt", "alpha\nbeta\n")

	diffs, err := manager.Diff(context.Background(), DiffOptions{Mode: "working"})
	if err != nil {
		t.Fatal(err)
	}
	byFile := map[string]FileDiff{}
	for _, item := range diffs {
		byFile[item.File] = item
	}
	modified := byFile["committed.txt"]
	if modified.Status != "modified" || modified.Additions != 1 || modified.Deletions != 1 || !strings.Contains(modified.Patch, "-two") || !strings.Contains(modified.Patch, "+TWO") {
		t.Fatalf("unexpected modified diff: %+v", modified)
	}
	added := byFile["dir/new.txt"]
	if added.Status != "added" || added.Additions != 2 || !strings.Contains(added.Patch, "+alpha") || !strings.Contains(added.Patch, "new file mode") {
		t.Fatalf("unexpected untracked diff: %+v", added)
	}

	grouped, err := manager.Diff(context.Background(), DiffOptions{Mode: "working", Untracked: "folders"})
	if err != nil {
		t.Fatal(err)
	}
	foundFolder := false
	for _, item := range grouped {
		if item.File == "dir/" && strings.Contains(item.Patch, "# dir/new.txt") && item.Additions == 2 {
			foundFolder = true
		}
	}
	if !foundFolder {
		t.Fatalf("folder grouping missing: %+v", grouped)
	}
}

func TestInfoBranchesAndBase(t *testing.T) {
	dir := newRepo(t)
	manager, _ := New(dir)
	ctx := context.Background()

	info, err := manager.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Provider != "git" || info.Branch.Current != "main" || info.Branch.Default != "main" {
		t.Fatalf("unexpected info: %+v", info)
	}

	git(t, dir, "checkout", "-q", "-b", "feature")
	writeFile(t, dir, "feature.txt", "feature\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "feature work")

	info, _ = manager.Info(ctx)
	if info.Branch.Current != "feature" || info.Branch.Default != "main" {
		t.Fatalf("unexpected feature info: %+v", info)
	}
	branches, err := manager.Branches(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 2 || branches[0] != "feature" || branches[1] != "main" {
		t.Fatalf("unexpected branches: %+v", branches)
	}
	filtered, _ := manager.Branches(ctx, "FEAT", 10)
	if len(filtered) != 1 || filtered[0] != "feature" {
		t.Fatalf("unexpected filtered branches: %+v", filtered)
	}

	base, err := manager.Base(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if base == nil || base.Name != "main" || base.Source != "default" || base.Ref == "" {
		t.Fatalf("unexpected base: %+v", base)
	}
	mergeBase := git(t, dir, "merge-base", "HEAD", "main")
	if base.Ref != mergeBase {
		t.Fatalf("base ref should be the merge-base: %s != %s", base.Ref, mergeBase)
	}
}

func TestBranchAndCommittedDiff(t *testing.T) {
	dir := newRepo(t)
	manager, _ := New(dir)
	ctx := context.Background()
	git(t, dir, "checkout", "-q", "-b", "feature")
	writeFile(t, dir, "committed.txt", "one\ntwo\nbranch\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "branch change")

	committed, err := manager.Diff(ctx, DiffOptions{Mode: "committed"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range committed {
		if item.File == "committed.txt" && item.Status == "modified" && strings.Contains(item.Patch, "+branch") {
			found = true
		}
	}
	if !found {
		t.Fatalf("committed diff missing: %+v", committed)
	}

	writeFile(t, dir, "committed.txt", "one\ntwo\nbranch\nworking\n")
	diffs, err := manager.Diff(ctx, DiffOptions{Mode: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, item := range diffs {
		if item.File == "committed.txt" && strings.Contains(item.Patch, "+working") {
			found = true
		}
	}
	if !found {
		t.Fatalf("branch diff missing working change: %+v", diffs)
	}
	if _, err := manager.Diff(ctx, DiffOptions{Mode: "bogus"}); err == nil {
		t.Fatal("unknown mode should fail")
	}
}

func TestNotRepositoryAndInit(t *testing.T) {
	dir := t.TempDir()
	manager, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Info(context.Background()); err != ErrNotRepository {
		t.Fatalf("want ErrNotRepository, got %v", err)
	}
	if _, err := manager.Diff(context.Background(), DiffOptions{}); err != ErrNotRepository {
		t.Fatalf("diff should reject non-repositories: %v", err)
	}
	if err := manager.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Info(context.Background()); err != nil {
		t.Fatalf("init did not create a repository: %v", err)
	}
	if err := manager.Init(context.Background()); err != ErrAlreadyRepository {
		t.Fatalf("second init should conflict: %v", err)
	}
	base, err := manager.Base(context.Background())
	if err != nil || base != nil {
		t.Fatalf("empty repository should have no base: %+v %v", base, err)
	}
}
