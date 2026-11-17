package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/permission"
)

func writeAgent(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverNestedAgents(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, filepath.Join(root, "agent", "team"), "reviewer.md", `---
description: Nested reviewer
variant: high
---
Review.
`)
	discovered := Discover([]string{filepath.Join(root, "agent")}, t.Logf)
	reviewer, ok := discovered["team/reviewer"]
	if !ok {
		t.Fatalf("nested agent missing: %+v", discovered)
	}
	if reviewer.Model != "" || reviewer.Variant != "high" {
		t.Fatalf("variant was not parsed: %+v", reviewer)
	}
}

func TestDiscoverMarkdownAgents(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeAgent(t, first, "reviewer.md", `---
name: MD Reviewer
description: Reviews code changes
mode: primary
model: work/fast
steps: 3
permissions:
  - action: shell
    resource: "*"
    effect: deny
---
You review code carefully.
`)
	writeAgent(t, first, "notes.md", "just a note\n")
	writeAgent(t, first, "Bad Name.md", "---\ndescription: x\n---\nbody\n")
	writeAgent(t, first, "nodesc.md", "---\nname: No Description\n---\nbody\n")
	// A later root wins for the same id.
	writeAgent(t, second, "reviewer.md", `---
name: Second Reviewer
description: Overrides the earlier file
mode: subagent
---
Second prompt.
`)

	discovered := Discover([]string{first, second}, t.Logf)
	if len(discovered) != 1 {
		t.Fatalf("unexpected agents: %+v", discovered)
	}
	reviewer, ok := discovered["reviewer"]
	if !ok {
		t.Fatal("reviewer missing")
	}
	if reviewer.Name != "Second Reviewer" || reviewer.Description != "Overrides the earlier file" || reviewer.Mode != ModeSubagent {
		t.Fatalf("later root did not win: %+v", reviewer)
	}
	if reviewer.System != "Second prompt." {
		t.Fatalf("unexpected system prompt: %q", reviewer.System)
	}

	// Remove the override to inspect the full first definition.
	os.Remove(filepath.Join(second, "reviewer.md"))
	discovered = Discover([]string{first}, t.Logf)
	reviewer = discovered["reviewer"]
	if reviewer.Model != "work/fast" || reviewer.Steps != 3 || reviewer.System != "You review code carefully." {
		t.Fatalf("unexpected parsed fields: %+v", reviewer)
	}
	if len(reviewer.Permissions) != 1 || reviewer.Permissions[0].Action != "shell" || reviewer.Permissions[0].Effect != permission.Deny {
		t.Fatalf("unexpected permissions: %+v", reviewer.Permissions)
	}
}

func TestDiscoverInlinePermissionsAndQuotes(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "quoted.md", `---
name: "Quoted Agent"
description: 'Inline permissions'
permissions: [{"action":"edit","resource":"*","effect":"deny"}]
---
Body.
`)
	discovered := Discover([]string{root}, t.Logf)
	agent := discovered["quoted"]
	if agent.Name != "Quoted Agent" || agent.Description != "Inline permissions" {
		t.Fatalf("unexpected scalar parsing: %+v", agent)
	}
	if len(agent.Permissions) != 1 || agent.Permissions[0].Action != "edit" {
		t.Fatalf("inline permissions missing: %+v", agent.Permissions)
	}
}

func TestOverlayMarkdownOverConfig(t *testing.T) {
	cfg := Config{Agents: map[string]Definition{
		"reviewer": {
			Description: "config description",
			Mode:        "subagent",
			Permissions: permission.Rules{{Action: "read", Resource: "*", Effect: permission.Allow}},
		},
		"build": {Model: "work/fast"},
	}}
	markdown := map[string]Definition{
		"reviewer": {
			Description: "markdown description",
			Model:       "work/smart",
			System:      "Review prompt.",
			Permissions: permission.Rules{{Action: "shell", Resource: "*", Effect: permission.Deny}},
		},
		"newcomer": {
			Description: "new agent",
			Mode:        ModePrimary,
		},
	}
	merged := cfg.Overlay(markdown)
	reviewer := merged.Agents["reviewer"]
	if reviewer.Description != "markdown description" || reviewer.Model != "work/smart" || reviewer.Mode != "subagent" {
		t.Fatalf("unexpected merge: %+v", reviewer)
	}
	if len(reviewer.Permissions) != 2 || reviewer.Permissions[1].Action != "shell" {
		t.Fatalf("permissions not appended: %+v", reviewer.Permissions)
	}
	if merged.Agents["build"].Model != "work/fast" {
		t.Fatalf("unrelated config agent lost: %+v", merged.Agents["build"])
	}
	if merged.Agents["newcomer"].Mode != ModePrimary {
		t.Fatalf("markdown agent missing: %+v", merged.Agents["newcomer"])
	}

	registry := New(merged, "")
	profile, ok := registry.Get("reviewer")
	if !ok || profile.Description != "markdown description" || profile.Model != "work/smart" {
		t.Fatalf("registry did not resolve merged agent: %+v", profile)
	}
}

func TestMarkdownCanDisableBuiltin(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "general.md", `---
description: disabled on purpose
disabled: true
---
`)
	merged := Config{}.Overlay(Discover([]string{root}, t.Logf))
	registry := New(merged, "")
	if _, ok := registry.Get("general"); ok {
		t.Fatal("markdown disabled flag should remove the builtin")
	}
}

func TestRootsIncludeUserAndProjectDirectories(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	workdir := t.TempDir()
	roots := Roots(configPath, workdir)
	want := []string{
		filepath.Join(filepath.Dir(configPath), "agent"),
		filepath.Join(filepath.Dir(configPath), "agents"),
		filepath.Join(workdir, ".lingzhu", "agent"),
		filepath.Join(workdir, ".lingzhu", "agents"),
	}
	if len(roots) != len(want) {
		t.Fatalf("unexpected roots: %v", roots)
	}
	for i, path := range want {
		if !strings.EqualFold(roots[i], path) {
			t.Fatalf("root %d: want %s, got %s", i, path, roots[i])
		}
	}
}
