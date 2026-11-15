package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnoseGoSyntaxError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.go")
	if err := os.WriteFile(path, []byte("package bad\nfunc main( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diagnostics := diagnose(path)
	if len(diagnostics) == 0 || diagnostics[0].Severity != "error" || diagnostics[0].Line == 0 {
		t.Fatalf("expected a Go parse diagnostic, got %#v", diagnostics)
	}
}

func TestDiagnoseJSONSyntaxError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{\n  \"a\":\n}"), 0o644); err != nil {
		t.Fatal(err)
	}
	diagnostics := diagnose(path)
	if len(diagnostics) != 1 || diagnostics[0].Line != 3 {
		t.Fatalf("expected a JSON syntax diagnostic on line 3, got %#v", diagnostics)
	}
}

func TestDiagnoseJavaScript(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	path := filepath.Join(t.TempDir(), "bad.js")
	if err := os.WriteFile(path, []byte("const value = ;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diagnostics := diagnose(path)
	if len(diagnostics) == 0 || diagnostics[0].Severity != "error" {
		t.Fatalf("expected a JavaScript syntax diagnostic, got %#v", diagnostics)
	}
}

func TestSearchMetadataCounts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	globMeta := map[string]any{}
	glob := &Glob{Dir: dir}
	if _, err := glob.Run(context.Background(), Call{
		Name:     "glob",
		Input:    json.RawMessage(`{"pattern":"*.txt"}`),
		Metadata: globMeta,
	}); err != nil {
		t.Fatal(err)
	}
	if globMeta["count"] != 1 {
		t.Fatalf("unexpected glob metadata: %#v", globMeta)
	}

	grepMeta := map[string]any{}
	grep := &Grep{Dir: dir}
	if _, err := grep.Run(context.Background(), Call{
		Name:     "grep",
		Input:    json.RawMessage(`{"pattern":"needle"}`),
		Metadata: grepMeta,
	}); err != nil {
		t.Fatal(err)
	}
	if grepMeta["matches"] != 1 {
		t.Fatalf("unexpected grep metadata: %#v", grepMeta)
	}
}

func TestReadReportsAgentsMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	read := &Read{Dir: dir}
	metadata := map[string]any{}
	if _, err := read.Run(context.Background(), Call{
		SessionID: "s1",
		Name:      "read",
		Input:     json.RawMessage(`{"path":"AGENTS.md"}`),
		Metadata:  metadata,
	}); err != nil {
		t.Fatal(err)
	}
	loaded, ok := metadata["loaded"].([]string)
	if !ok || len(loaded) != 1 || filepath.Base(loaded[0]) != "AGENTS.md" {
		t.Fatalf("unexpected loaded metadata: %#v", metadata)
	}
}

func TestReadInjectsNestedInstructionsOnce(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "AGENTS.md"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	read := &Read{Dir: dir}
	var got []Instruction
	call := Call{
		SessionID: "s1",
		Name:      "read",
		Input:     json.RawMessage(`{"path":"sub/a.txt"}`),
		OnInstruction: func(instruction Instruction) {
			got = append(got, instruction)
		},
	}
	for range 2 {
		if _, err := read.Run(context.Background(), call); err != nil {
			t.Fatal(err)
		}
	}

	want := filepath.Join("sub", "AGENTS.md")
	if len(got) != 1 || len(got[0].Paths) != 1 || got[0].Paths[0] != want {
		t.Fatalf("unexpected nearby instructions: %#v", got)
	}
	if got[0].Description != "Loaded "+want || !strings.Contains(got[0].Text, "nested") {
		t.Fatalf("unexpected instruction payload: %#v", got[0])
	}
}
