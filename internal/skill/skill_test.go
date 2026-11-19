package skill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/permission"
	"lingzhu/internal/tool"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverSkills(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "demo", "SKILL.md"), `---
name: Demo Skill
description: "Does demo work"
---
# Demo
Run the demo.
`)
	writeFile(t, filepath.Join(root, "demo", "scripts", "run.sh"), "echo hi")
	writeFile(t, filepath.Join(root, "demo", ".hidden.txt"), "hidden")
	writeFile(t, filepath.Join(root, "loose.md"), `---
name: Loose
description: A loose skill
---
Loose body.
`)
	writeFile(t, filepath.Join(root, ".hidden", "SKILL.md"), "---\ndescription: hidden\n---\nhidden")
	writeFile(t, filepath.Join(root, "nested", "deep", "SKILL.md"), "---\ndescription: deep\n---\ndeep body")

	manager := Discover([]string{root}, t.Logf)
	list := manager.List()
	if len(list) != 3 {
		t.Fatalf("want 3 skills, got %+v", list)
	}

	demo, ok := manager.Get("demo")
	if !ok {
		t.Fatal("demo skill not found")
	}
	if demo.Name != "Demo Skill" || demo.Description != "Does demo work" || !strings.Contains(demo.Content, "Run the demo.") {
		t.Fatalf("unexpected demo skill: %+v", demo)
	}
	if len(demo.Files) != 2 {
		t.Fatalf("unexpected resource list: %+v", demo.Files)
	}
	foundScript := false
	foundHidden := false
	for _, file := range demo.Files {
		switch {
		case strings.HasSuffix(filepath.ToSlash(file), "scripts/run.sh"):
			foundScript = true
		case strings.HasSuffix(filepath.ToSlash(file), ".hidden.txt"):
			foundHidden = true
		}
	}
	if !foundScript || !foundHidden {
		t.Fatalf("expected the script and dot file resources: %+v", demo.Files)
	}
	loaded := demo.Loaded()
	for _, want := range []string{"<skill_content name=\"Demo Skill\">", "# Skill: Demo Skill", "Run the demo.", "Base directory", "<skill_files>"} {
		if !strings.Contains(loaded, want) {
			t.Fatalf("loaded skill missing %q: %s", want, loaded)
		}
	}
	if _, ok := manager.Get("hidden"); ok {
		t.Fatal("hidden skill should be skipped")
	}
	if _, ok := manager.Get("loose"); !ok {
		t.Fatal("top-level markdown skill missing")
	}
	if _, ok := manager.Get("deep"); !ok {
		t.Fatal("nested skill missing")
	}
}

func TestAllowedTools(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "demo", "SKILL.md"), `---
name: Demo
description: demo
allowed-tools:
  - read
  - grep
---
body
`)
	item, ok := Discover([]string{root}, t.Logf).Get("demo")
	if !ok {
		t.Fatal("skill not discovered")
	}
	if len(item.AllowedTools) != 2 || item.AllowedTools[0] != "read" || item.AllowedTools[1] != "grep" {
		t.Fatalf("unexpected allowed tools: %+v", item.AllowedTools)
	}
	if !strings.Contains(item.Loaded(), "Allowed tools for this skill: read, grep") {
		t.Fatalf("allowed tools missing from loaded skill: %s", item.Loaded())
	}
}

func TestSkillAutoinvokeFlags(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "auto", "SKILL.md"), "---\ndescription: auto\n---\nauto")
	writeFile(t, filepath.Join(root, "manual", "SKILL.md"), "---\ndescription: manual\ndisable-model-invocation: true\n---\nmanual")
	writeFile(t, filepath.Join(root, "meta", "SKILL.md"), "---\ndescription: meta\nmetadata:\n  lingzhu/autoinvoke: \"false\"\n---\nmeta")

	manager := Discover([]string{root}, t.Logf)
	summaries := manager.Summaries()
	if len(summaries) != 1 || summaries[0].ID != "auto" {
		t.Fatalf("unexpected prompt skills: %+v", summaries)
	}
	if len(manager.List()) != 3 {
		t.Fatalf("manual skills should still be loadable: %+v", manager.List())
	}
	manual, _ := manager.Get("manual")
	if manual.Autoinvoke {
		t.Fatal("manual skill should not auto-invoke")
	}
}

func TestFrontmatterYAMLShapes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "shapes", "SKILL.md"), `---
name: "Quoted Skill"
description: >
  A folded
  description.
allowed-tools: [read, "grep", bash]
metadata:
  lingzhu/autoinvoke: false
  owner:
    team: docs
  tags:
    - one
    - two
---
body
`)
	item, ok := Discover([]string{root}, t.Logf).Get("shapes")
	if !ok {
		t.Fatal("skill not discovered")
	}
	if item.Name != "Quoted Skill" || item.Description != "A folded description." {
		t.Fatalf("unexpected scalar values: %+v", item)
	}
	if len(item.AllowedTools) != 3 || item.AllowedTools[0] != "read" || item.AllowedTools[2] != "bash" {
		t.Fatalf("unexpected inline list: %+v", item.AllowedTools)
	}
	if item.Autoinvoke {
		t.Fatal("nested YAML boolean should disable auto-invoke")
	}
}

func TestLaterRootOverrides(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeFile(t, filepath.Join(first, "demo", "SKILL.md"), "---\nname: First\n---\none")
	writeFile(t, filepath.Join(second, "demo", "SKILL.md"), "---\nname: Second\n---\ntwo")

	manager := Discover([]string{first, second}, t.Logf)
	item, ok := manager.Get("demo")
	if !ok || item.Name != "Second" {
		t.Fatalf("later root should win: %+v", item)
	}
	if _, ok := manager.Get("missing"); ok {
		t.Fatal("unexpected missing skill")
	}
}

func TestConfigAcceptsBothForms(t *testing.T) {
	var list Config
	if err := json.Unmarshal([]byte(`["a","b","https://example.com/skills"]`), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Paths) != 2 {
		t.Fatalf("unexpected list config: %+v", list)
	}
	if len(list.URLs) != 1 || list.URLs[0] != "https://example.com/skills" {
		t.Fatalf("array URL was not split from paths: %+v", list)
	}
	var object Config
	if err := json.Unmarshal([]byte(`{"paths":["c","https://example.com/d"],"urls":["https://example.com"]}`), &object); err != nil {
		t.Fatal(err)
	}
	if len(object.Paths) != 1 || len(object.URLs) != 2 {
		t.Fatalf("unexpected object config: %+v", object)
	}
}

func TestSkillToolLoadsAndRejects(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "demo", "SKILL.md"), "---\nname: Demo\ndescription: demo\n---\nbody")
	manager := Discover([]string{root}, t.Logf)
	registry := tool.NewRegistry()
	registry.Add(NewTool(manager))
	allow := permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})

	metadata := map[string]any{}
	out, err := registry.Run(context.Background(), tool.Call{
		Name:     "skill",
		Input:    json.RawMessage(`{"id":"demo"}`),
		Perm:     allow,
		Metadata: metadata,
		OnAsk:    func(permission.Request) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "body") {
		t.Fatalf("unexpected skill output: %q", out)
	}
	if metadata["name"] != "demo" {
		t.Fatalf("unexpected skill metadata: %#v", metadata)
	}

	if _, err := registry.Run(context.Background(), tool.Call{Name: "skill", Input: json.RawMessage(`{"id":"missing"}`), Perm: allow}); err == nil {
		t.Fatal("want missing skill error")
	}

	deny := permission.New(permission.Rules{{Action: "skill", Resource: "*", Effect: permission.Deny}})
	if _, err := registry.Run(context.Background(), tool.Call{Name: "skill", Input: json.RawMessage(`{"id":"demo"}`), Perm: deny}); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("want permission denial, got %v", err)
	}
}

func TestReloadReplacesCatalog(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeFile(t, filepath.Join(first, "demo", "SKILL.md"), "---\ndescription: first\n---\none")
	manager := Discover([]string{first}, t.Logf)
	if item, ok := manager.Get("demo"); !ok || !strings.Contains(item.Content, "one") {
		t.Fatalf("unexpected first catalog: %+v", item)
	}
	writeFile(t, filepath.Join(second, "demo", "SKILL.md"), "---\ndescription: second\n---\ntwo")
	manager.Reload([]string{second}, t.Logf)
	if item, ok := manager.Get("demo"); !ok || !strings.Contains(item.Content, "two") {
		t.Fatalf("unexpected reloaded catalog: %+v", item)
	}
}

func TestFilterRemovesBlockedSkills(t *testing.T) {
	manager := &Manager{
		skills: map[string]Info{"a": {ID: "a"}, "b": {ID: "b"}},
		list:   []Info{{ID: "a"}, {ID: "b"}},
	}
	manager.Filter(func(id string) bool { return id != "b" })
	if len(manager.List()) != 1 || manager.List()[0].ID != "a" {
		t.Fatalf("unexpected filtered list: %+v", manager.List())
	}
	if _, ok := manager.Get("b"); ok {
		t.Fatal("blocked skill should be removed")
	}
}

func TestAncestorSkillRoots(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "nested", "work")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(parent, ".lingzhu", "skills", "repo", "SKILL.md"), "---\ndescription: repo\n---\nbody")

	wanted := filepath.Join(parent, ".lingzhu", "skills")
	found := false
	for _, root := range Roots(nil, "", child) {
		if filepath.Clean(root) == filepath.Clean(wanted) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ancestor skill root missing from %v", Roots(nil, "", child))
	}
	if _, ok := Discover([]string{wanted}, t.Logf).Get("repo"); !ok {
		t.Fatal("ancestor skill was not discovered")
	}
}

func TestFrontmatterSanitizesPlainValues(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "loose", "SKILL.md"), `---
name: [Draft] Guide
description: 10:30 lesson
---
body
`)
	item, ok := Discover([]string{root}, t.Logf).Get("loose")
	if !ok {
		t.Fatal("skill not discovered")
	}
	if item.Name != "[Draft] Guide" || item.Description != "10:30 lesson" {
		t.Fatalf("loose frontmatter was not recovered: %+v", item)
	}
}

func TestAutoinvokeMetadataWinsOverDisable(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "meta", "SKILL.md"), `---
description: meta
metadata:
  vendor/autoinvoke: true
disable-model-invocation: true
---
body
`)
	item, ok := Discover([]string{root}, t.Logf).Get("meta")
	if !ok {
		t.Fatal("skill not discovered")
	}
	if !item.Autoinvoke {
		t.Fatal("explicit metadata autoinvoke should win over disable-model-invocation")
	}
}

func TestSymlinkedSkillDirectory(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	writeFile(t, filepath.Join(target, "demo", "SKILL.md"), "---\ndescription: linked\n---\nbody")
	if err := os.Symlink(filepath.Join(target, "demo"), filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	item, ok := Discover([]string{root}, t.Logf).Get("linked")
	if !ok || item.Description != "linked" {
		t.Fatalf("symlinked skill was not discovered: %+v", item)
	}
}
