package config

import (
	"encoding/json"
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
	if cfg.Path != path {
		t.Fatalf("config path not recorded: want %q, got %q", path, cfg.Path)
	}
}

func TestSavePermissionPreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"addr":":9000","custom":{"keep":true},"permission":[{"action":"read","resource":"*.env","effect":"ask"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := []Rule{
		{Action: "*", Resource: "*", Effect: "allow"},
		{Action: "bash", Resource: "git *", Effect: "allow"},
	}
	if err := SavePermission(path, rules); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if file["addr"] != ":9000" {
		t.Fatalf("other fields should be preserved: %+v", file)
	}
	if custom, ok := file["custom"].(map[string]any); !ok || custom["keep"] != true {
		t.Fatalf("unknown fields should be preserved: %+v", file["custom"])
	}
	entries, ok := file["permission"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("permission should be replaced: %+v", file["permission"])
	}
	if last, _ := entries[1].(map[string]any); last["action"] != "bash" || last["resource"] != "git *" {
		t.Fatalf("unexpected last rule: %+v", entries[1])
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

func TestLoadProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"provider":"work","model":"fast","providers":{"work":{"name":"Work","base_url":"https://work.test/v1/","api_key_env":"WORK_KEY","models":[{"id":"fast","name":"Fast","context_window":32000}]}}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "work" || cfg.Model != "fast" {
		t.Fatalf("unexpected selection: %+v", cfg)
	}
	provider := cfg.Providers["work"]
	if provider.Name != "Work" || provider.BaseURL != "https://work.test/v1" {
		t.Fatalf("unexpected provider: %+v", provider)
	}
	if len(provider.Models) != 1 || provider.Models[0].ContextWindow == nil || *provider.Models[0].ContextWindow != 32000 {
		t.Fatalf("unexpected models: %+v", provider.Models)
	}
}
