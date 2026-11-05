// Package app is the only place that knows how dependencies are wired.
package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
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
	catalog, err := buildCatalog(cfg)
	if err != nil {
		cancel()
		_ = store.Close()
		return nil, fmt.Errorf("provider catalog: %w", err)
	}
	rules := make(permission.Rules, 0, len(cfg.Permission))
	for _, r := range cfg.Permission {
		rules = append(rules, permission.Rule{Action: r.Action, Resource: r.Resource, Effect: permission.Effect(r.Effect)})
	}
	perm := permission.New(rules)
	if cfg.Path != "" {
		path := cfg.Path
		perm.SetSaver(func(rules permission.Rules) error {
			out := make([]config.Rule, 0, len(rules))
			for _, rule := range rules {
				out = append(out, config.Rule{
					Action:   rule.Action,
					Resource: rule.Resource,
					Effect:   string(rule.Effect),
				})
			}
			return config.SavePermission(path, out)
		})
	}

	runner := agent.New(store, catalog, registry, perm, agent.Options{
		MaxSteps: cfg.MaxSteps,
		SystemFunc: func() string {
			return prompt.Build(prompt.Context{
				Workdir: workdir,
				Shell:   cfg.Shell,
				Extra:   cfg.SystemPrompt,
				Now:     time.Now(),
			})
		},
		CompactKeep: cfg.CompactKeep,
		Base:        base,
	})
	return &App{
		Config: cfg,
		Agent:  runner,
		Server: server.New(runner, store),
		Store:  store,
		cancel: cancel,
	}, nil
}

// Recover resumes sessions a previous process left with an open turn claim or
// undelivered input. It starts the turns and returns; their events are durable.
func (a *App) Recover(ctx context.Context) error {
	list, err := a.Store.Recoveries()
	if err != nil {
		return err
	}
	for _, item := range list {
		meta, _ := a.Store.Meta(item.SessionID)
		events, err := a.Agent.Resume(ctx, item.SessionID, item.ClaimID, provider.Selection{
			Provider: meta.Provider,
			Model:    meta.Model,
		})
		if err != nil {
			log.Printf("recover %s: %v", item.SessionID, err)
			continue
		}
		go func() {
			for range events {
			}
		}()
	}
	return nil
}

// Shutdown interrupts every running turn and waits for them to finish, or for
// ctx to end first.
func (a *App) Shutdown(ctx context.Context) error {
	a.cancel()
	return a.Agent.Wait(ctx)
}

// buildCatalog maps config providers onto the provider package. Configs that do
// not declare providers keep the legacy single endpoint and model; once a
// providers block exists, the top-level base_url/api_key are ignored.
func buildCatalog(cfg config.Config) (*provider.Catalog, error) {
	specs := make([]provider.ProviderSpec, 0, len(cfg.Providers)+1)
	ids := make([]string, 0, len(cfg.Providers))
	for id := range cfg.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := cfg.Providers[id]
		key := p.APIKey
		if key == "" && p.APIKeyEnv != "" {
			key = os.Getenv(p.APIKeyEnv)
		}
		models := make([]provider.ModelSpec, 0, len(p.Models))
		for _, model := range p.Models {
			window := cfg.ContextWindow
			if model.ContextWindow != nil {
				window = *model.ContextWindow
			}
			noTools := model.ToolCall != nil && !*model.ToolCall
			models = append(models, provider.ModelSpec{
				ID:            model.ID,
				Name:          model.Name,
				ContextWindow: window,
				Options:       model.Options,
				Variants:      model.Variants,
				NoTools:       noTools,
				Reasoning:     model.Reasoning,
				Attachment:    model.Attachment,
				MaxOutput:     model.MaxOutputTokens,
			})
		}
		specs = append(specs, provider.ProviderSpec{
			ID:        id,
			Name:      p.Name,
			BaseURL:   p.BaseURL,
			APIKey:    key,
			Models:    models,
			Blacklist: p.Blacklist,
			Whitelist: p.Whitelist,
		})
	}

	legacy := !cfg.ProvidersSet && len(cfg.Providers) == 0
	if legacy {
		models := []provider.ModelSpec{}
		if cfg.Model != "" {
			models = append(models, provider.ModelSpec{
				ID:            cfg.Model,
				Name:          cfg.Model,
				ContextWindow: cfg.ContextWindow,
			})
		}
		specs = append(specs, provider.ProviderSpec{
			ID:      "default",
			Name:    "Default",
			BaseURL: cfg.BaseURL,
			APIKey:  cfg.APIKey,
			Models:  models,
		})
	}
	selected := cfg.Provider
	if selected == "" && legacy {
		selected = "default"
	}
	return provider.NewCatalog(specs, selected, cfg.Model)
}

// resolveWorkdir turns the configured working directory into an absolute path,
// defaulting to the process working directory.
func resolveWorkdir(dir string) (string, error) {
	if dir == "" {
		return os.Getwd()
	}
	return filepath.Abs(dir)
}
