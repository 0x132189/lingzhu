//go:build windows

package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestDefaultShellPrefersPwsh(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "pwsh.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	shell := DefaultShell()
	if filepath.Clean(shell[0]) != filepath.Clean(exe) {
		t.Fatalf("pwsh should come first, got %+v", shell)
	}
	if len(shell) < 2 || shell[len(shell)-1] != "-Command" {
		t.Fatalf("pwsh arguments are incomplete: %+v", shell)
	}
}

func TestLoadKeepsAnExplicitShell(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	explicit := `{"shell":["cmd.exe","/c"]}`
	if err := os.WriteFile(path, []byte(explicit), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Shell, []string{"cmd.exe", "/c"}) {
		t.Fatalf("an explicit shell must be preserved: %+v", cfg.Shell)
	}
}

func TestDefaultShellRuns(t *testing.T) {
	shell := DefaultShell()
	if len(shell) < 2 {
		t.Fatalf("default shell is incomplete: %+v", shell)
	}
	args := append(append([]string{}, shell[1:]...), "exit 0")
	if output, err := exec.Command(shell[0], args...).CombinedOutput(); err != nil {
		t.Fatalf("default shell %q is not runnable: %v (%s)", shell[0], err, output)
	}
}
