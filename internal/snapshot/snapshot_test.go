package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCaptureAndRestore(t *testing.T) {
	workdir := t.TempDir()
	manager, err := New(workdir, t.TempDir())
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	ctx := context.Background()
	write(t, filepath.Join(workdir, "a.txt"), "one")
	write(t, filepath.Join(workdir, "sub", "b.txt"), "two")

	first, err := manager.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("empty snapshot id")
	}
	if again, err := manager.Capture(ctx); err != nil || again != first {
		t.Fatalf("unchanged capture should reuse %s, got %s (%v)", first, again, err)
	}

	write(t, filepath.Join(workdir, "a.txt"), "changed")
	if err := os.Remove(filepath.Join(workdir, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(workdir, "c.txt"), "new")
	second, err := manager.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("changed capture should create a new snapshot")
	}

	changed, err := manager.Changed(ctx, first, second)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.txt", "c.txt", "sub/b.txt"}
	if len(changed) != len(want) {
		t.Fatalf("unexpected changes: %v", changed)
	}
	for i, path := range want {
		if changed[i] != path {
			t.Fatalf("unexpected changes: %v", changed)
		}
	}

	if err := manager.Restore(ctx, first); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(workdir, "a.txt")); got != "one" {
		t.Fatalf("a.txt not restored: %q", got)
	}
	if got := read(t, filepath.Join(workdir, "sub", "b.txt")); got != "two" {
		t.Fatalf("b.txt not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(workdir, "c.txt")); !os.IsNotExist(err) {
		t.Fatalf("c.txt should be removed, err=%v", err)
	}

	if err := manager.Restore(ctx, second); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(workdir, "a.txt")); got != "changed" {
		t.Fatalf("a.txt not reapplied: %q", got)
	}
	if got := read(t, filepath.Join(workdir, "c.txt")); got != "new" {
		t.Fatalf("c.txt not reapplied: %q", got)
	}
	if _, err := os.Stat(filepath.Join(workdir, "sub", "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("b.txt should stay deleted, err=%v", err)
	}
}

func TestInvalidSnapshotRejected(t *testing.T) {
	workdir := t.TempDir()
	manager, err := New(workdir, t.TempDir())
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if err := manager.Restore(context.Background(), "HEAD; rm -rf /"); err == nil {
		t.Fatal("invalid snapshot should be rejected")
	}
}

func TestDiffBetweenSnapshots(t *testing.T) {
	workdir := t.TempDir()
	manager, err := New(workdir, t.TempDir())
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	ctx := context.Background()
	write(t, filepath.Join(workdir, "a.txt"), "one\n")
	write(t, filepath.Join(workdir, "c.txt"), "remove\n")
	from, err := manager.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(workdir, "a.txt"), "one\ntwo\n")
	write(t, filepath.Join(workdir, "b.txt"), "new\n")
	if err := os.Remove(filepath.Join(workdir, "c.txt")); err != nil {
		t.Fatal(err)
	}
	to, err := manager.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := manager.Diff(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	byFile := map[string]FileDiff{}
	for _, diff := range diffs {
		byFile[diff.File] = diff
	}
	a, ok := byFile["a.txt"]
	if !ok || a.Status != "modified" || a.Additions != 1 || a.Deletions != 0 || !strings.Contains(a.Patch, "+two") {
		t.Fatalf("unexpected a.txt diff: %+v", a)
	}
	b, ok := byFile["b.txt"]
	if !ok || b.Status != "added" || b.Additions != 1 || !strings.Contains(b.Patch, "+new") {
		t.Fatalf("unexpected b.txt diff: %+v", b)
	}
	c, ok := byFile["c.txt"]
	if !ok || c.Status != "deleted" || c.Deletions != 1 {
		t.Fatalf("unexpected c.txt diff: %+v", c)
	}
}
