package app

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lingzhu/internal/config"
	"lingzhu/internal/global"
	"lingzhu/internal/skill"
)

const skillWatchInterval = 2 * time.Second

var skillSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".venv":        true,
	"__pycache__":  true,
	"dist":         true,
	"build":        true,
}

// skillsState owns one workspace's skill catalog and the roots it was built
// from. The watcher rescans those roots, and the refresh endpoint re-runs the
// remote pull before rescanning.
type skillsState struct {
	mu         sync.RWMutex
	manager    *skill.Manager
	roots      []string
	localRoots []string
	errors     []skill.SourceError
	cfg        config.Config
	workdir    string
	logf       func(string, ...any)
	keep       func(string) bool
}

func newSkillsState(manager *skill.Manager, roots []string, cfg config.Config, workdir string, logf func(string, ...any), keep func(string) bool) *skillsState {
	return &skillsState{
		manager:    manager,
		roots:      append([]string(nil), roots...),
		localRoots: skill.Roots(cfg.Skills.Paths, cfg.Path, workdir),
		cfg:        cfg,
		workdir:    workdir,
		logf:       logf,
		keep:       keep,
	}
}

// Roots returns a copy of the current scan roots.
func (s *skillsState) Roots() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.roots...)
}

// refresh reloads local roots and retries every configured remote source.
func (s *skillsState) refresh(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("skills are not available")
	}
	local := skill.Roots(s.cfg.Skills.Paths, s.cfg.Path, s.workdir)
	roots := local
	var pullErr error
	var pullErrors []skill.SourceError
	if len(s.cfg.Skills.URLs) > 0 {
		result := skill.PullStatus(ctx, s.cfg.Skills.URLs, global.Cache(), s.logf)
		pullErrors = result.Errors
		if len(result.Roots) > 0 {
			roots = append(result.Roots, local...)
		} else {
			roots = append(s.Roots(), local...)
		}
		if len(result.Errors) > 0 {
			parts := make([]string, 0, len(result.Errors))
			for _, item := range result.Errors {
				if item.URL == "" {
					parts = append(parts, item.Error)
				} else {
					parts = append(parts, item.URL+": "+item.Error)
				}
			}
			pullErr = fmt.Errorf("skill sources: %s", strings.Join(parts, "; "))
		}
	}
	s.manager.Reload(roots, s.logf)
	s.manager.Filter(s.keep)
	s.mu.Lock()
	s.roots = roots
	s.localRoots = local
	if len(s.cfg.Skills.URLs) > 0 {
		s.errors = append([]skill.SourceError(nil), pullErrors...)
	}
	s.mu.Unlock()
	return pullErr
}

// refreshSource retries one remote URL and rebuilds the catalog with the new
// roots for that source, keeping every other root in place.
func (s *skillsState) refreshSource(ctx context.Context, sourceURL string) error {
	if s == nil {
		return fmt.Errorf("skills are not available")
	}
	sourceURL = strings.TrimSpace(sourceURL)
	if sourceURL == "" {
		return s.refresh(ctx)
	}
	local := skill.Roots(s.cfg.Skills.Paths, s.cfg.Path, s.workdir)
	cacheRoot := filepath.Join(global.Cache(), "skills")
	result := skill.PullStatus(ctx, []string{sourceURL}, global.Cache(), s.logf)

	// One source owns every root under its cache key, so the refreshed roots
	// replace exactly the previous roots of that key.
	refreshed := map[string]bool{}
	for _, root := range result.Roots {
		relative, err := filepath.Rel(cacheRoot, root)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		refreshed[strings.Split(relative, string(filepath.Separator))[0]] = true
	}
	roots := append([]string(nil), result.Roots...)
	for _, root := range s.Roots() {
		relative, err := filepath.Rel(cacheRoot, root)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			if refreshed[strings.Split(relative, string(filepath.Separator))[0]] {
				continue
			}
		}
		roots = append(roots, root)
	}
	for _, root := range local {
		if !containsRoot(roots, root) {
			roots = append(roots, root)
		}
	}
	s.manager.Reload(roots, s.logf)
	s.manager.Filter(s.keep)
	s.mu.Lock()
	s.roots = roots
	s.localRoots = local
	kept := s.errors[:0]
	for _, item := range s.errors {
		if item.URL != sourceURL {
			kept = append(kept, item)
		}
	}
	s.errors = append(kept, result.Errors...)
	s.mu.Unlock()
	if len(result.Errors) > 0 {
		parts := make([]string, 0, len(result.Errors))
		for _, item := range result.Errors {
			if item.URL == "" {
				parts = append(parts, item.Error)
			} else {
				parts = append(parts, item.URL+": "+item.Error)
			}
		}
		return fmt.Errorf("skill source: %s", strings.Join(parts, "; "))
	}
	return nil
}

// SourceErrors returns the last per-source pull failures.
func (s *skillsState) SourceErrors() []skill.SourceError {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]skill.SourceError(nil), s.errors...)
}

// reloadLocal rescans the roots already known without touching the network.
func (s *skillsState) reloadLocal() {
	s.manager.Reload(s.Roots(), s.logf)
	s.manager.Filter(s.keep)
}

// CreateSkill writes a new SKILL.md under the first configured local root.
func (s *skillsState) CreateSkill(ctx context.Context, id, name, description, content string) error {
	if s == nil {
		return fmt.Errorf("skills are not available")
	}
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) || filepath.Base(id) != id {
		return fmt.Errorf("invalid skill id %q", id)
	}
	s.mu.RLock()
	roots := append([]string(nil), s.localRoots...)
	s.mu.RUnlock()
	// Prefer the project's own skill directory so new skills do not land in a
	// shared home directory by accident.
	root := ""
	if strings.TrimSpace(s.workdir) != "" {
		root = filepath.Join(s.workdir, ".lingzhu", "skills")
	}
	if root == "" && len(roots) > 0 {
		root = roots[0]
	}
	if root == "" {
		return fmt.Errorf("no local skill root is configured")
	}
	s.mu.Lock()
	if !containsRoot(s.roots, root) {
		s.roots = append(s.roots, root)
	}
	if !containsRoot(s.localRoots, root) {
		s.localRoots = append(s.localRoots, root)
	}
	s.mu.Unlock()
	directory := filepath.Join(root, id)
	if _, err := os.Stat(directory); err == nil {
		return fmt.Errorf("skill %q already exists", id)
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		name = id
	}
	if strings.TrimSpace(content) == "" {
		content = "# " + name
	}
	body := fmt.Sprintf("---\nname: %q\ndescription: %q\n---\n%s\n", name, strings.TrimSpace(description), strings.TrimSpace(content))
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(body), 0o644); err != nil {
		return err
	}
	s.reloadLocal()
	return nil
}

// DeleteSkill removes a skill whose directory is inside a configured local
// root. Remote cache packages are never deleted from the UI.
func (s *skillsState) DeleteSkill(id string) error {
	if s == nil {
		return fmt.Errorf("skills are not available")
	}
	item, ok := s.manager.Get(strings.TrimSpace(id))
	if !ok {
		return fmt.Errorf("skill %q not found", id)
	}
	s.mu.RLock()
	roots := append([]string(nil), s.localRoots...)
	s.mu.RUnlock()
	allowed := false
	flat := filepath.Clean(filepath.Dir(item.Path)) == filepath.Clean(item.Directory) &&
		strings.EqualFold(filepath.Ext(item.Path), ".md") &&
		!strings.EqualFold(filepath.Base(item.Path), "SKILL.md")
	for _, root := range roots {
		root = filepath.Clean(root)
		if flat && strings.HasPrefix(filepath.Clean(item.Path), root+string(filepath.Separator)) {
			allowed = true
			break
		}
		if !flat && strings.HasPrefix(filepath.Clean(item.Directory), root+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("skill %q is not in a local root", id)
	}
	if flat {
		if err := os.Remove(item.Path); err != nil {
			return err
		}
	} else if err := os.RemoveAll(item.Directory); err != nil {
		return err
	}
	s.reloadLocal()
	return nil
}

func containsRoot(roots []string, target string) bool {
	for _, root := range roots {
		if filepath.Clean(root) == filepath.Clean(target) {
			return true
		}
	}
	return false
}

func (s *skillsState) watch(ctx context.Context) {
	watchSkills(ctx, s.manager, s.Roots, s.keep, s.logf, skillWatchInterval)
}

// watchSkills rescans when a skill file appears, changes, or disappears. The
// interval is a parameter so tests can run faster than the workspace default.
func watchSkills(ctx context.Context, manager *skill.Manager, roots func() []string, keep func(string) bool, logf func(string, ...any), interval time.Duration) {
	if interval <= 0 {
		interval = skillWatchInterval
	}
	signature := skillSignature(roots())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		next := skillSignature(roots())
		if next == signature {
			continue
		}
		signature = next
		manager.Reload(roots(), logf)
		manager.Filter(keep)
	}
}

// skillSignature tracks the files that affect discovery: every SKILL.md, the
// top-level markdown documents of each root, and the resources beside a
// SKILL.md. A missing root is part of the signature so creating or removing it
// triggers a rescan.
func skillSignature(roots []string) string {
	var b strings.Builder
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			b.WriteString(root)
			b.WriteString(":missing\n")
			continue
		}
		var skillDirs []string
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			name := entry.Name()
			if entry.IsDir() {
				if path == root {
					return nil
				}
				if skillSkipDirs[name] || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err == nil {
					skillDirs = append(skillDirs, path)
				}
				return nil
			}
			if strings.HasPrefix(name, ".") {
				return nil
			}
			base := filepath.Base(path)
			topLevel := filepath.Dir(path) == root
			inSkill := false
			for _, dir := range skillDirs {
				if strings.HasPrefix(path, dir+string(filepath.Separator)) {
					inSkill = true
					break
				}
			}
			if base != "SKILL.md" && !(topLevel && strings.HasSuffix(strings.ToLower(base), ".md")) && !inSkill {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return nil
			}
			fmt.Fprintf(&b, "%s|%d|%d\n", path, info.Size(), info.ModTime().UnixNano())
			return nil
		})
	}
	return b.String()
}
