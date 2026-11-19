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
	mu      sync.RWMutex
	manager *skill.Manager
	roots   []string
	cfg     config.Config
	workdir string
	logf    func(string, ...any)
	keep    func(string) bool
}

func newSkillsState(manager *skill.Manager, roots []string, cfg config.Config, workdir string, logf func(string, ...any), keep func(string) bool) *skillsState {
	return &skillsState{
		manager: manager,
		roots:   append([]string(nil), roots...),
		cfg:     cfg,
		workdir: workdir,
		logf:    logf,
		keep:    keep,
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
	if len(s.cfg.Skills.URLs) > 0 {
		remote := skill.Pull(ctx, s.cfg.Skills.URLs, global.Cache(), s.logf)
		if len(remote) > 0 {
			roots = append(remote, local...)
		} else {
			pullErr = fmt.Errorf("no remote skill source could be loaded")
			roots = append(s.Roots(), local...)
		}
	}
	s.manager.Reload(roots, s.logf)
	s.manager.Filter(s.keep)
	s.mu.Lock()
	s.roots = roots
	s.mu.Unlock()
	return pullErr
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
