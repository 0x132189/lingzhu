package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"addr":":9000","data_dir":"data","model":"m","max_steps":4,"workdir":"ws","context_window":64000,"compact_keep":8000}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":9000" || cfg.Model != "m" || cfg.MaxSteps != 4 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if want := filepath.Join(dir, "data"); cfg.DataDir != want {
		t.Fatalf("data dir should resolve next to config: want %q, got %q", want, cfg.DataDir)
	}
	if want := filepath.Join(dir, "ws"); cfg.Workdir != want {
		t.Fatalf("workdir should resolve next to config: want %q, got %q", want, cfg.Workdir)
	}
	if cfg.ContextWindow != 64000 || cfg.CompactKeep != 8000 {
		t.Fatalf("unexpected context settings: %+v", cfg)
	}
}

func TestZeroContextWindowDisablesCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"context_window":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ContextWindow != 0 {
		t.Fatalf("explicit zero should disable compaction, got %d", cfg.ContextWindow)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"addr":":9000","model":"file-model"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINGZHU_ADDR", ":7000")
	t.Setenv("LINGZHU_MODEL", "env-model")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":7000" || cfg.Model != "env-model" {
		t.Fatalf("env should override file: %+v", cfg)
	}
}

func TestMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:8787" || cfg.MaxSteps != 16 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestBrokenFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("want error for invalid json")
	}
}
