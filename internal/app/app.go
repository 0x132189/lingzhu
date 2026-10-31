// Package app is the only place that knows how dependencies are wired.
package app

import (
	"fmt"

	"lingzhu/internal/agent"
	"lingzhu/internal/config"
	"lingzhu/internal/permission"
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
}

func New(cfg config.Config) (*App, error) {
	store, err := session.Open(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	registry := tool.NewRegistry()
	for _, t := range tool.Builtins(cfg.Shell, cfg.DataDir) {
		registry.Add(t)
	}
	prov := provider.NewOpenAI(cfg.BaseURL, cfg.APIKey)
	rules := make(permission.Rules, 0, len(cfg.Permission))
	for _, r := range cfg.Permission {
		rules = append(rules, permission.Rule{Action: r.Action, Resource: r.Resource, Effect: permission.Effect(r.Effect)})
	}
	perm := permission.New(rules)
	runner := agent.New(store, prov, registry, perm, cfg.Model, cfg.MaxSteps)
	return &App{
		Config: cfg,
		Agent:  runner,
		Server: server.New(runner, store),
		Store:  store,
	}, nil
}
