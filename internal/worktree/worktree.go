// Package worktree creates and removes git worktrees for a repository. It never
// changes the source worktree and stores derived checkouts under the caller's
// data directory.
package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Entry is one worktree reported by git.
type Entry struct {
	Path    string `json:"path"`
	Branch  string `json:"branch,omitempty"`
	Head    string `json:"head,omitempty"`
	Main    bool   `json:"main"`
	Managed bool   `json:"managed"`
	Size    int64  `json:"size"`
}

// Manager owns worktrees derived from one repository.
type Manager struct {
	repo string
	main string
	base string
	root string
	mu   sync.Mutex
}

// New discovers the repository containing workdir. A non-git directory is an
// error so the caller can disable worktree support.
func New(workdir, dataDir string) (*Manager, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("git is not installed: %w", err)
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	repo, err := gitOutput(context.Background(), abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not a git repository", abs)
	}
	repo, err = filepath.Abs(strings.TrimSpace(repo))
	if err != nil {
		return nil, err
	}
	main := mainWorktree(repo)
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(main))))
	root := filepath.Join(dataDir, "worktrees")
	base := filepath.Join(root, hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &Manager{repo: repo, main: main, base: base, root: root}, nil
}

// mainWorktree resolves a linked checkout back to the repository's first
// worktree, so every manager for the repository shares one storage directory
// and marks the same checkout as main.
func mainWorktree(repo string) string {
	common, err := gitOutput(context.Background(), repo, "rev-parse", "--git-common-dir")
	if err != nil {
		return repo
	}
	common = strings.TrimSpace(common)
	if common == "" {
		return repo
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(repo, common)
	}
	common = filepath.Clean(common)
	if !strings.EqualFold(filepath.Base(common), ".git") {
		return repo
	}
	return filepath.Dir(common)
}

// Repo returns the repository's main worktree.
func (m *Manager) Repo() string {
	if m == nil {
		return ""
	}
	return m.main
}

// List returns the repository's worktrees, main first.
func (m *Manager) List(ctx context.Context) ([]Entry, error) {
	if m == nil {
		return nil, fmt.Errorf("worktrees are not available")
	}
	out, err := gitOutput(ctx, m.repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var entries []Entry
	var current *Entry
	flush := func() {
		if current == nil {
			return
		}
		current.Path = filepath.Clean(current.Path)
		current.Main = samePath(current.Path, m.main)
		current.Managed = isManagedPath(m.root, current.Path)
		current.Size = directorySize(current.Path)
		entries = append(entries, *current)
		current = nil
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			flush()
			current = &Entry{Path: value}
		case "HEAD":
			if current != nil {
				current.Head = value
			}
		case "branch":
			if current != nil {
				current.Branch = strings.TrimPrefix(value, "refs/heads/")
			}
		}
	}
	flush()
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Main && !entries[j].Main })
	return entries, nil
}

// Create adds a worktree and returns its entry. A missing branch is created
// from base, which defaults to HEAD.
func (m *Manager) Create(ctx context.Context, name, branch, base string) (Entry, error) {
	if m == nil {
		return Entry{}, fmt.Errorf("worktrees are not available")
	}
	name = strings.TrimSpace(name)
	if !namePattern.MatchString(name) {
		return Entry{}, fmt.Errorf("worktree name must use letters, numbers, dot, dash or underscore")
	}
	if strings.TrimSpace(branch) == "" {
		branch = "lingzhu/" + name
	}
	if strings.ContainsAny(branch, " \t\r\n~^:?*[\\") {
		return Entry{}, fmt.Errorf("invalid branch name %q", branch)
	}
	if strings.TrimSpace(base) == "" {
		base = "HEAD"
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	destination := filepath.Join(m.base, name)
	if _, err := os.Stat(destination); err == nil {
		return Entry{}, fmt.Errorf("worktree %q already exists", name)
	}

	args := []string{"worktree", "add"}
	if _, err := gitOutput(ctx, m.repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		args = append(args, "-b", branch)
	}
	args = append(args, destination)
	if branchExists(ctx, m.repo, branch) {
		args = append(args, branch)
	} else {
		args = append(args, base)
	}
	if _, err := gitOutput(ctx, m.repo, args...); err != nil {
		return Entry{}, err
	}
	entries, err := m.listUnlocked(ctx)
	if err != nil {
		return Entry{}, err
	}
	for _, entry := range entries {
		if samePath(entry.Path, destination) {
			return entry, nil
		}
	}
	return Entry{Path: destination, Branch: branch}, nil
}

func branchExists(ctx context.Context, repo, branch string) bool {
	_, err := gitOutput(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// Remove deletes a worktree. The main worktree cannot be removed.
func (m *Manager) Remove(ctx context.Context, path string, force bool) error {
	if m == nil {
		return fmt.Errorf("worktrees are not available")
	}
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" {
		return fmt.Errorf("worktree path is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := m.listUnlocked(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, entry := range entries {
		if samePath(entry.Path, path) {
			if entry.Main {
				return fmt.Errorf("the main worktree cannot be removed")
			}
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("worktree %s is not registered", path)
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	if _, err := gitOutput(ctx, m.repo, args...); err != nil {
		return err
	}
	return nil
}

func (m *Manager) listUnlocked(ctx context.Context) ([]Entry, error) {
	out, err := gitOutput(ctx, m.repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var entries []Entry
	var current *Entry
	flush := func() {
		if current == nil {
			return
		}
		current.Path = filepath.Clean(current.Path)
		current.Main = samePath(current.Path, m.main)
		current.Managed = isManagedPath(m.root, current.Path)
		current.Size = directorySize(current.Path)
		entries = append(entries, *current)
		current = nil
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			flush()
			current = &Entry{Path: value}
		case "HEAD":
			if current != nil {
				current.Head = value
			}
		case "branch":
			if current != nil {
				current.Branch = strings.TrimPrefix(value, "refs/heads/")
			}
		}
	}
	flush()
	return entries, nil
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// isManagedPath reports whether a checkout lives in this manager's storage
// directory, which is how a worktree created by the app is distinguished from
// one created outside it.
func isManagedPath(base, path string) bool {
	if base == "" {
		return false
	}
	base = filepath.Clean(base)
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		base = strings.ToLower(base)
		path = strings.ToLower(path)
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
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

// directorySize totals the regular files below root. Errors are treated as
// zero so a missing checkout does not fail the whole list.
func directorySize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
