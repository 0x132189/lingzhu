package commands

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCommand(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverCommands(t *testing.T) {
	root := t.TempDir()
	writeCommand(t, root, "review.md", "---\ndescription: Review code\nagent: plan\nmodel: test/m\n---\nReview $ARGUMENTS.")
	writeCommand(t, root, "team/check.md", "Check $1 with $2.")

	discovered := Discover([]string{root}, map[string]Definition{
		"configured": {Description: "From config", Template: "Configured $ARGUMENTS"},
	}, nil)
	if len(discovered) != 3 {
		t.Fatalf("unexpected commands: %+v", discovered)
	}
	review := discovered["review"]
	if review.Description != "Review code" || review.Agent != "plan" || review.Model != "test/m" || review.Template != "Review $ARGUMENTS." {
		t.Fatalf("unexpected review command: %+v", review)
	}
	if discovered["team/check"].Template != "Check $1 with $2." {
		t.Fatalf("unexpected nested command: %+v", discovered["team/check"])
	}
	if discovered["configured"].Template != "Configured $ARGUMENTS" {
		t.Fatalf("unexpected config command: %+v", discovered["configured"])
	}
}

func TestRenderCommandArguments(t *testing.T) {
	if got := Render("Review $ARGUMENTS.", "src/auth.ts"); got != "Review src/auth.ts." {
		t.Fatalf("unexpected arguments render: %q", got)
	}
	if got := Render("Check $1. Focus on $2.", `src/auth.ts "error handling"`); got != "Check src/auth.ts. Focus on error handling." {
		t.Fatalf("unexpected positional render: %q", got)
	}
	if got := Render("Compare $1 with $2.", "api stable branch"); got != "Compare api with stable branch." {
		t.Fatalf("highest positional should consume the rest: %q", got)
	}
	if got := Render("Explain this code.", "src/cache.ts"); got != "Explain this code.\n\nsrc/cache.ts" {
		t.Fatalf("unexpected fallback render: %q", got)
	}
}

func TestRegistryExpand(t *testing.T) {
	registry := New(nil, map[string]Definition{
		"review":     {Template: "Review $ARGUMENTS."},
		"team/check": {Template: "Check $1."},
	})
	got, ok := registry.Expand("/review src/main.go")
	if !ok || got != "Review src/main.go." {
		t.Fatalf("unexpected review expansion: %q %v", got, ok)
	}
	got, ok = registry.Expand("/team/check src/main.go")
	if !ok || got != "Check src/main.go." {
		t.Fatalf("unexpected nested expansion: %q %v", got, ok)
	}
	if _, ok := registry.Expand("/missing x"); ok {
		t.Fatal("missing command should not expand")
	}
}
