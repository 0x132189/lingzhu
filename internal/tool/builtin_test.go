package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteThenRead(t *testing.T) {
	dir := t.TempDir()
	registry := NewRegistry()
	for _, tl := range Builtins([]string{"bash", "-lc"}, dir) {
		registry.Add(tl)
	}

	if _, err := registry.Run(context.Background(), "write_file", []byte(`{"path":"sub/a.txt","content":"line1\nline2\n"}`)); err != nil {
		t.Fatal(err)
	}
	out, err := registry.Run(context.Background(), "read_file", []byte(`{"path":"sub/a.txt"}`))
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

func TestUnknownTool(t *testing.T) {
	registry := NewRegistry()
	if _, err := registry.Run(context.Background(), "nope", nil); err == nil {
		t.Fatal("want error for unknown tool")
	}
}

func TestDefinitionsSorted(t *testing.T) {
	dir := t.TempDir()
	registry := NewRegistry()
	for _, tl := range Builtins([]string{"bash", "-lc"}, dir) {
		registry.Add(tl)
	}
	defs := registry.Definitions()
	if len(defs) != 3 {
		t.Fatalf("want 3 definitions, got %d", len(defs))
	}
	for i := 1; i < len(defs); i++ {
		if defs[i-1].Name > defs[i].Name {
			t.Fatalf("definitions not sorted: %v", defs)
		}
	}
}
