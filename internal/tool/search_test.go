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

	if _, err := run(t, registry, "edit", `{"path":"a.txt","old_string":"beta","new_string":"gamma"}`, allowAll()); err != nil {
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

	if _, err := run(t, registry, "edit", `{"path":"a.txt","old_string":"same","new_string":"x"}`, allowAll()); err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Fatalf("want ambiguity error, got %v", err)
	}
	if _, err := run(t, registry, "edit", `{"path":"a.txt","old_string":"absent","new_string":"x"}`, allowAll()); err == nil || !strings.Contains(err.Error(), "not found") {
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
	if !strings.Contains(out, "a.go:1: needle here") {
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
