package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	maxMatches   = 200
	maxFileBytes = 2 * 1024 * 1024
	maxLineRunes = 400
)

// skipDirs are never walked: they hold no project source worth searching.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".venv":        true,
	"__pycache__":  true,
	"dist":         true,
	"build":        true,
}

// Glob finds files by pattern, newest first.
type Glob struct {
	Dir string
}

func (g *Glob) Name() string { return "glob" }

func (g *Glob) Description() string {
	return "Search file paths using a glob pattern (examples: \"**/*.ts\", \"src/**/*.tsx\")."
}

func (g *Glob) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Glob pattern to match files against"},
			"path": {"type": "string", "description": "Directory to search. Defaults to the current working directory."},
			"hidden": {"type": "boolean", "description": "Include hidden files and directories (default: false)."},
			"limit": {"type": "integer", "description": "Maximum number of matching files to return (default: 100)"}
		},
		"required": ["pattern"]
	}`)
}

func (g *Glob) Run(ctx context.Context, call Call) (string, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Hidden  bool   `json:"hidden"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	pattern := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(args.Pattern)), "./")
	if pattern == "" {
		return "", fmt.Errorf("glob: pattern is empty")
	}
	root, err := call.Resolve(ctx, "glob", g.Dir, defaultString(args.Path, "."))
	if err != nil {
		return "", err
	}
	limit := clampLimit(args.Limit, 100)
	if limit > maxMatches {
		limit = maxMatches
	}

	type hit struct {
		rel   string
		mtime time.Time
	}
	var hits []hit
	err = walk(root, args.Hidden, func(path, rel string, info os.FileInfo) error {
		if matchPattern(pattern, rel) {
			hits = append(hits, hit{rel: relPath(g.Dir, path), mtime: info.ModTime()})
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].mtime.After(hits[j].mtime) })
	total := len(hits)
	call.SetMetadata("count", total)
	if len(hits) == 0 {
		return "No files found", nil
	}
	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}

	var b strings.Builder
	for _, h := range hits {
		b.WriteString(h.rel + "\n")
	}
	if truncated {
		fmt.Fprintf(&b, "\n(Results are truncated: showing first %d results. Consider using a more specific path or pattern.)", limit)
	}
	return truncate(strings.TrimRight(b.String(), "\n")), nil
}

// Grep searches file contents with a regular expression or literal text.
type Grep struct {
	Dir string
}

func (g *Grep) Name() string { return "grep" }

func (g *Grep) Description() string {
	return "Search file contents using ripgrep's regular expression syntax or literal text matching. Use it to locate specific code, symbols, or text patterns, and narrow searches with `path` or `include`. Returns matching file paths, line numbers, and line previews."
}

func (g *Grep) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Regular expression or literal text to match in file contents."},
			"path": {"type": "string", "description": "File or directory to search. Defaults to the current working directory."},
			"include": {"type": "string", "description": "Glob pattern to filter files (for example, \"*.js\" or \"*.{ts,tsx}\")"},
			"literal": {"type": "boolean", "description": "Treat pattern as exact text instead of a regular expression (default: false)."},
			"caseSensitive": {"type": "boolean", "description": "Use case-sensitive matching (default: true)."},
			"limit": {"type": "integer", "description": "Maximum number of matching lines to return (default: 100)"}
		},
		"required": ["pattern"]
	}`)
}

func (g *Grep) Run(ctx context.Context, call Call) (string, error) {
	var args struct {
		Pattern       string `json:"pattern"`
		Path          string `json:"path"`
		Include       string `json:"include"`
		Literal       bool   `json:"literal"`
		CaseSensitive *bool  `json:"caseSensitive"`
		Limit         int    `json:"limit"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return "", fmt.Errorf("grep: pattern must not be empty")
	}
	pattern := args.Pattern
	if args.Literal {
		pattern = regexp.QuoteMeta(pattern)
	}
	if args.CaseSensitive != nil && !*args.CaseSensitive {
		pattern = "(?i)" + pattern
	}
	expr, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	root, err := call.Resolve(ctx, "grep", g.Dir, defaultString(args.Path, "."))
	if err != nil {
		return "", err
	}
	limit := clampLimit(args.Limit, 100)
	if limit > maxMatches {
		limit = maxMatches
	}

	var matches []struct {
		path string
		line int
		text string
	}
	stopped := false
	scan := func(path string) {
		if stopped {
			return
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Size() > maxFileBytes {
			return
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		if args.Include != "" && !matchPattern(args.Include, filepath.ToSlash(rel)) {
			return
		}
		file, err := os.Open(path)
		if err != nil {
			return
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), maxFileBytes)
		line := 0
		for scanner.Scan() {
			line++
			text := scanner.Text()
			if line == 1 && strings.ContainsRune(text, 0) {
				return
			}
			if !expr.MatchString(text) {
				continue
			}
			matches = append(matches, struct {
				path string
				line int
				text string
			}{path: relPath(g.Dir, path), line: line, text: clipLine(text)})
			if len(matches) >= limit {
				stopped = true
				return
			}
		}
	}

	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	if info.IsDir() {
		err = walk(root, false, func(path, rel string, info os.FileInfo) error {
			if stopped {
				return nil
			}
			scan(path)
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("grep: %w", err)
		}
	} else {
		scan(root)
	}

	call.SetMetadata("matches", len(matches))
	if len(matches) == 0 {
		return "No matches found", nil
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Found %d matches", len(matches)))
	current := ""
	for _, match := range matches {
		if current != match.path {
			if current != "" {
				lines = append(lines, "")
			}
			current = match.path
			lines = append(lines, match.path+":")
		}
		lines = append(lines, fmt.Sprintf("  Line %d: %s", match.line, match.text))
	}
	if stopped {
		lines = append(lines, "", fmt.Sprintf("(Results are truncated: showing first %d results. Consider using a more specific path or pattern.)", len(matches)))
	}
	return truncate(strings.Join(lines, "\n")), nil
}

// walk visits regular files under root, skipping VCS and dependency trees. When
// hidden is false, dot entries are skipped as well. rel is slash-separated and
// relative to the walked root.
func walk(root string, hidden bool, visit func(path, rel string, info os.FileInfo) error) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return visit(root, filepath.Base(root), info)
	}
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		name := info.Name()
		if path != root && !hidden && strings.HasPrefix(name, ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			if skipDirs[name] && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		return visit(path, filepath.ToSlash(rel), info)
	})
}

// matchPattern matches a slash-separated path against a glob where ** spans
// any number of segments. A pattern without a slash matches the base name.
func matchPattern(pattern, path string) bool {
	if !strings.Contains(pattern, "/") {
		ok, err := filepath.Match(pattern, filepath.Base(path))
		return err == nil && ok
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func matchSegments(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		if matchSegments(pattern[1:], path) {
			return true
		}
		return len(path) > 0 && matchSegments(pattern, path[1:])
	}
	if len(path) == 0 {
		return false
	}
	ok, err := filepath.Match(pattern[0], path[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pattern[1:], path[1:])
}

func clipLine(text string) string {
	runes := []rune(strings.TrimRight(text, "\r"))
	if len(runes) <= maxLineRunes {
		return string(runes)
	}
	return string(runes[:maxLineRunes]) + "…"
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
