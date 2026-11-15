// Package vcs exposes read-only git state for clients: repository info, branch
// lists, uncommitted status and per-file diffs. It shells out to git so it
// works with the same repository the agent and snapshot store already use.
package vcs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	maxFiles       = 200
	maxPatchBytes  = 5 * 1024 * 1024
	maxBranchItems = 100
)

// ErrNotRepository means the configured workdir is not inside a git checkout.
var ErrNotRepository = errors.New("vcs: not a git repository")

// ErrAlreadyRepository means init was asked for a directory that already has
// version control.
var ErrAlreadyRepository = errors.New("vcs: directory already has a repository")

// Branch is the current and default branch name.
type Branch struct {
	Current string `json:"current,omitempty"`
	Default string `json:"default,omitempty"`
}

// Info is repository metadata.
type Info struct {
	Provider string `json:"provider"`
	Branch   Branch `json:"branch"`
}

// Base is the inferred review base.
type Base struct {
	Name   string `json:"name"`
	Ref    string `json:"ref"`
	Source string `json:"source"`
}

// FileStatus is one uncommitted change with line counts.
type FileStatus struct {
	File      string `json:"file"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Status    string `json:"status"`
}

// FileDiff is one file patch.
type FileDiff struct {
	File      string `json:"file"`
	Patch     string `json:"patch"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Status    string `json:"status"`
}

// DiffOptions controls the diff endpoints.
type DiffOptions struct {
	// Mode is working, branch or committed.
	Mode string
	// Base overrides the inferred review base.
	Base string
	// Context is the unified context line count; negative keeps git's default.
	Context int
	// Untracked is files (default) or folders.
	Untracked string
}

// Manager runs git in one workdir.
type Manager struct {
	dir string
}

// New prepares a manager; repository checks happen per request.
func New(workdir string) (*Manager, error) {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("vcs: %s is not a directory", abs)
	}
	return &Manager{dir: abs}, nil
}

// Info returns the provider and branch names.
func (m *Manager) Info(ctx context.Context) (Info, error) {
	if err := m.ensure(ctx); err != nil {
		return Info{}, err
	}
	out := Info{Provider: "git"}
	if current, err := m.run(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		current = strings.TrimSpace(current)
		if current != "HEAD" {
			out.Branch.Current = current
		}
	}
	out.Branch.Default = m.defaultBranch(ctx)
	return out, nil
}

// Base returns the inferred review base, or nil before the first commit.
func (m *Manager) Base(ctx context.Context) (*Base, error) {
	if err := m.ensure(ctx); err != nil {
		return nil, err
	}
	if _, err := m.run(ctx, "rev-parse", "--verify", "HEAD"); err != nil {
		return nil, nil
	}
	name := m.defaultBranch(ctx)
	if name == "" {
		return nil, nil
	}
	ref := name
	if merged, err := m.run(ctx, "merge-base", "HEAD", name); err == nil {
		ref = strings.TrimSpace(merged)
	}
	if ref == "" {
		return nil, nil
	}
	return &Base{Name: name, Ref: ref, Source: "default"}, nil
}

// Status lists uncommitted files with additions and deletions.
func (m *Manager) Status(ctx context.Context) ([]FileStatus, error) {
	if err := m.ensure(ctx); err != nil {
		return nil, err
	}
	changes, err := m.statusEntries(ctx)
	if err != nil {
		return nil, err
	}
	stats := map[string][2]int{}
	if out, err := m.run(ctx, "diff", "--numstat", "HEAD"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			fields := strings.SplitN(line, "\t", 3)
			if len(fields) != 3 {
				continue
			}
			additions, _ := strconv.Atoi(fields[0])
			deletions, _ := strconv.Atoi(fields[1])
			stats[normalizePath(fields[2])] = [2]int{additions, deletions}
		}
	}
	out := make([]FileStatus, 0, len(changes))
	for _, change := range changes {
		status := classify(change.code)
		counts := stats[change.path]
		if change.code == "??" {
			additions := countLines(filepath.Join(m.dir, filepath.FromSlash(change.path)))
			counts = [2]int{additions, 0}
		}
		out = append(out, FileStatus{
			File:      change.path,
			Additions: counts[0],
			Deletions: counts[1],
			Status:    status,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

// Branches lists local and remote branch names.
func (m *Manager) Branches(ctx context.Context, search string, limit int) ([]string, error) {
	if err := m.ensure(ctx); err != nil {
		return nil, err
	}
	out, err := m.run(ctx, "for-each-ref", "--format=%(refname:short)", "refs/heads", "refs/remotes")
	if err != nil {
		return nil, err
	}
	search = strings.ToLower(strings.TrimSpace(search))
	if limit <= 0 || limit > maxBranchItems {
		limit = 50
	}
	branches := make([]string, 0, 16)
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasSuffix(name, "/HEAD") || strings.EqualFold(name, "HEAD") {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(name), search) {
			continue
		}
		branches = append(branches, name)
		if len(branches) >= limit {
			break
		}
	}
	sort.Strings(branches)
	return branches, nil
}

// Diff returns per-file patches for one comparison mode.
func (m *Manager) Diff(ctx context.Context, options DiffOptions) ([]FileDiff, error) {
	if err := m.ensure(ctx); err != nil {
		return nil, err
	}
	mode := strings.ToLower(strings.TrimSpace(options.Mode))
	if mode == "" {
		mode = "working"
	}
	args := []string{"diff", "--no-color", "--patch", "--no-ext-diff"}
	if options.Context >= 0 {
		args = append(args, "--unified="+strconv.Itoa(options.Context))
	}
	switch mode {
	case "working":
		if _, err := m.run(ctx, "rev-parse", "--verify", "HEAD"); err == nil {
			args = append(args, "HEAD")
		}
	case "branch", "committed":
		base := strings.TrimSpace(options.Base)
		if base == "" {
			base = m.defaultBranch(ctx)
		}
		if base == "" {
			return nil, errors.New("vcs: no review base is available")
		}
		mergeBase, err := m.run(ctx, "merge-base", "HEAD", base)
		if err != nil {
			return nil, fmt.Errorf("vcs: merge-base %s: %w", base, err)
		}
		mergeBase = strings.TrimSpace(mergeBase)
		if mode == "committed" {
			args = append(args, mergeBase, "HEAD")
		} else {
			args = append(args, mergeBase)
		}
	default:
		return nil, fmt.Errorf("vcs: unknown diff mode %q", mode)
	}
	out, err := m.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	entries := splitPatch(out)
	if mode != "committed" {
		entries = append(entries, m.untrackedDiffs(ctx, options.Untracked)...)
	}
	if len(entries) > maxFiles {
		entries = entries[:maxFiles]
	}
	total := 0
	trimmed := entries[:0]
	for _, entry := range entries {
		if total+len(entry.Patch) > maxPatchBytes {
			break
		}
		total += len(entry.Patch)
		trimmed = append(trimmed, entry)
	}
	sort.Slice(trimmed, func(i, j int) bool { return trimmed[i].File < trimmed[j].File })
	return trimmed, nil
}

// Init initializes a git repository in the workdir.
func (m *Manager) Init(ctx context.Context) error {
	if err := m.ensure(ctx); err == nil {
		return ErrAlreadyRepository
	}
	if _, err := m.run(ctx, "init", "-q"); err != nil {
		return err
	}
	return nil
}

func (m *Manager) ensure(ctx context.Context) error {
	out, err := m.run(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(out) != "true" {
		return ErrNotRepository
	}
	return nil
}

func (m *Manager) defaultBranch(ctx context.Context) string {
	if out, err := m.run(ctx, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		name := strings.TrimSpace(out)
		name = strings.TrimPrefix(name, "origin/")
		if name != "" {
			return name
		}
	}
	for _, candidate := range []string{"main", "master"} {
		if _, err := m.run(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+candidate); err == nil {
			return candidate
		}
	}
	if out, err := m.run(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		name := strings.TrimSpace(out)
		if name != "" && name != "HEAD" {
			return name
		}
	}
	return ""
}

type statusEntry struct {
	code string
	path string
}

func (m *Manager) statusEntries(ctx context.Context) ([]statusEntry, error) {
	out, err := m.run(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\x00")
	entries := make([]statusEntry, 0, len(parts))
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if len(part) < 4 {
			continue
		}
		code := part[:2]
		path := strings.TrimSpace(part[3:])
		if path == "" {
			continue
		}
		if strings.ContainsAny(code, "RC") && i+1 < len(parts) {
			// The rename/copy source follows as its own NUL field.
			i++
		}
		entries = append(entries, statusEntry{code: code, path: normalizePath(path)})
	}
	return entries, nil
}

func (m *Manager) untrackedDiffs(ctx context.Context, untracked string) []FileDiff {
	entries, err := m.statusEntries(ctx)
	if err != nil {
		return nil
	}
	grouped := untracked == "folders"
	out := make([]FileDiff, 0, 8)
	folders := map[string]*FileDiff{}
	var order []string
	for _, entry := range entries {
		if entry.code != "??" {
			continue
		}
		full := filepath.Join(m.dir, filepath.FromSlash(entry.path))
		parts := strings.SplitN(entry.path, "/", 2)
		if !grouped || len(parts) == 1 {
			patch, additions := addedPatch(entry.path, full)
			if patch == "" {
				continue
			}
			out = append(out, FileDiff{
				File:      entry.path,
				Patch:     patch,
				Additions: additions,
				Status:    "added",
			})
			continue
		}
		folder := parts[0]
		group, ok := folders[folder]
		if !ok {
			group = &FileDiff{
				File:   folder + "/",
				Patch:  "diff --git a/" + folder + "/ b/" + folder + "/\n",
				Status: "added",
			}
			folders[folder] = group
			order = append(order, folder)
		}
		group.Patch += "# " + entry.path + "\n"
		group.Additions += countLines(full)
	}
	for _, name := range order {
		out = append(out, *folders[name])
	}
	return out
}

func addedPatch(path, full string) (string, int) {
	data, err := os.ReadFile(full)
	if err != nil || isBinary(data) {
		return "", 0
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	var b strings.Builder
	b.WriteString("diff --git a/" + path + " b/" + path + "\n")
	b.WriteString("new file mode 100644\n")
	b.WriteString("--- /dev/null\n")
	b.WriteString("+++ b/" + path + "\n")
	fmt.Fprintf(&b, "@@ -0,0 +1,%d @@\n", len(lines))
	for _, line := range lines {
		b.WriteString("+" + line + "\n")
	}
	return b.String(), len(lines)
}

func splitPatch(out string) []FileDiff {
	out = strings.ReplaceAll(out, "\r\n", "\n")
	if strings.TrimSpace(out) == "" {
		return nil
	}
	var chunks []string
	for _, line := range strings.SplitAfter(out, "\n") {
		if strings.HasPrefix(line, "diff --git ") && len(chunks) > 0 {
			chunks = append(chunks, "")
		}
		if len(chunks) == 0 {
			chunks = append(chunks, "")
		}
		chunks[len(chunks)-1] += line
	}
	entries := make([]FileDiff, 0, len(chunks))
	for _, chunk := range chunks {
		chunk = strings.TrimRight(chunk, "\n")
		if chunk == "" {
			continue
		}
		path := patchPath(chunk)
		if path == "" {
			continue
		}
		additions, deletions := countPatchLines(chunk)
		entries = append(entries, FileDiff{
			File:      path,
			Patch:     chunk + "\n",
			Additions: additions,
			Deletions: deletions,
			Status:    classifyPatch(chunk),
		})
	}
	return entries
}

func patchPath(chunk string) string {
	for _, line := range strings.Split(chunk, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			return normalizePath(strings.TrimPrefix(line, "+++ b/"))
		case strings.HasPrefix(line, "--- a/"):
			return normalizePath(strings.TrimPrefix(line, "--- a/"))
		}
	}
	for _, line := range strings.Split(chunk, "\n") {
		if strings.HasPrefix(line, "diff --git a/") {
			rest := strings.TrimPrefix(line, "diff --git a/")
			if index := strings.LastIndex(rest, " b/"); index > 0 {
				return normalizePath(rest[:index])
			}
		}
	}
	return ""
}

func classifyPatch(chunk string) string {
	switch {
	case strings.Contains(chunk, "\nnew file mode "):
		return "added"
	case strings.Contains(chunk, "\ndeleted file mode "):
		return "deleted"
	default:
		return "modified"
	}
}

func countPatchLines(chunk string) (int, int) {
	additions, deletions := 0, 0
	for _, line := range strings.Split(chunk, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			additions++
		case strings.HasPrefix(line, "-"):
			deletions++
		}
	}
	return additions, deletions
}

func classify(code string) string {
	switch {
	case code == "??":
		return "added"
	case strings.Contains(code, "D"):
		return "deleted"
	case strings.Contains(code, "A"):
		return "added"
	default:
		return "modified"
	}
}

func normalizePath(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `"`)
	return filepath.ToSlash(filepath.Clean(value))
}

func countLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil || isBinary(data) {
		return 0
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if text == "" {
		return 0
	}
	lines := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		lines++
	}
	return lines
}

func isBinary(data []byte) bool {
	limit := len(data)
	if limit > 8000 {
		limit = 8000
	}
	for i := 0; i < limit; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

func (m *Manager) run(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"-c", "core.quotepath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = m.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), detail)
	}
	return string(out), nil
}
