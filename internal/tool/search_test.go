package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/permission"
)

func allowAll() *permission.Manager {
	return permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEditReplacesUniqueMatch(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	write(t, dir, "a.txt", "alpha\nbeta\n")

	if _, err := run(t, registry, "edit", `{"path":"a.txt","oldString":"beta","newString":"gamma"}`, allowAll()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "alpha\ngamma\n" {
		t.Fatalf("unexpected content: %q", data)
	}
}

func TestEditRejectsAmbiguousAndMissingMatch(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	write(t, dir, "a.txt", "same\nsame\n")

	if _, err := run(t, registry, "edit", `{"path":"a.txt","oldString":"same","newString":"x"}`, allowAll()); err == nil || !strings.Contains(err.Error(), "expected exactly one") {
		t.Fatalf("want ambiguity error, got %v", err)
	}
	if _, err := run(t, registry, "edit", `{"path":"a.txt","oldString":"absent","newString":"x"}`, allowAll()); err == nil || !strings.Contains(err.Error(), "Could not find") {
		t.Fatalf("want not-found error, got %v", err)
	}
}

func TestGlobMatchesNestedPatterns(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	write(t, dir, "a.go", "package a")
	write(t, dir, "sub/b.go", "package b")
	write(t, dir, "sub/deep/c.txt", "text")

	out, err := run(t, registry, "glob", `{"pattern":"**/*.go"}`, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "sub/b.go") || strings.Contains(out, "c.txt") {
		t.Fatalf("unexpected glob output: %q", out)
	}
}

func TestGrepFiltersByInclude(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	write(t, dir, "a.go", "needle here\nother\n")
	write(t, dir, "b.txt", "needle here\n")

	out, err := run(t, registry, "grep", `{"pattern":"needle","include":"*.go"}`, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:") || !strings.Contains(out, "Line 1: needle here") {
		t.Fatalf("unexpected grep output: %q", out)
	}
	if strings.Contains(out, "b.txt") {
		t.Fatalf("include filter ignored: %q", out)
	}
}

func TestGrepRejectsInvalidRegexp(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	write(t, dir, "a.txt", "x")
	if _, err := run(t, registry, "grep", `{"pattern":"("}`, allowAll()); err == nil {
		t.Fatal("want regexp error")
	}
}

func TestGrepLiteralAndCaseSensitivity(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	write(t, dir, "a.txt", "A.B\naxb\n")

	out, err := run(t, registry, "grep", `{"pattern":"A.B","literal":true}`, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Line 1: A.B") || strings.Contains(out, "axb") {
		t.Fatalf("literal match failed: %q", out)
	}

	out, err = run(t, registry, "grep", `{"pattern":"a.b","caseSensitive":false}`, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Line 1: A.B") {
		t.Fatalf("case-insensitive match failed: %q", out)
	}
}

func TestGrepSearchesSingleFile(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	write(t, dir, "a.txt", "needle\n")
	write(t, dir, "b.txt", "needle\n")

	out, err := run(t, registry, "grep", `{"pattern":"needle","path":"b.txt"}`, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "b.txt:") || !strings.Contains(out, "Line 1: needle") || strings.Contains(out, "a.txt") {
		t.Fatalf("single file search failed: %q", out)
	}
}

func TestGlobPathAndHidden(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	write(t, dir, "a.go", "package a")
	write(t, dir, ".hidden.go", "package hidden")
	write(t, dir, "sub/b.go", "package b")

	out, err := run(t, registry, "glob", `{"pattern":"*.go","hidden":true}`, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, ".hidden.go") {
		t.Fatalf("hidden file missing: %q", out)
	}

	out, err = run(t, registry, "glob", `{"pattern":"*.go","path":"sub"}`, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sub/b.go") || strings.Contains(out, "a.go") {
		t.Fatalf("path scoping failed: %q", out)
	}
}

func TestEditReplaceAllAndCRLF(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	write(t, dir, "a.txt", "same\r\nsame\r\n")

	if _, err := run(t, registry, "edit", `{"path":"a.txt","oldString":"same","newString":"x","replaceAll":true}`, allowAll()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "x\r\nx\r\n" {
		t.Fatalf("unexpected content: %q", data)
	}

	write(t, dir, "b.txt", "a\nb\na\n")
	if _, err := run(t, registry, "edit", `{"path":"b.txt","oldString":"a","newString":"z"}`, allowAll()); err == nil || !strings.Contains(err.Error(), "expected exactly one") {
		t.Fatalf("want ambiguity error, got %v", err)
	}
}
