// Package prompt derives the system prompt for one request from configuration
// and the environment. The prompt is never persisted.
package prompt

import (
	_ "embed"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"lingzhu/internal/global"
	"lingzhu/internal/skill"
)

//go:embed prompts/gpt.txt
var gptPrompt string

//go:embed prompts/gpt-astra.txt
var gptAstraPrompt string

//go:embed prompts/anthropic.txt
var anthropicPrompt string

//go:embed prompts/kimi.txt
var kimiPrompt string

//go:embed prompts/trinity.txt
var trinityPrompt string

//go:embed prompts/meta.txt
var metaPrompt string

// defaultPrompt is the reference base prompt. The guidance placeholder is
// filled from the tools exposed for this request.
var defaultPrompt = strings.Join([]string{
	"You are an AI agent running in Lingzhu, a coding agent harness. Help the user accomplish their goals using the tools you have available.",
	"",
	"# Harness",
	"- Responses are rendered as GitHub-flavored Markdown.",
	"- `<system-reminder>` blocks are harness instructions, not user-authored content. Read and follow them.",
	"- Prefer parallelizing independent tool calls.",
	"${TOOL_GUIDANCE}",
	"",
	"# Communication",
	"- Use clear file paths when referring to files.",
	"- Keep responses clear and concise, and avoid unnecessary technical jargon.",
	"",
	"# Working in codebases",
	"- Keep changes consistent with the structure, naming, style, and patterns of the surrounding code.",
	"- Treat unfamiliar files or changes as potential user work and investigate before deleting or overwriting them.",
}, "\n") + "\n"

// referenceInstructions renders the available reference directories, matching
// the reference harness's <available_references> block.
func referenceInstructions(references []Reference) string {
	if len(references) == 0 {
		return ""
	}
	sorted := append([]Reference(nil), references...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var b strings.Builder
	b.WriteString("Project references provide additional directories that can be accessed when relevant.\n")
	b.WriteString("<available_references>\n")
	for _, reference := range sorted {
		if strings.TrimSpace(reference.Description) == "" {
			continue
		}
		b.WriteString("  <reference>\n")
		b.WriteString("    <name>" + reference.Name + "</name>\n")
		b.WriteString("    <path>" + reference.Path + "</path>\n")
		b.WriteString("    <description>" + reference.Description + "</description>\n")
		b.WriteString("  </reference>\n")
	}
	b.WriteString("</available_references>")
	if !strings.Contains(b.String(), "<reference>") {
		return ""
	}
	return b.String()
}

// MCPServer is one connected server's handshake guidance.
type MCPServer struct {
	Name         string
	Instructions string
}

// Reference is one configured reference directory, rendered as prompt guidance.
type Reference struct {
	Name        string
	Path        string
	Description string
}

// Context is everything Build needs beyond the process environment.
type Context struct {
	Workdir string
	Shell   []string
	Extra   string
	Skills  []skill.Summary
	// AgentSystem replaces the base harness prompt for an agent with its own
	// system prompt, matching the reference harness. Tool guidance is kept.
	AgentSystem string
	// MCP lists server guidance injected as <mcp_instructions>.
	MCP []MCPServer
	// References lists extra directories the model may consult.
	References []Reference
	// MCPCodemode reports that MCP tools are reachable only through execute.
	MCPCodemode bool
	// Instructions lists extra instruction files from configuration, resolved
	// from the working directory.
	Instructions []string
	// Tools lists the tool names exposed to the model for this request. Tool
	// guidance is derived from it so the prompt only documents what exists.
	Tools []string
	// Catalog is the rendered Code Mode tool listing. Empty means execute is
	// unavailable and the section is skipped.
	Catalog string
	// Model is the catalog model id and RequestModel the upstream id. They
	// choose the reference harness's model-specific prompt when ModelPrompts is
	// enabled; otherwise every model uses the unified base prompt.
	Model        string
	RequestModel string
	ModelName    string
	// ModelPrompts opts into the per-family prompt presets. The default is the
	// unified harness prompt, matching the reference v2 behavior.
	ModelPrompts bool
	Now          time.Time
}

// guidance renders the tool-specific rules that follow the harness section.
// Each rule documents a tool that is actually available.
func guidance(tools []string) []string {
	has := func(name string) bool {
		for _, tool := range tools {
			if tool == name {
				return true
			}
		}
		return false
	}
	var lines []string
	if has("shell") {
		lines = append(lines,
			"- Prefer dedicated tools over shell commands; fall back to the shell when a tool cannot do what you need.",
			"- Do not chain shell commands with separators like `echo \"====\";` or `printf '---'`; the output becomes noisy in a way that makes the user's side of the conversation worse.",
		)
	}
	if has("write") {
		lines = append(lines, "- Use the write tool to create files or completely replace their content. Prefer using the edit tool for targeted changes.")
	}
	if has("edit") {
		lines = append(lines, "- Use the edit tool for targeted changes to existing text files. It replaces the exact text in `oldString` with `newString`, and the values must differ. By default, `oldString` must occur exactly once. If it occurs multiple times, include more surrounding context to make it unique or set `replaceAll` to true to replace every occurrence.")
	}
	return lines
}

// Build assembles the model context the same way the reference harness does:
// one base prompt followed by instructions ordered from most to least shared,
// so the prompt-cache prefix stays reusable across sessions.
// Section is one instruction block with a stable key. Keys let the agent
// persist a per-session baseline and render only the sections that changed.
type Section struct {
	Key  string
	Text string
}

// Parts separates the stable harness prompt from the instruction blocks. The
// agent freezes the initial instruction values in the system prompt and sends
// later changes as synthetic messages, matching the reference prompt cache.
type Parts struct {
	Base     string
	Sections []Section
}

// String renders the parts in the same order and spacing as Build.
func (p Parts) String() string {
	var b strings.Builder
	b.WriteString(p.Base)
	b.WriteString("\n")
	for _, section := range p.Sections {
		if strings.TrimSpace(section.Text) == "" {
			continue
		}
		b.WriteString("\n\n")
		b.WriteString(section.Text)
	}
	b.WriteString("\n")
	return b.String()
}

// BuildParts renders the stable base and each instruction section separately.
func BuildParts(c Context) Parts {
	base := strings.TrimSpace(c.AgentSystem)
	if base == "" {
		base = strings.TrimRight(basePrompt(c), "\n")
	}
	parts := Parts{Base: base}

	if catalog := strings.TrimSpace(c.Catalog); catalog != "" {
		parts.Sections = append(parts.Sections, Section{Key: "codemode", Text: catalog})
	}
	if mcp := mcpInstructions(c); mcp != "" {
		parts.Sections = append(parts.Sections, Section{Key: "mcp", Text: mcp})
	}
	if references := referenceInstructions(c.References); references != "" {
		parts.Sections = append(parts.Sections, Section{Key: "references", Text: references})
	}

	var skills strings.Builder
	skills.WriteString("# Skills\n\n")
	skills.WriteString("Skills provide specialized instructions and workflows for specific tasks.\n")
	skills.WriteString("Use the skill tool to load a skill when a task matches its description.\n")
	skills.WriteString("The user may also invoke a skill directly. When that happens, its instructions appear in the conversation as a <skill_content> block, the same shape the skill tool returns. A skill that is already present this way does not need to be invoked again.\n")
	if len(c.Skills) == 0 {
		skills.WriteString("\nNo skills are currently available.")
	} else {
		skills.WriteString("\n<available_skills>\n")
		for _, item := range c.Skills {
			skills.WriteString("  <skill>\n")
			skills.WriteString("    <id>" + item.ID + "</id>\n")
			skills.WriteString("    <name>" + item.Name + "</name>\n")
			skills.WriteString("    <description>" + item.Description + "</description>\n")
			skills.WriteString("  </skill>\n")
		}
		skills.WriteString("</available_skills>")
	}
	parts.Sections = append(parts.Sections, Section{Key: "skills", Text: skills.String()})

	if instructions := Instructions(c.Workdir); instructions != "" {
		parts.Sections = append(parts.Sections, Section{Key: "agents", Text: instructions})
	}
	if instructions := ConfiguredInstructions(c.Instructions, c.Workdir); instructions != "" {
		parts.Sections = append(parts.Sections, Section{Key: "instructions", Text: instructions})
	}
	parts.Sections = append(parts.Sections, Section{Key: "date", Text: "Today's date: " + c.Now.Format("Mon Jan 2 2006")})
	parts.Sections = append(parts.Sections, Section{Key: "environment", Text: environment(c.Workdir)})
	if extra := strings.TrimSpace(c.Extra); extra != "" {
		parts.Sections = append(parts.Sections, Section{Key: "extra", Text: "# Additional instructions\n\n" + extra})
	}
	return parts
}

func Build(c Context) string {
	return BuildParts(c).String()
}

// basePrompt selects the reference harness prompt for the model family. GPT and
// Claude have their own instruction sets; Kimi, Trinity and Muse replace the
// base prompt entirely. The placeholder fills tool guidance where the template
// supports it.
func basePrompt(c Context) string {
	if !c.ModelPrompts {
		return renderPrompt(defaultPrompt, c.Tools)
	}
	id := strings.ToLower(c.Model + " " + c.RequestModel)
	switch {
	case strings.Contains(id, "gpt"):
		if strings.Contains(id, "astra") {
			return renderPrompt(gptAstraPrompt, c.Tools)
		}
		return renderPrompt(gptPrompt, c.Tools)
	case strings.Contains(id, "claude"):
		return renderPrompt(defaultPrompt, c.Tools) + "\n\n" + anthropicPrompt
	case strings.Contains(id, "kimi"):
		return kimiPrompt
	case strings.Contains(id, "trinity"):
		return trinityPrompt
	case strings.Contains(id, "muse"):
		name := c.ModelName
		if name == "" {
			name = c.Model
		}
		return strings.ReplaceAll(metaPrompt, "{{MODEL_NAME}}", name)
	}
	return renderPrompt(defaultPrompt, c.Tools)
}

func renderPrompt(template string, tools []string) string {
	return strings.ReplaceAll(template, "${TOOL_GUIDANCE}", strings.Join(guidance(tools), "\n"))
}

// environment renders the same block the reference harness adds to every
// request. The platform is the Node-style name agents already expect.
func environment(workdir string) string {
	root, git := repoRoot(workdir)
	return strings.Join([]string{
		"Here is some useful information about the environment you are running in:",
		"<env>",
		"  Working directory: " + workdir,
		"  Workspace root folder: " + root,
		"  Is directory a git repo: " + map[bool]string{true: "yes", false: "no"}[git],
		"  Platform: " + platform(),
		"  Prefer " + global.Tmp() + " over generic system temporary directories such as /tmp; it is pre-created and approved for external access.",
		"</env>",
	}, "\n")
}

// repoRoot finds the nearest git root at or above dir. Without git, the
// workspace root is the directory itself.
func repoRoot(dir string) (string, bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir, false
	}
	for {
		if info, statErr := os.Stat(filepath.Join(abs, ".git")); statErr == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return abs, true
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return dir, false
		}
		abs = parent
	}
}

func platform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return runtime.GOOS
	}
}

// mcpInstructions renders connected servers' own guidance. The text and shape
// follow the reference harness so models see the same envelope.
func mcpInstructions(c Context) string {
	if len(c.MCP) == 0 {
		return ""
	}
	lines := []string{"<mcp_instructions>"}
	for _, server := range c.MCP {
		lines = append(lines, `  <server name="`+server.Name+`">`)
		if c.MCPCodemode {
			lines = append(lines, "    Use tools from this server through `execute`: find them with `search({ namespace: \""+server.Name+"\" })`.")
		}
		for _, line := range strings.Split(server.Instructions, "\n") {
			lines = append(lines, "    "+line)
		}
		lines = append(lines, "  </server>")
	}
	lines = append(lines, "</mcp_instructions>")
	return strings.Join(lines, "\n")
}

// ConfiguredInstructions loads extra instruction files named in config. A
// relative path resolves from workdir; ~ expands to the user's home.
func ConfiguredInstructions(paths []string, workdir string) string {
	blocks := make([]string, 0, len(paths))
	for _, raw := range paths {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}
		if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
			if home, err := os.UserHomeDir(); err == nil {
				path = filepath.Join(home, path[2:])
			}
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(workdir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		blocks = append(blocks, "Instructions from: "+path+"\n"+text)
	}
	return strings.Join(blocks, "\n\n")
}

// Instructions renders every AGENTS.md from the workspace root down to dir,
// outermost first, so nested files override their parents. The combined text is
// capped at instructionsBudget bytes; anything beyond that is dropped and
// marked, so one oversized file cannot consume the whole context. An empty
// string means none was found.
func Instructions(dir string) string {
	paths := InstructionsPaths(dir)
	globalFile := filepath.Join(global.Config(), "AGENTS.md")
	if len(paths) == 0 {
		if data, err := os.ReadFile(globalFile); err != nil || strings.TrimSpace(string(data)) == "" {
			return ""
		}
	}
	blocks := make([]string, 0, len(paths)+1)
	// The user-level file comes first, then project files from the root down,
	// matching the reference harness's instruction order.
	if data, err := os.ReadFile(globalFile); err == nil {
		if text := strings.TrimSpace(string(data)); text != "" {
			blocks = append(blocks, "Instructions from: "+globalFile+"\n"+text)
		}
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		blocks = append(blocks, "Instructions from: "+path+"\n"+text)
	}
	return boundInstructions(blocks)
}

// boundInstructions joins the instruction blocks and truncates the result once
// it passes the budget, keeping whole blocks so no file is cut mid-line.
func boundInstructions(blocks []string) string {
	const marker = "\n\n(project instructions truncated to fit the context budget)"
	var b strings.Builder
	for _, block := range blocks {
		next := len(block)
		if b.Len() > 0 {
			next++
		}
		if b.Len()+next > instructionsBudget {
			if b.Len() == 0 {
				return block[:instructionsBudget] + marker
			}
			return b.String() + marker
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(block)
	}
	return b.String()
}

// Instructions returns the nearest AGENTS.md at or above dir, or "".
//
// Deprecated: use InstructionsPaths to include every ancestor file.
func InstructionsPath(dir string) string {
	paths := InstructionsPaths(dir)
	if len(paths) == 0 {
		return ""
	}
	return paths[len(paths)-1]
}

// instructionsBudget bounds the combined AGENTS.md text. A very large file
// anywhere up the tree would otherwise consume the whole context budget.
const instructionsBudget = 64 * 1024

// InstructionsPaths returns every AGENTS.md at or above dir, outermost first.
// The walk stops at the repository root when one is found, so a project never
// inherits unrelated files from directories above it.
func InstructionsPaths(dir string) []string {
	if dir == "" {
		return nil
	}
	path, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	var reversed []string
	for {
		candidate := filepath.Join(path, "AGENTS.md")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			reversed = append(reversed, candidate)
		}
		// A repository root is the natural top of the project, so nothing above
		// it belongs to this workspace.
		if info, err := os.Stat(filepath.Join(path, ".git")); err == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	paths := make([]string, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		paths = append(paths, reversed[i])
	}
	return paths
}
