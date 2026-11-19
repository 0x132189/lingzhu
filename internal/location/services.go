package location

import (
	"context"
	"sync"

	"lingzhu/internal/agent"
	"lingzhu/internal/agents"
	"lingzhu/internal/commands"
	"lingzhu/internal/config"
	"lingzhu/internal/format"
	"lingzhu/internal/mcp"
	"lingzhu/internal/permission"
	"lingzhu/internal/plugin"
	"lingzhu/internal/pty"
	"lingzhu/internal/skill"
	"lingzhu/internal/snapshot"
	"lingzhu/internal/subagent"
	"lingzhu/internal/tool"
	"lingzhu/internal/vcs"
	"lingzhu/internal/worktree"
)

// Services is everything one workspace owns. Nothing here is shared with
// another workspace: each entry is built from that directory's own config,
// files and git repository, so two projects in one process never see each
// other's tools, snapshots, terminals, MCP servers or agent profiles.
//
// Managers that are keyed by data directory rather than by source tree — the
// session store, artifacts, schedules, shares — stay on the core and are not
// part of this struct.
type Services struct {
	Ref Ref

	Agent      *agent.Agent
	Agents     *agents.Registry
	Tools      *tool.Registry
	Permission *permission.Manager
	Snapshots  *snapshot.Manager
	Formatter  *format.Manager
	Worktrees  *worktree.Manager
	PTY        *pty.Manager
	VCS        *vcs.Manager
	MCP        *mcp.Manager
	Plugins    *plugin.Manager
	Skills     *skill.Manager
	Commands   *commands.Registry
	Subagents  *subagent.Manager

	// RefreshSkills rescans local skill roots and retries remote sources.
	RefreshSkills func(context.Context) error
	// CreateSkill writes a new skill under a local root.
	CreateSkill func(context.Context, string, string, string, string) error
	// DeleteSkill removes one skill from a local root.
	DeleteSkill func(string) error
	// SkillErrors returns per-source remote pull failures.
	SkillErrors func() []skill.SourceError

	configMu sync.RWMutex
	config   config.Config

	// CloseFn releases the child processes and watchers this workspace started.
	CloseFn   func()
	closeOnce sync.Once
}

// Config returns this workspace's resolved configuration.
func (s *Services) Config() config.Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

// SetConfig replaces this workspace's configuration. Only the settings a client
// can patch are affected; the services themselves keep what they were built
// with until the workspace is reopened.
func (s *Services) SetConfig(cfg config.Config) {
	s.configMu.Lock()
	s.config = cfg
	s.configMu.Unlock()
}

// Set holds every workspace the core serves, with one of them marked default.
// The default answers requests that name no directory, so a client written
// against a single-workspace core keeps working.
type Set struct {
	mu    sync.RWMutex
	def   string
	byKey map[string]*Services
	order []string
}

// NewSet creates an empty set.
func NewSet() *Set {
	return &Set{byKey: map[string]*Services{}}
}

// Put stores a workspace's services, replacing an existing entry for the same
// directory. The first workspace inserted becomes the default.
func (s *Set) Put(services *Services) {
	if services == nil {
		return
	}
	key := services.Ref.Key()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byKey[key]; !exists {
		s.order = append(s.order, key)
	}
	s.byKey[key] = services
	if s.def == "" {
		s.def = key
	}
}

// Get returns the services for one directory key.
func (s *Set) Get(key string) (*Services, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	services, ok := s.byKey[key]
	return services, ok
}

// Resolve returns the services for a directory, or false when that directory is
// not open in this process.
func (s *Set) Resolve(directory string) (*Services, bool) {
	ref, err := Make(directory)
	if err != nil {
		return nil, false
	}
	return s.Get(ref.Key())
}

// Default returns the workspace that answers unnamed requests, or nil before
// the first one is added.
func (s *Set) Default() *Services {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byKey[s.def]
}

// SetDefault marks one directory as the default. It reports false when the
// directory is not open.
func (s *Set) SetDefault(directory string) bool {
	ref, err := Make(directory)
	if err != nil {
		return false
	}
	key := ref.Key()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byKey[key]; !ok {
		return false
	}
	s.def = key
	return true
}

// List returns every workspace in insertion order.
func (s *Set) List() []*Services {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Services, 0, len(s.order))
	for _, key := range s.order {
		if services, ok := s.byKey[key]; ok {
			out = append(out, services)
		}
	}
	return out
}

// Remove drops one workspace and returns it so the caller can close it.
func (s *Set) Remove(directory string) *Services {
	ref, err := Make(directory)
	if err != nil {
		return nil
	}
	key := ref.Key()
	s.mu.Lock()
	defer s.mu.Unlock()
	services, ok := s.byKey[key]
	if !ok {
		return nil
	}
	delete(s.byKey, key)
	for i, item := range s.order {
		if item == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	if s.def == key {
		s.def = ""
		if len(s.order) > 0 {
			s.def = s.order[0]
		}
	}
	return services
}

// Close releases every workspace this set holds.
func (s *Set) Close() {
	for _, services := range s.List() {
		services.Close()
	}
}

// Close releases the child processes and watchers this workspace started. It
// is idempotent, so shutting a core down twice is harmless.
func (s *Services) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.CloseFn != nil {
			s.CloseFn()
		}
	})
}

// Root is the workspace directory.
func (s *Services) Root() string {
	if s == nil {
		return ""
	}
	return s.Ref.Directory
}
