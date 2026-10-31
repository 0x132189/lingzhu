package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/permission"
)

func newRegistry(t *testing.T, rules permission.Rules) (*Registry, string) {
	t.Helper()
	dir := t.TempDir()
	registry := NewRegistry()
	for _, tl := range Builtins([]string{"bash", "-lc"}, dir) {
		registry.Add(tl)
	}
	return registry, dir
}

func run(t *testing.T, registry *Registry, name, input string, perm *permission.Manager) (string, error) {
	t.Helper()
	return registry.Run(context.Background(), Call{
		Name:      name,
		Input:     []byte(input),
		Perm:      perm,
		SessionID: "test",
		OnAsk:     func(permission.Request) {},
	})
}

func TestWriteThenRead(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	perm := permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})

	if _, err := run(t, registry, "write_file", `{"path":"sub/a.txt","content":"line1\nline2\n"}`, perm); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, registry, "read_file", `{"path":"sub/a.txt"}`, perm)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1\tline1") || !strings.Contains(out, "2\tline2") {
		t.Fatalf("unexpected read output: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "a.txt")); err != nil {
		t.Fatalf("file not written under workdir: %v", err)
	}
}

func TestDeniedToolDoesNotRun(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	perm := permission.New(permission.Rules{{Action: "write", Resource: "*", Effect: permission.Deny}})

	_, err := run(t, registry, "write_file", `{"path":"a.txt","content":"x"}`, perm)
	if err == nil {
		t.Fatal("want permission error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("want denied error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "a.txt")); statErr == nil {
		t.Fatal("file should not be written when denied")
	}
}

func TestOutsideWorkdirNeedsExternalPermission(t *testing.T) {
	registry, _ := newRegistry(t, nil)
	rules := permission.Rules{
		{Action: "read", Resource: "*", Effect: permission.Allow},
		{Action: "external_directory", Resource: "*", Effect: permission.Deny},
	}
	perm := permission.New(rules)

	outside := filepath.Join(os.TempDir(), "lingzhu-external.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	if _, err := run(t, registry, "read_file", `{"path":"`+strings.ReplaceAll(outside, `\`, `\\`)+`"}`, perm); err == nil {
		t.Fatal("want external_directory denial")
	}
}

func TestUnknownTool(t *testing.T) {
	registry, _ := newRegistry(t, nil)
	if _, err := run(t, registry, "nope", `{}`, nil); err == nil {
		t.Fatal("want error for unknown tool")
	}
}

func TestDefinitionsSorted(t *testing.T) {
	registry, _ := newRegistry(t, nil)
	defs := registry.Definitions()
	if len(defs) != 6 {
		t.Fatalf("want 6 definitions, got %d", len(defs))
	}
	for i := 1; i < len(defs); i++ {
		if defs[i-1].Name > defs[i].Name {
			t.Fatalf("definitions not sorted: %v", defs)
		}
	}
}
