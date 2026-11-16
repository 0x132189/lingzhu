package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/global"
	"lingzhu/internal/skill"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lingzhu-prompt-config")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("LINGZHU_CONFIG_DIR", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestBuildIncludesEnvironmentAndInstructions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("- Write English comments"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	out := Build(Context{
		Workdir: dir,
		Shell:   []string{"bash", "-lc"},
		Extra:   "Always run go test.",
		Now:     now,
	})
	for _, want := range []string{dir, "Workspace root folder:", "Is directory a git repo:", "Oct 31 2026", "Write English comments", "Always run go test."} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt missing %q:\n%s", want, out)
		}
	}
}

func TestBuildWrapsEnvironment(t *testing.T) {
	out := Build(Context{Workdir: t.TempDir(), Shell: []string{"pwsh", "-c"}, Now: time.Now()})
	for _, want := range []string{"# Harness", "<env>", "Working directory:", "Today's date:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt missing %q:\n%s", want, out)
		}
	}
}

func TestBuildToolGuidanceFollowsTools(t *testing.T) {
	dir := t.TempDir()
	withShell := Build(Context{Workdir: dir, Tools: []string{"shell", "read"}, Now: time.Now()})
	if !strings.Contains(withShell, "Do not chain shell commands") {
		t.Fatalf("shell guidance missing:\n%s", withShell)
	}
	if strings.Contains(withShell, "Use the edit tool for targeted changes") {
		t.Fatalf("edit guidance should be absent without the edit tool:\n%s", withShell)
	}

	withEdit := Build(Context{Workdir: dir, Tools: []string{"edit"}, Now: time.Now()})
	if !strings.Contains(withEdit, "Use the edit tool for targeted changes") {
		t.Fatalf("edit guidance missing:\n%s", withEdit)
	}
	if strings.Contains(withEdit, "Do not chain shell commands") {
		t.Fatalf("shell guidance should be absent without the shell tool:\n%s", withEdit)
	}
}

func TestBuildListsSkills(t *testing.T) {
	out := Build(Context{
		Workdir: t.TempDir(),
		Shell:   []string{"bash", "-lc"},
		Skills: []skill.Summary{
			{ID: "demo", Name: "Demo Skill", Description: "Does demo work"},
		},
		Now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
	})
	for _, want := range []string{"# Skills", "<available_skills>", "<id>demo</id>", "Does demo work", "skill tool"} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt missing %q:\n%s", want, out)
		}
	}
}

func TestBuildMentionsDirectSkillInvocation(t *testing.T) {
	out := Build(Context{Workdir: t.TempDir(), Now: time.Now()})
	for _, want := range []string{"<skill_content>", "No skills are currently available."} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt missing %q:\n%s", want, out)
		}
	}
}

func TestInstructionsIncludeGlobalFileFirst(t *testing.T) {
	dir := t.TempDir()
	globalFile := filepath.Join(global.Config(), "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(globalFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalFile, []byte("global rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(globalFile)
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("project rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Instructions(dir)
	globalAt := strings.Index(got, "global rules")
	projectAt := strings.Index(got, "project rules")
	if globalAt < 0 || projectAt < 0 || globalAt > projectAt {
		t.Fatalf("global instructions must come first: %q", got)
	}
}

func TestInstructionsIncludeEveryAncestor(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "AGENTS.md"), []byte("middle rules"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := Instructions(nested)
	rootAt := strings.Index(got, "root rules")
	middleAt := strings.Index(got, "middle rules")
	if rootAt < 0 || middleAt < 0 {
		t.Fatalf("want both files, got %q", got)
	}
	if rootAt > middleAt {
		t.Fatalf("outermost file must come first, got %q", got)
	}
	if !strings.Contains(got, "Instructions from: ") {
		t.Fatalf("want labelled blocks, got %q", got)
	}
}

func TestInstructionsPathsOrder(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	outer := filepath.Join(root, "AGENTS.md")
	inner := filepath.Join(nested, "AGENTS.md")
	for _, path := range []string{outer, inner} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths := InstructionsPaths(nested)
	if len(paths) < 2 {
		t.Fatalf("want at least two paths, got %v", paths)
	}
	if paths[0] != outer || paths[len(paths)-1] != inner {
		t.Fatalf("want outermost first and nearest last, got %v", paths)
	}
}

func TestInstructionsMissingIsEmpty(t *testing.T) {
	if got := Instructions(t.TempDir()); got != "" {
		t.Fatalf("want empty instructions, got %q", got)
	}
}

func TestInstructionsKeepsFullFile(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("a", 32*1024)
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Instructions(dir)
	if !strings.Contains(got, big) || strings.Contains(got, "truncated to fit the context budget") {
		t.Fatalf("project instructions should not be truncated, got %d bytes", len(got))
	}
}

func TestInstructionsTruncatesOversizedFile(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("a", instructionsBudget*2)
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Instructions(dir)
	if !strings.Contains(got, "truncated to fit the context budget") {
		t.Fatalf("an oversized file should be marked as truncated, got %d bytes", len(got))
	}
	if len(got) > instructionsBudget+200 {
		t.Fatalf("instructions should stay near the budget, got %d bytes", len(got))
	}
}

func TestInstructionsStopAtRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "pkg", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := InstructionsPaths(nested)
	if len(paths) != 1 || filepath.Base(filepath.Dir(paths[0])) != filepath.Base(root) {
		t.Fatalf("the walk should stop at the repository root, got %+v", paths)
	}
}

func TestBuildIncludesConfiguredInstructions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "extra.md")
	if err := os.WriteFile(path, []byte("extra rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := Build(Context{Workdir: dir, Instructions: []string{"extra.md"}, Now: time.Now()})
	if !strings.Contains(out, "extra rules") || !strings.Contains(out, "Instructions from: "+path) {
		t.Fatalf("configured instructions missing:\n%s", out)
	}
}

func TestBuildUsesUnifiedPromptByDefault(t *testing.T) {
	for _, model := range []string{"gpt-5", "claude-sonnet", "kimi-k3", "trinity"} {
		out := Build(Context{
			Workdir: t.TempDir(),
			Model:   model,
			Tools:   []string{"shell"},
			Now:     time.Now(),
		})
		if !strings.Contains(out, "You are an AI agent running in Lingzhu") {
			t.Fatalf("%s should use the unified prompt by default:\n%s", model, out)
		}
		if !strings.Contains(out, "Do not chain shell commands") {
			t.Fatalf("%s unified prompt missing tool guidance:\n%s", model, out)
		}
	}
}

func TestBuildSelectsModelSpecificPrompt(t *testing.T) {
	cases := []struct {
		model, request, name, want string
	}{
		{"gpt-5", "gpt-5", "GPT-5", "Autonomy"},
		{"gpt-5-astra", "gpt-5-astra", "Astra", "Delegation"},
		{"claude-sonnet", "claude-sonnet", "Claude", "# Code comments"},
		{"kimi-k3", "kimi-k3", "Kimi", "interactive general AI agent"},
		{"trinity", "trinity", "Trinity", "# Tone and style"},
		{"muse-1", "muse-1", "Test Model", "powered by Test Model"},
	}
	for _, tc := range cases {
		out := Build(Context{
			Workdir:      t.TempDir(),
			Model:        tc.model,
			RequestModel: tc.request,
			ModelName:    tc.name,
			ModelPrompts: true,
			Tools:        []string{"shell"},
			Now:          time.Now(),
		})
		if !strings.Contains(out, tc.want) {
			t.Fatalf("%s prompt missing %q:\n%s", tc.model, tc.want, out)
		}
	}
	if out := Build(Context{Workdir: t.TempDir(), Model: "claude-sonnet", ModelPrompts: true, Tools: []string{"shell"}, Now: time.Now()}); !strings.Contains(out, "You are an AI agent running in Lingzhu") || !strings.Contains(out, "Do not chain shell commands") {
		t.Fatalf("Claude prompt should append to the base with tool guidance:\n%s", out)
	}
}

func TestModelPresetsRenderToolGuidance(t *testing.T) {
	for _, model := range []string{"gpt-5", "gpt-5-astra", "claude-sonnet"} {
		out := Build(Context{
			Workdir:      t.TempDir(),
			Model:        model,
			RequestModel: model,
			ModelPrompts: true,
			Tools:        []string{"shell"},
			Now:          time.Now(),
		})
		if strings.Contains(out, "${") {
			t.Fatalf("%s left a placeholder unrendered:\n%s", model, out)
		}
		if !strings.Contains(out, "Do not chain shell commands") {
			t.Fatalf("%s preset missing tool guidance:\n%s", model, out)
		}
	}
}

func TestBuildPartsSeparatesBaseAndInstructionSections(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("project rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	parts := BuildParts(Context{
		Workdir: dir,
		Model:   "gpt-5",
		Now:     time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if !strings.Contains(parts.Base, "You are an AI agent running in Lingzhu") {
		t.Fatalf("base prompt missing: %q", parts.Base)
	}
	keys := map[string]string{}
	for _, section := range parts.Sections {
		keys[section.Key] = section.Text
	}
	for _, key := range []string{"skills", "agents", "date", "environment"} {
		if keys[key] == "" {
			t.Fatalf("instruction section %q missing: %+v", key, parts.Sections)
		}
	}
	if !strings.Contains(keys["agents"], "project rules") {
		t.Fatalf("AGENTS section missing: %q", keys["agents"])
	}
	if !strings.Contains(parts.String(), "project rules") {
		t.Fatal("rendered parts should include the AGENTS section")
	}
}

func TestBuildRendersReferenceGuidance(t *testing.T) {
	out := Build(Context{
		Workdir: t.TempDir(),
		References: []Reference{
			{Name: "zeta", Path: "/repos/zeta", Description: "Second repository"},
			{Name: "alpha", Path: "/repos/alpha", Description: "First repository"},
			{Name: "hidden", Path: "/repos/hidden"},
		},
		Now: time.Now(),
	})
	for _, want := range []string{
		"<available_references>",
		"    <name>alpha</name>",
		"    <path>/repos/alpha</path>",
		"    <description>First repository</description>",
		"</available_references>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "<name>alpha</name>") > strings.Index(out, "<name>zeta</name>") {
		t.Fatal("references should be sorted by name")
	}
	if strings.Contains(out, "<name>hidden</name>") {
		t.Fatal("a reference without a description must not be listed")
	}
}

func TestBuildInjectsMCPInstructions(t *testing.T) {
	out := Build(Context{
		Workdir: t.TempDir(),
		MCP: []MCPServer{{
			Name:         "figma",
			Instructions: "Use the design tools.\nPrefer vectors.",
		}},
		MCPCodemode: true,
		Now:         time.Now(),
	})
	for _, want := range []string{
		"<mcp_instructions>",
		`<server name="figma">`,
		"Use tools from this server through `execute` under `tools[\"figma\"]`.",
		"    Use the design tools.",
		"    Prefer vectors.",
		"</mcp_instructions>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(Build(Context{Workdir: t.TempDir(), Now: time.Now()}), "mcp_instructions") {
		t.Fatal("empty MCP list must not render the section")
	}
}

func TestAgentSystemReplacesModelSpecificBase(t *testing.T) {
	out := Build(Context{
		Workdir:      t.TempDir(),
		AgentSystem:  "You are a custom reviewer.",
		Model:        "gpt-5",
		RequestModel: "gpt-5",
		Tools:        []string{"shell", "edit"},
		Now:          time.Now(),
	})
	if !strings.Contains(out, "You are a custom reviewer.") {
		t.Fatalf("agent system missing:\n%s", out)
	}
	if strings.Contains(out, "Do not infer authorization") || strings.Contains(out, "You are an AI agent running in Lingzhu") {
		t.Fatalf("model base prompt was not skipped:\n%s", out)
	}
	if strings.Contains(out, "# Tool guidance") || strings.Contains(out, "Prefer dedicated tools") {
		t.Fatalf("custom agent system must replace the whole prompt:\n%s", out)
	}
}
