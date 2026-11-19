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
	if len(demo.Files) != 1 || !strings.HasSuffix(filepath.ToSlash(demo.Files[0]), "scripts/run.sh") {
		t.Fatalf("unexpected resource list: %+v", demo.Files)
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
	if err := json.Unmarshal([]byte(`["a","b"]`), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Paths) != 2 {
		t.Fatalf("unexpected list config: %+v", list)
	}
	var object Config
	if err := json.Unmarshal([]byte(`{"paths":["c"],"urls":["https://example.com"]}`), &object); err != nil {
		t.Fatal(err)
	}
	if len(object.Paths) != 1 || len(object.URLs) != 1 {
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
