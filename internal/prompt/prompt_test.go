package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildIncludesEnvironmentAndInstructions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("- Write English comments"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	out := Build(Context{
		Workdir: dir,
		Shell:   []string{"bash", "-lc"},
		Extra:   "Always run go test.",
		Now:     now,
	})
	for _, want := range []string{dir, "bash -lc", "2026-10-31", "Write English comments", "Always run go test."} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt missing %q:\n%s", want, out)
		}
	}
}

func TestInstructionsWalksUp(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("root rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Instructions(nested); got != "root rules" {
		t.Fatalf("want instructions from parent, got %q", got)
	}
}

func TestInstructionsMissingIsEmpty(t *testing.T) {
	if got := Instructions(t.TempDir()); got != "" {
		t.Fatalf("want empty instructions, got %q", got)
	}
}
