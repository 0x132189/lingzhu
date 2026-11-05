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
	return "Find files by glob pattern relative to the working directory: ** matches any number of directories, * and ? match within one segment. Results are newest first."
}

func (g *Glob) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Glob such as **/*.go"},
			"limit": {"type": "integer", "description": "Maximum results, default 100"}
		},
		"required": ["pattern"]
	}`)
}

func (g *Glob) Run(ctx context.Context, call Call) (string, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	pattern := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(args.Pattern)), "./")
	if pattern == "" {
		return "", fmt.Errorf("glob: pattern is empty")
	}
	if err := call.Allow(ctx, "read", []string{pattern}, ""); err != nil {
		return "", err
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > maxMatches {
		limit = maxMatches
	}

	type hit struct {
		rel   string
		mtime time.Time
	}
	var hits []hit
	err := walk(g.Dir, func(path, rel string, info os.FileInfo) error {
		if matchPattern(pattern, rel) {
			hits = append(hits, hit{rel: rel, mtime: info.ModTime()})
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].mtime.After(hits[j].mtime) })
	if len(hits) == 0 {
		return "no files match " + pattern, nil
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
		fmt.Fprintf(&b, "(showing %d of %d+ matches)", limit, limit)
	}
	return truncate(strings.TrimRight(b.String(), "\n")), nil
}

// Grep searches file contents with a regular expression.
type Grep struct {
	Dir string
}

func (g *Grep) Name() string { return "grep" }

func (g *Grep) Description() string {
	return "Search file contents with a Go regular expression and print file:line:text. Narrow the search with include (glob) and path (directory)."
}

func (g *Grep) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Go regular expression"},
			"include": {"type": "string", "description": "Only search files matching this glob, e.g. *.go"},
			"path": {"type": "string", "description": "Directory to search, default the working directory"},
			"limit": {"type": "integer", "description": "Maximum matches, default 100"}
		},
		"required": ["pattern"]
	}`)
}

func (g *Grep) Run(ctx context.Context, call Call) (string, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Include string `json:"include"`
		Path    string `json:"path"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	expr, err := regexp.Compile(args.Pattern)
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	root, err := call.Resolve(ctx, "read", g.Dir, defaultString(args.Path, "."))
	if err != nil {
		return "", err
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > maxMatches {
		limit = maxMatches
	}

	var b strings.Builder
	matches := 0
	stopped := false
	err = walk(root, func(path, rel string, info os.FileInfo) error {
		if stopped || info.Size() > maxFileBytes {
			return nil
		}
		if args.Include != "" && !matchPattern(args.Include, rel) {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer file.Close()

		label := rel
		if root != g.Dir {
			label = filepath.ToSlash(path)
		}
		scan := bufio.NewScanner(file)
		scan.Buffer(make([]byte, 0, 64*1024), maxFileBytes)
		line := 0
		binary := false
		for scan.Scan() {
			line++
			text := scan.Text()
			if line == 1 && strings.ContainsRune(text, 0) {
				binary = true
				break
			}
			if !expr.MatchString(text) {
				continue
			}
			fmt.Fprintf(&b, "%s:%d: %s\n", label, line, clipLine(text))
			matches++
			if matches >= limit {
				stopped = true
				return nil
			}
		}
		if binary {
			return nil
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	if matches == 0 {
		return "no matches for " + args.Pattern, nil
	}
	if stopped {
		fmt.Fprintf(&b, "(stopped at %d matches)\n", limit)
	}
	return truncate(strings.TrimRight(b.String(), "\n")), nil
}

// walk visits regular files under root, skipping VCS and dependency trees.
// rel is slash-separated and relative to the walked root.
func walk(root string, visit func(path, rel string, info os.FileInfo) error) error {
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
		if info.IsDir() {
			if skipDirs[info.Name()] && path != root {
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
