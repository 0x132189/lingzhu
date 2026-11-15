package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Patch applies a compact add/update/delete patch to one or more files.
type Patch struct {
	Dir string
}

func (p *Patch) Name() string { return "patch" }

func (p *Patch) Description() string {
	return "Use the `patch` tool to edit files. Your patch language is a stripped-down, file-oriented diff format designed to be easy to parse and safe to apply. You can think of it as a high-level envelope:\n\n" +
		"*** Begin Patch\n[ one or more file sections ]\n*** End Patch\n\n" +
		"Within that envelope, you get a sequence of file operations.\nYou MUST include a header to specify the action you are taking.\nEach operation starts with one of three headers:\n\n" +
		"*** Add File: <path> - create a new file. Every following line is a + line (the initial contents).\n" +
		"*** Delete File: <path> - remove an existing file. Nothing follows.\n" +
		"*** Update File: <path> - patch an existing file in place (optionally with a rename).\n\n" +
		"Example patch:\n\n```\n*** Begin Patch\n*** Add File: hello.txt\n+Hello world\n*** Update File: src/app.py\n*** Move to: src/main.py\n@@ def greet():\n-print(\"Hi\")\n+print(\"Hello, world!\")\n*** Delete File: obsolete.txt\n*** End Patch\n```\n\n" +
		"It is important to remember:\n\n- You must include a header with your intended action (Add/Delete/Update)\n- You must prefix new lines with `+` even when creating a new file"
}

func (p *Patch) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"patchText": {"type": "string", "description": "The full patch text describing add, update, and delete operations"}
		},
		"required": ["patchText"]
	}`)
}

type patchHunk struct {
	lines []string
}

type patchFile struct {
	op    string
	path  string
	move  string
	lines []string
	hunks []patchHunk
}

func (p *Patch) Run(ctx context.Context, call Call) (string, error) {
	var args struct {
		PatchText string `json:"patchText"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("patch: %w", err)
	}
	files, err := parsePatch(args.PatchText)
	if err != nil {
		return "", err
	}

	targets := make([]string, 0, len(files))
	for _, file := range files {
		abs, err := resolvePath(p.Dir, file.path)
		if err != nil {
			return "", fmt.Errorf("patch: %w", err)
		}
		if external(p.Dir, abs) {
			if err := call.Allow(ctx, "external_directory", []string{abs}, ""); err != nil {
				return "", err
			}
		}
		targets = append(targets, abs)
		if file.move != "" {
			move, err := resolvePath(p.Dir, file.move)
			if err != nil {
				return "", fmt.Errorf("patch: %w", err)
			}
			if external(p.Dir, move) {
				if err := call.Allow(ctx, "external_directory", []string{move}, ""); err != nil {
					return "", err
				}
			}
			targets = append(targets, move)
		}
	}
	if err := call.Allow(ctx, "edit", targets, ""); err != nil {
		return "", err
	}

	var applied []string
	var diffs strings.Builder
	var formatted []string
	var formatWarnings []string
	var written []string
	var changed []map[string]any
	for _, file := range files {
		path, _ := resolvePath(p.Dir, file.path)
		var before, after string
		switch file.op {
		case "add":
			if _, err := os.Stat(path); err == nil {
				return "", fmt.Errorf("patch: add %s: file already exists", relPath(p.Dir, path))
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return "", fmt.Errorf("patch: %w", err)
			}
			after = strings.Join(file.lines, "\n")
			if len(file.lines) > 0 {
				after += "\n"
			}
			if err := os.WriteFile(path, []byte(after), 0o644); err != nil {
				return "", fmt.Errorf("patch: %w", err)
			}
		case "delete":
			data, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("patch: %w", err)
			}
			before = string(data)
			if err := os.Remove(path); err != nil {
				return "", fmt.Errorf("patch: %w", err)
			}
		case "update":
			data, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("patch: %w", err)
			}
			before = string(data)
			updated, err := applyHunks(p.Dir, path, before, file.hunks)
			if err != nil {
				return "", err
			}
			after = updated
			if err := os.WriteFile(path, []byte(after), 0o644); err != nil {
				return "", fmt.Errorf("patch: %w", err)
			}
			if file.move != "" {
				move, _ := resolvePath(p.Dir, file.move)
				if err := os.MkdirAll(filepath.Dir(move), 0o755); err != nil {
					return "", fmt.Errorf("patch: %w", err)
				}
				if err := os.Rename(path, move); err != nil {
					return "", fmt.Errorf("patch: %w", err)
				}
				path = move
			}
		default:
			return "", fmt.Errorf("patch: unsupported operation %q", file.op)
		}
		if file.op != "delete" {
			if name, err := call.FormatFile(ctx, path); err != nil {
				formatWarnings = append(formatWarnings, relPath(p.Dir, path)+": "+err.Error())
			} else if name != "" {
				formatted = append(formatted, name)
				if data, err := os.ReadFile(path); err == nil {
					after = string(data)
				}
			}
		}
		if file.op != "delete" {
			written = append(written, path)
		}
		if before != "" || after != "" {
			patch := lineDiff(p.Dir, path, before, after)
			diffs.WriteString(patch)
			status := "modified"
			if file.op == "add" {
				status = "added"
			} else if file.op == "delete" {
				status = "deleted"
			}
			changed = append(changed, fileChange(path, patch, status))
		}
		label := "M"
		if file.op == "add" {
			label = "A"
		} else if file.op == "delete" {
			label = "D"
		}
		applied = append(applied, label+" "+relPath(p.Dir, path))
	}
	call.SetDiff(strings.TrimRight(diffs.String(), "\n"))
	call.SetMetadata("diagnostics", diagnosePaths(written))
	call.SetMetadata("files", changed)

	var b strings.Builder
	b.WriteString("Success. Updated the following files:")
	for _, item := range applied {
		b.WriteString("\n" + item)
	}
	if len(formatted) > 0 {
		b.WriteString("\nFormatted with: " + strings.Join(uniqueStrings(formatted), ", "))
	}
	for _, warning := range formatWarnings {
		b.WriteString("\nFormatter failed: " + warning)
	}
	return b.String(), nil
}

func uniqueStrings(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func parsePatch(text string) ([]patchFile, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "*** Begin Patch" {
		return nil, fmt.Errorf("patch: missing *** Begin Patch header")
	}
	var files []patchFile
	var current *patchFile
	var hunk *patchHunk
	flushHunk := func() {
		if current != nil && hunk != nil && len(hunk.lines) > 0 {
			current.hunks = append(current.hunks, *hunk)
		}
		hunk = nil
	}
	finishFile := func() {
		flushHunk()
		if current != nil {
			files = append(files, *current)
		}
		current = nil
	}

	for i := 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		switch {
		case line == "*** End Patch":
			finishFile()
			if len(files) == 0 {
				return nil, fmt.Errorf("patch: no file operations")
			}
			return files, nil
		case strings.HasPrefix(line, "*** Add File:"):
			finishFile()
			current = &patchFile{op: "add", path: strings.TrimSpace(strings.TrimPrefix(line, "*** Add File:"))}
		case strings.HasPrefix(line, "*** Delete File:"):
			finishFile()
			current = &patchFile{op: "delete", path: strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File:"))}
		case strings.HasPrefix(line, "*** Update File:"):
			finishFile()
			current = &patchFile{op: "update", path: strings.TrimSpace(strings.TrimPrefix(line, "*** Update File:"))}
		case strings.HasPrefix(line, "*** Move to:"):
			if current == nil || current.op != "update" {
				return nil, fmt.Errorf("patch: *** Move to without an update section")
			}
			current.move = strings.TrimSpace(strings.TrimPrefix(line, "*** Move to:"))
		case strings.HasPrefix(line, "@@"):
			if current == nil || current.op != "update" {
				return nil, fmt.Errorf("patch: hunk without an update section")
			}
			flushHunk()
			hunk = &patchHunk{}
		case line == "":
			// Blank lines inside sections are ignored.
		default:
			if current == nil {
				return nil, fmt.Errorf("patch: line outside a file section: %q", line)
			}
			switch current.op {
			case "add":
				if !strings.HasPrefix(line, "+") {
					return nil, fmt.Errorf("patch: add line must start with +: %q", line)
				}
				current.lines = append(current.lines, line[1:])
			case "update":
				if hunk == nil {
					hunk = &patchHunk{}
				}
				if line[0] != ' ' && line[0] != '+' && line[0] != '-' {
					return nil, fmt.Errorf("patch: update line must start with space, + or -: %q", line)
				}
				hunk.lines = append(hunk.lines, line)
			default:
				return nil, fmt.Errorf("patch: unexpected line in delete section: %q", line)
			}
		}
	}
	return nil, fmt.Errorf("patch: missing *** End Patch")
}

func applyHunks(dir, path, content string, hunks []patchHunk) (string, error) {
	eol := "\n"
	if strings.Contains(content, "\r\n") {
		eol = "\r\n"
	}
	view := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(view, "\n")

	for _, hunk := range hunks {
		var oldLines, newLines []string
		for _, line := range hunk.lines {
			switch line[0] {
			case ' ':
				oldLines = append(oldLines, line[1:])
				newLines = append(newLines, line[1:])
			case '-':
				oldLines = append(oldLines, line[1:])
			case '+':
				newLines = append(newLines, line[1:])
			}
		}
		if len(oldLines) == 0 {
			return "", fmt.Errorf("patch: %s: hunk has no context or removed lines", relPath(dir, path))
		}
		start, err := findLines(lines, oldLines)
		if err != nil {
			return "", fmt.Errorf("patch: %s: %w", relPath(dir, path), err)
		}
		lines = append(append(append([]string{}, lines[:start]...), newLines...), lines[start+len(oldLines):]...)
	}

	updated := strings.Join(lines, "\n")
	if eol == "\r\n" {
		updated = strings.ReplaceAll(updated, "\n", "\r\n")
	}
	return updated, nil
}

func findLines(lines, block []string) (int, error) {
	found := -1
	for i := 0; i+len(block) <= len(lines); i++ {
		ok := true
		for j := range block {
			if lines[i+j] != block[j] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if found >= 0 {
			return 0, fmt.Errorf("hunk matches more than one location; include more context")
		}
		found = i
	}
	if found < 0 {
		return 0, fmt.Errorf("hunk context not found")
	}
	return found, nil
}
