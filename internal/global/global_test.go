package global

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOverrideRedirectsWritableRoots(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LINGZHU_HOME", root)

	for name, want := range map[string]string{
		"data":  filepath.Join(root, "data"),
		"state": filepath.Join(root, "state"),
		"cache": filepath.Join(root, "cache"),
		"tmp":   filepath.Join(root, "tmp"),
	} {
		got := map[string]string{
			"data":  Data(),
			"state": State(),
			"cache": Cache(),
			"tmp":   Tmp(),
		}[name]
		if got != want {
			t.Fatalf("%s root = %q, want %q", name, got, want)
		}
	}
}

func TestConfigStaysPerUserWithoutOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LINGZHU_HOME", dir)
	t.Setenv("LINGZHU_CONFIG_DIR", filepath.Join(dir, "config"))
	if got, want := Config(), filepath.Join(dir, "config"); got != want {
		t.Fatalf("config root = %q, want %q", got, want)
	}
}

func TestConfigRootsAtAppName(t *testing.T) {
	t.Setenv("LINGZHU_CONFIG_DIR", "")
	t.Setenv("LINGZHU_HOME", "")
	base, err := os.UserConfigDir()
	if err != nil {
		t.Skip("no user config directory on this machine")
	}
	if got, want := Config(), filepath.Join(base, "lingzhu"); got != want {
		t.Fatalf("config root = %q, want %q", got, want)
	}
}
