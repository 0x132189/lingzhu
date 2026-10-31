// Package prompt derives the system prompt for one request from configuration
// and the environment. The prompt is never persisted.
package prompt

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// maxInstructions caps the project instructions copied into the prompt.
const maxInstructions = 8 * 1024

const rules = `# How you work
- Use tools to inspect the workspace instead of guessing file contents.
- Prefer edit over write_file when changing part of an existing file.
- Search before reading: use glob to locate files and grep to locate code.
- Run one focused command at a time and read its output before deciding the next step.
- Tool output and permission rejections are the user's answer; follow them instead of retrying blindly.
- Answer in the language the user writes in. Keep final answers short.`

// Context is everything Build needs beyond the process environment.
type Context struct {
	Workdir string
	Shell   []string
	Extra   string
	Now     time.Time
}

// Build assembles the system prompt: identity and rules, an environment
// snapshot, the nearest AGENTS.md, and the configured extra section.
func Build(c Context) string {
	var b strings.Builder
	b.WriteString("You are lingzhu, an AI coding agent running locally on the user's machine.\n\n")
	b.WriteString("Working directory: " + c.Workdir + "\n")
	b.WriteString("Platform: " + runtime.GOOS + "/" + runtime.GOARCH + "\n")
	b.WriteString("Shell: " + strings.Join(c.Shell, " ") + "\n")
	b.WriteString("Today: " + c.Now.Format("2006-01-02") + "\n\n")
	b.WriteString(rules)
	if instructions := Instructions(c.Workdir); instructions != "" {
		b.WriteString("\n\n# Project instructions\n\n")
		b.WriteString(instructions)
	}
	if extra := strings.TrimSpace(c.Extra); extra != "" {
		b.WriteString("\n\n# Additional instructions\n\n")
		b.WriteString(extra)
	}
	b.WriteString("\n")
	return b.String()
}

// Instructions returns the nearest AGENTS.md at or above dir, truncated to a
// bounded size. An empty string means none was found.
func Instructions(dir string) string {
	if dir == "" {
		return ""
	}
	path, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(path, "AGENTS.md")
		if data, err := os.ReadFile(candidate); err == nil {
			return truncate(strings.TrimSpace(string(data)))
		}
		parent := filepath.Dir(path)
		if parent == path {
			return ""
		}
		path = parent
	}
}

func truncate(text string) string {
	if len(text) <= maxInstructions {
		return text
	}
	return strings.ToValidUTF8(text[:maxInstructions], "") + "\n\n(project instructions truncated)"
}
