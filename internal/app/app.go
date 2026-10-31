// Package app is the only place that knows how dependencies are wired.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"lingzhu/internal/agent"
	"lingzhu/internal/config"
	"lingzhu/internal/permission"
	"lingzhu/internal/prompt"
	"lingzhu/internal/provider"
	"lingzhu/internal/server"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
)

type App struct {
	Config config.Config
	Agent  *agent.Agent
	Server *server.Server
	Store  *session.Store

	cancel context.CancelFunc
}

func New(cfg config.Config) (*App, error) {
	workdir, err := resolveWorkdir(cfg.Workdir)
	if err != nil {
		return nil, fmt.Errorf("resolve workdir: %w", err)
	}
	store, err := session.Open(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	base, cancel := context.WithCancel(context.Background())
	registry := tool.NewRegistry()
	for _, t := range tool.Builtins(cfg.Shell, workdir) {
		registry.Add(t)
	}
	prov := provider.NewOpenAI(cfg.BaseURL, cfg.APIKey)
	rules := make(permission.Rules, 0, len(cfg.Permission))
	for _, r := range cfg.Permission {
		rules = append(rules, permission.Rule{Action: r.Action, Resource: r.Resource, Effect: permission.Effect(r.Effect)})
	}
	perm := permission.New(rules)

	system := prompt.Build(prompt.Context{
		Workdir: workdir,
		Shell:   cfg.Shell,
		Extra:   cfg.SystemPrompt,
		Now:     time.Now(),
	})
	runner := agent.New(store, prov, registry, perm, agent.Options{
		Model:         cfg.Model,
		MaxSteps:      cfg.MaxSteps,
		System:        system,
		ContextWindow: cfg.ContextWindow,
		CompactKeep:   cfg.CompactKeep,
		Base:          base,
	})
	return &App{
		Config: cfg,
		Agent:  runner,
		Server: server.New(runner, store),
		Store:  store,
		cancel: cancel,
	}, nil
}

// Shutdown interrupts every running turn and waits for them to finish, or for
// ctx to end first.
func (a *App) Shutdown(ctx context.Context) error {
	a.cancel()
	return a.Agent.Wait(ctx)
}

// resolveWorkdir turns the configured working directory into an absolute path,
// defaulting to the process working directory.
func resolveWorkdir(dir string) (string, error) {
	if dir == "" {
		return os.Getwd()
	}
	return filepath.Abs(dir)
}
