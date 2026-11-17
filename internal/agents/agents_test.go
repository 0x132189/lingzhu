package agents

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/global"
	"lingzhu/internal/permission"
)

func TestBuiltinsAndDefault(t *testing.T) {
	registry := New(Config{}, "")
	if registry.Default().ID != "build" {
		t.Fatalf("unexpected default: %+v", registry.Default())
	}
	primary := registry.Primary()
	if len(primary) != 2 || primary[0].ID != "build" || primary[1].ID != "plan" {
		t.Fatalf("unexpected primary agents: %+v", primary)
	}
	subagents := registry.Subagents()
	if len(subagents) != 2 || subagents[0].ID != "explore" || subagents[1].ID != "general" {
		t.Fatalf("unexpected subagents: %+v", subagents)
	}
	if profile, ok := registry.Get("plan"); !ok || profile.Mode != ModePrimary || len(profile.Permissions) == 0 {
		t.Fatalf("unexpected plan profile: %+v", profile)
	}
	if profile, ok := registry.Get("explore"); !ok || profile.System == "" {
		t.Fatalf("unexpected explore profile: %+v", profile)
	}
}

func TestConfigOverridesAndDisables(t *testing.T) {
	registry := New(Config{Agents: map[string]Definition{
		"build": {
			Description: "Custom build",
			Model:       "work/fast",
			Steps:       3,
			Permissions: permission.Rules{{Action: "shell", Resource: "*", Effect: permission.Deny}},
		},
		"reviewer": {
			Name:        "Reviewer",
			Description: "Reads code",
			Mode:        "subagent",
			System:      "REVIEW_MARKER",
		},
		"general": {Disabled: true},
	}}, "")
	build := registry.Default()
	if build.Description != "Custom build" || build.Model != "work/fast" || build.Steps != 3 {
		t.Fatalf("builtin override failed: %+v", build)
	}
	if len(build.Permissions) != 1 || build.Permissions[0].Action != "shell" {
		t.Fatalf("permissions not merged: %+v", build.Permissions)
	}
	reviewer, ok := registry.Get("reviewer")
	if !ok || reviewer.Name != "Reviewer" || reviewer.Mode != ModeSubagent || reviewer.System != "REVIEW_MARKER" {
		t.Fatalf("custom agent missing: %+v", reviewer)
	}
	if _, ok := registry.Get("general"); ok {
		t.Fatal("disabled agent should be removed")
	}
}

func TestDefaultAgentSelection(t *testing.T) {
	registry := New(Config{}, "plan")
	if registry.Default().ID != "plan" {
		t.Fatalf("configured default ignored: %+v", registry.Default())
	}
	registry = New(Config{}, "explore")
	if registry.Default().ID != "build" {
		t.Fatalf("subagent cannot be the default: %+v", registry.Default())
	}
}

func TestHiddenAndModes(t *testing.T) {
	registry := New(Config{Agents: map[string]Definition{
		"hidden": {Hidden: true},
		"both":   {Mode: "all"},
	}}, "")
	if len(registry.Primary()) != 3 {
		t.Fatalf("hidden or mode filtering failed: %+v", registry.Primary())
	}
	for _, profile := range registry.Primary() {
		if profile.ID == "hidden" {
			t.Fatal("hidden agent is selectable")
		}
	}
	if len(registry.Subagents()) != 3 {
		t.Fatalf("mode=all should be a subagent too: %+v", registry.Subagents())
	}
}

func TestPartialOverrideKeepsBuiltinHidden(t *testing.T) {
	// A config entry that never mentions hidden must not un-hide a built-in
	// hidden agent; only an explicit hidden value may change it.
	registry := New(Config{Agents: map[string]Definition{
		"title": {Description: "renamed"},
	}}, "")
	title, ok := registry.Get("title")
	if !ok {
		t.Fatal("title profile missing")
	}
	if !title.Hidden {
		t.Fatal("a partial override must keep the built-in hidden flag")
	}
	shown := New(Config{Agents: map[string]Definition{
		"title": {HiddenSet: true, Hidden: false},
	}}, "")
	if profile, ok := shown.Get("title"); !ok || profile.Hidden {
		t.Fatal("an explicit hidden=false should un-hide the agent")
	}
}

func TestPartialOverrideKeepsBuiltinMode(t *testing.T) {
	registry := New(Config{Agents: map[string]Definition{
		"explore": {Description: "renamed"},
	}}, "")
	explore, ok := registry.Get("explore")
	if !ok {
		t.Fatal("explore profile missing")
	}
	if explore.Mode != ModeSubagent {
		t.Fatalf("a partial override must keep the built-in mode, got %q", explore.Mode)
	}
}

func TestExploreProfileIsReadOnly(t *testing.T) {
	registry := New(Config{}, "")
	explore, ok := registry.Get("explore")
	if !ok {
		t.Fatal("explore profile missing")
	}
	rules := append(permission.Default(), explore.Permissions...)
	if got := rules.EvaluateAll("edit", []string{"a.txt"}); got != permission.Deny {
		t.Fatalf("explore should deny edit, got %s", got)
	}
	if got := rules.EvaluateAll("shell", []string{"git status"}); got != permission.Allow {
		t.Fatalf("explore should allow read-only shell, got %s", got)
	}
	if got := rules.EvaluateAll("read", []string{"src/a.go"}); got != permission.Allow {
		t.Fatalf("explore should allow read, got %s", got)
	}
}

func TestAllowsToolHonorsPermissions(t *testing.T) {
	denied, _ := New(Config{Agents: map[string]Definition{
		"build": {Permissions: permission.Rules{{Action: "shell", Resource: "*", Effect: permission.Deny}}},
	}}, "").Get("build")
	if denied.AllowsTool("shell") || !denied.AllowsTool("read") {
		t.Fatalf("a denied action must not hide unrelated actions: %+v", denied.Permissions)
	}
	if denied.Permissions.WhollyDenies("read") {
		t.Fatalf("unrelated actions must stay available: %+v", denied.Permissions)
	}
	if !(Profile{}).AllowsTool("anything") {
		t.Fatal("an empty ruleset should allow everything")
	}
	edit, _ := New(Config{Agents: map[string]Definition{
		"build": {Permissions: permission.Rules{{Action: "edit", Resource: "*", Effect: permission.Deny}}},
	}}, "").Get("build")
	if edit.AllowsTool("patch") || edit.AllowsTool("write") {
		t.Fatal("write and patch follow the edit action")
	}
}

func TestRequestAndColorConfig(t *testing.T) {
	registry := New(Config{Agents: map[string]Definition{
		"custom": {
			Name:  "Custom",
			Color: "#aabbcc",
			Request: &RequestConfig{
				Headers:  map[string]string{"x-test": "1"},
				Settings: map[string]any{"reasoning_effort": "high"},
				Body:     map[string]any{"temperature": 0.2},
			},
		},
	}}, "")
	profile, ok := registry.Get("custom")
	if !ok {
		t.Fatal("custom profile missing")
	}
	if profile.Color != "#aabbcc" {
		t.Fatalf("color = %q", profile.Color)
	}
	if profile.Headers["x-test"] != "1" {
		t.Fatalf("headers = %+v", profile.Headers)
	}
	if profile.Options["reasoning_effort"] != "high" || profile.Options["temperature"] != 0.2 {
		t.Fatalf("options = %+v", profile.Options)
	}
}

func TestModelRefAcceptsHashVariant(t *testing.T) {
	if got := NormalizeModelRef("openai/gpt-5#high"); got != "openai/gpt-5::high" {
		t.Fatalf("hash variant was not normalized: %q", got)
	}
	if got := WithVariant("openai/gpt-5::low", "high"); got != "openai/gpt-5::high" {
		t.Fatalf("variant was not replaced: %q", got)
	}
	providerID, model, err := ValidateModel("openai/gpt-5#high")
	if err != nil || providerID != "openai" || model != "gpt-5::high" {
		t.Fatalf("unexpected model split: %q %q %v", providerID, model, err)
	}
}

func TestValidateModel(t *testing.T) {
	providerID, model, err := ValidateModel("work/fast")
	if err != nil || providerID != "work" || model != "fast" {
		t.Fatalf("unexpected parse: %q %q %v", providerID, model, err)
	}
	for _, value := range []string{"", "fast", "/fast", "work/"} {
		if _, _, err := ValidateModel(value); err == nil {
			t.Fatalf("want error for %q", value)
		}
	}
}

func TestDefinitionJSON(t *testing.T) {
	var cfg Config
	body := `{"demo":{"name":"Demo","mode":"primary","model":"work/fast","permissions":[{"action":"edit","resource":"*","effect":"deny"}]}}`
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	def := cfg.Agents["demo"]
	if def.Name != "Demo" || def.Mode != "primary" || len(def.Permissions) != 1 || def.Permissions[0].Effect != permission.Deny {
		t.Fatalf("unexpected definition: %+v", def)
	}
}

func TestPlanAgentUsesPlanDirectory(t *testing.T) {
	registry := New(Config{}, "")
	plan, ok := registry.Get("plan")
	if !ok {
		t.Fatal("plan agent missing")
	}
	dir := filepath.Join(global.Config(), "plan")
	if !strings.Contains(plan.System, dir) {
		t.Fatalf("plan prompt must name its directory: %s", plan.System)
	}
	if got := plan.Permissions.Evaluate("edit", filepath.Join(dir, "plan.md")); got != permission.Allow {
		t.Fatalf("plan files should be writable, got %s", got)
	}
	if got := plan.Permissions.Evaluate("edit", filepath.Join(t.TempDir(), "other.md")); got != permission.Deny {
		t.Fatalf("other files should stay denied, got %s", got)
	}
}

func TestAgentHeadersAndHiddenSummary(t *testing.T) {
	registry := New(Config{Agents: map[string]Definition{
		"build": {Headers: map[string]string{"X-Agent": "demo"}},
	}}, "")
	build, ok := registry.Get("build")
	if !ok || build.Headers["X-Agent"] != "demo" {
		t.Fatalf("agent headers not loaded: %+v", build)
	}
	summary, ok := registry.Get("summary")
	if !ok || !summary.Hidden || !strings.Contains(summary.System, "pull request description") {
		t.Fatalf("hidden summary agent missing: %+v", summary)
	}
	for _, profile := range registry.Primary() {
		if profile.ID == "summary" {
			t.Fatal("summary agent must stay hidden from the selector")
		}
	}
}

func TestSubagentProfilePermissions(t *testing.T) {
	registry := New(Config{}, "")
	general, _ := registry.Get("general")
	if general.Permissions.Evaluate("question", "*") != permission.Deny || general.Permissions.Evaluate("subagent", "*") != permission.Deny {
		t.Fatalf("general subagent permissions drifted: %+v", general.Permissions)
	}
	explore, _ := registry.Get("explore")
	if explore.Permissions.Evaluate("edit", "*") != permission.Deny ||
		explore.Permissions.Evaluate("read", "*") != permission.Allow ||
		explore.Permissions.Evaluate("grep", "*") != permission.Allow ||
		explore.Permissions.Evaluate("websearch", "*") != permission.Allow ||
		explore.Permissions.Evaluate("subagent", "*") != permission.Deny {
		t.Fatalf("explore subagent permissions drifted: %+v", explore.Permissions)
	}
}

func TestHiddenCompactionAgent(t *testing.T) {
	registry := New(Config{}, "")
	compaction, ok := registry.Get("compaction")
	if !ok || !compaction.Hidden || compaction.Mode != ModePrimary {
		t.Fatalf("hidden compaction agent missing: %+v", compaction)
	}
	for _, profile := range registry.Primary() {
		if profile.ID == "compaction" {
			t.Fatal("compaction agent must stay hidden from the selector")
		}
	}
}
