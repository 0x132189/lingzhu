// Package snapshot keeps per-turn copies of the work directory in a shadow git
// repository. It never touches the project's own repository, and a missing git
// binary simply disables snapshots.
package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Manager serializes access to one shadow repository.
type Manager struct {
	workdir string
	gitDir  string
	mu      sync.Mutex
}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// New prepares a shadow repository under cacheDir for workdir.
func New(workdir, cacheDir string) (*Manager, error) {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("git is not installed: %w", err)
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(abs))))
	gitDir := filepath.Join(cacheDir, "snapshots", hex.EncodeToString(sum[:8]))
	m := &Manager{workdir: abs, gitDir: gitDir}
	if err := m.setup(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) setup() error {
	if err := os.MkdirAll(m.gitDir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(m.gitDir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		cmd := exec.Command("git", "init", "-q", "--bare", m.gitDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git init: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if err := os.MkdirAll(filepath.Join(m.gitDir, "info"), 0o755); err != nil {
		return err
	}
	exclude := strings.Join([]string{
		".git/",
		".dev/",
		".lingzhu/",
		"node_modules/",
		"vendor/",
		"dist/",
		"build/",
		".venv/",
		"__pycache__/",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(m.gitDir, "info", "exclude"), []byte(exclude), 0o644); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, args := range [][]string{
		{"config", "user.name", "lingzhu"},
		{"config", "user.email", "snapshots@lingzhu.local"},
		{"config", "commit.gpgsign", "false"},
		{"config", "core.autocrlf", "false"},
	} {
		if _, err := m.git(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

// Capture commits the current work directory and returns the commit hash. An
// unchanged tree reuses the previous commit instead of creating a new one.
func (m *Manager) Capture(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.git(ctx, "add", "-A"); err != nil {
		return "", err
	}
	head, headErr := m.git(ctx, "rev-parse", "--verify", "HEAD")
	if headErr == nil {
		if _, err := m.git(ctx, "diff", "--cached", "--quiet"); err == nil {
			return strings.TrimSpace(head), nil
		}
	}
	message := fmt.Sprintf("snapshot %d", time.Now().UnixNano())
	if _, err := m.git(ctx, "commit", "-q", "--allow-empty", "-m", message); err != nil {
		return "", err
	}
	out, err := m.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Changed lists the files that differ between two commits.
func (m *Manager) Changed(ctx context.Context, from, to string) ([]string, error) {
	changes, err := m.changes(ctx, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(changes))
	for _, change := range changes {
		out = append(out, change.path)
		if change.old != "" {
			out = append(out, change.old)
		}
	}
	sort.Strings(out)
	return compact(out), nil
}

// FileDiff is one file changed between two snapshots.
type FileDiff struct {
	File      string `json:"file"`
	Patch     string `json:"patch"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Status    string `json:"status"`
}

// Diff returns unified patches between two captured snapshots.
func (m *Manager) Diff(ctx context.Context, from, to string) ([]FileDiff, error) {
	if !commitPattern.MatchString(strings.TrimSpace(from)) || !commitPattern.MatchString(strings.TrimSpace(to)) {
		return nil, errors.New("invalid snapshot")
	}
	if from == to {
		return []FileDiff{}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out, err := m.git(ctx, "diff", "--no-color", "--unified=3", from, to, "--")
	if err != nil {
		return nil, err
	}
	return parsePatch(out), nil
}

func parsePatch(out string) []FileDiff {
	if strings.TrimSpace(out) == "" {
		return []FileDiff{}
	}
	diffs := []FileDiff{}
	var current *FileDiff
	var patch strings.Builder
	additions, deletions := 0, 0
	flush := func() {
		if current == nil {
			return
		}
		current.Patch = patch.String()
		current.Additions = additions
		current.Deletions = deletions
		diffs = append(diffs, *current)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			current = &FileDiff{File: diffHeaderPath(line), Status: "modified"}
			patch.Reset()
			additions, deletions = 0, 0
			patch.WriteString(line)
			patch.WriteByte('\n')
			continue
		}
		if current == nil {
			continue
		}
		patch.WriteString(line)
		patch.WriteByte('\n')
		switch {
		case strings.HasPrefix(line, "new file mode"):
			current.Status = "added"
		case strings.HasPrefix(line, "deleted file mode"):
			current.Status = "deleted"
		case strings.HasPrefix(line, "+++ b/"):
			current.File = strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			additions++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			deletions++
		}
	}
	flush()
	return diffs
}

func diffHeaderPath(line string) string {
	line = strings.TrimPrefix(line, "diff --git ")
	if index := strings.LastIndex(line, " b/"); index >= 0 {
		return line[index+3:]
	}
	return line
}

// Restore writes the given commit back into the work directory. Files added
// after that commit are removed; modified and deleted files are restored.
func (m *Manager) Restore(ctx context.Context, commit string) error {
	if !commitPattern.MatchString(strings.TrimSpace(commit)) {
		return fmt.Errorf("invalid snapshot %q", commit)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.git(ctx, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return fmt.Errorf("snapshot %s is not available", commit[:7])
	}
	// Sync the index with the worktree so the diff describes what is on disk,
	// not merely what HEAD recorded.
	if _, err := m.git(ctx, "add", "-A"); err != nil {
		return err
	}
	changes, err := m.worktreeChanges(ctx, commit)
	if err != nil {
		return err
	}
	var checkout []string
	for _, change := range changes {
		switch change.status {
		case "A":
			if err := removePath(m.workdir, change.path); err != nil {
				return err
			}
		case "R", "C":
			if err := removePath(m.workdir, change.path); err != nil {
				return err
			}
			if change.old != "" {
				checkout = append(checkout, change.old)
			}
		case "D", "M", "T":
			checkout = append(checkout, change.path)
		}
	}
	for start := 0; start < len(checkout); start += 100 {
		end := min(start+100, len(checkout))
		args := append([]string{"checkout", commit, "--"}, checkout[start:end]...)
		if _, err := m.git(ctx, args...); err != nil {
			return err
		}
	}
	_, _ = m.git(ctx, "add", "-A")
	return nil
}

type change struct {
	status string
	path   string
	old    string
}

func (m *Manager) changes(ctx context.Context, from, to string) ([]change, error) {
	if from == to {
		return nil, nil
	}
	out, err := m.git(ctx, "diff", "--name-status", "-z", from, to)
	if err != nil {
		return nil, err
	}
	return parseChanges(out), nil
}

func (m *Manager) worktreeChanges(ctx context.Context, commit string) ([]change, error) {
	out, err := m.git(ctx, "diff", "--name-status", "-z", commit)
	if err != nil {
		return nil, err
	}
	return parseChanges(out), nil
}

func parseChanges(out string) []change {
	tokens := strings.Split(out, "\x00")
	var changes []change
	for i := 0; i < len(tokens); i++ {
		status := strings.TrimSpace(tokens[i])
		if status == "" {
			continue
		}
		kind := status[:1]
		if i+1 >= len(tokens) {
			break
		}
		path := tokens[i+1]
		i++
		item := change{status: kind, path: path}
		if kind == "R" || kind == "C" {
			if i+1 < len(tokens) {
				item.old = path
				item.path = tokens[i+1]
				i++
			}
		}
		changes = append(changes, item)
	}
	return changes
}

func (m *Manager) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = m.workdir
	cmd.Env = append(cleanEnv(), "GIT_DIR="+m.gitDir, "GIT_WORK_TREE="+m.workdir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), detail)
	}
	return stdout.String(), nil
}

func cleanEnv() []string {
	env := os.Environ()
	out := env[:0]
	for _, item := range env {
		if strings.HasPrefix(item, "GIT_") {
			continue
		}
		out = append(out, item)
	}
	return out
}

func removePath(root, relative string) error {
	full := filepath.Join(root, filepath.FromSlash(relative))
	if !strings.HasPrefix(filepath.Clean(full), filepath.Clean(root)+string(os.PathSeparator)) {
		return fmt.Errorf("snapshot path escapes the work directory: %s", relative)
	}
	if err := os.RemoveAll(full); err != nil {
		return err
	}
	parent := filepath.Dir(full)
	for parent != root {
		if err := os.Remove(parent); err != nil {
			break
		}
		parent = filepath.Dir(parent)
	}
	return nil
}

func compact(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
