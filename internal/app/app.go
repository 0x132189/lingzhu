package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lingzhu/internal/agent"
	"lingzhu/internal/artifact"
	"lingzhu/internal/auth"
	"lingzhu/internal/config"
	"lingzhu/internal/event"
	"lingzhu/internal/imagegen"
	"lingzhu/internal/job"
	"lingzhu/internal/location"
	"lingzhu/internal/mcp"
	"lingzhu/internal/oauth"
	"lingzhu/internal/plugin"
	"lingzhu/internal/policy"
	"lingzhu/internal/provider"
	"lingzhu/internal/pty"
	"lingzhu/internal/question"
	"lingzhu/internal/schedule"
	"lingzhu/internal/server"
	"lingzhu/internal/session"
	"lingzhu/internal/share"
	"lingzhu/internal/skill"
	"lingzhu/internal/subagent"
	"lingzhu/internal/websearch"
)

// App is one core process. It owns the managers keyed by data directory and a
// set of workspaces, each with its own tools, snapshots, terminals and agents.
type App struct {
	Config config.Config
	// Agent, Store and the managers below belong to the default workspace. A
	// core that serves several directories reaches the others through
	// Locations.
	Agent     *agent.Agent
	Server    *server.Server
	Store     *session.Store
	MCP       *mcp.Manager
	Jobs      *job.Manager
	Skills    *skill.Manager
	Plugins   *plugin.Manager
	Schedules *schedule.Manager
	Shares    *share.Manager
	PTY       *pty.Manager
	Subagents *subagent.Manager
	// Locations holds every open workspace, including the default one.
	Locations *location.Set
	// Token is the API token this core requires. It is empty when the token
	// could not be created, which leaves the API unauthenticated.
	Token string

	shared *shared
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

	artifacts, artifactErr := artifact.New(cfg.DataDir, cfg.Artifact)
	if artifactErr != nil {
		log.Printf("artifacts: disabled: %v", artifactErr)
	}
	schedules, scheduleErr := schedule.New(cfg.DataDir, cfg.Schedule, log.Printf)
	if scheduleErr != nil {
		log.Printf("schedules: disabled: %v", scheduleErr)
	}
	shares, shareErr := share.New(cfg.DataDir, cfg.Share)
	if shareErr != nil {
		log.Printf("shares: disabled: %v", shareErr)
	} else {
		shares.SetLogger(log.Printf)
		if status := shares.Status(); status.Enabled {
			if status.Remote {
				log.Printf("share: %s via %s", status.Mode, status.Endpoint)
			} else {
				log.Printf("share: %s (local)", status.Mode)
			}
		}
	}
	oauthManager, oauthErr := oauth.New(cfg.DataDir, cfg.OAuth)
	if oauthErr != nil {
		log.Printf("oauth: disabled: %v", oauthErr)
	}
	// Background jobs are recorded under the data directory, so a restart keeps
	// their command, status and captured output instead of dropping every one.
	jobManager, jobErr := job.Open(filepath.Join(cfg.DataDir, "jobs"))
	if jobErr != nil {
		log.Printf("jobs: persistence disabled: %v", jobErr)
		jobManager = job.New()
	}

	core := &shared{
		base:      base,
		store:     store,
		jobs:      jobManager,
		questions: question.New(),
		artifacts: artifacts,
		schedules: schedules,
		shares:    shares,
		oauth:     oauthManager,
		websearch: websearch.New(cfg.WebSearch),
		image:     imagegen.New(cfg.Image),
		log:       log.Printf,
	}

	ref, err := location.Make(workdir)
	if err != nil {
		cancel()
		_ = store.Close()
		return nil, err
	}
	defaults, err := core.openWorkspace(ref, cfg)
	if err != nil {
		cancel()
		_ = store.Close()
		return nil, fmt.Errorf("provider catalog: %w", err)
	}
	set := location.NewSet()
	set.Put(defaults)

	if schedules != nil {
		schedules.SetFire(func(item schedule.Item) error {
			return defaults.Agent.Notify(item.SessionID, event.ScheduleFiredEvent(item.ID, item.Name, item.Prompt))
		})
		schedules.Start(base)
	}
	if shares != nil && shares.Auto() {
		go autoShareSessions(base, defaults.Agent, store, shares)
	}
	core.jobs.SetNotifier(func(item job.Job) {
		notification := event.JobCompletedEvent(item.ID, item.Command, string(item.Status), item.File, item.Notification(), item.ExitCode)
		if err := defaults.Agent.Notify(item.SessionID, notification); err != nil {
			log.Printf("job %s notify: %v", item.ID, err)
		}
	})

	srv := server.New(defaults.Agent, store)
	srv.SetLocations(set)
	srv.SetConfig(cfg)
	srv.SetWorkdir(workdir)
	srv.SetAgents(defaults.Agents)
	srv.SetPlugins(defaults.Plugins)
	srv.SetSkills(defaults.Skills)
	srv.SetSubagents(defaults.Subagents)
	srv.SetCommands(defaults.Commands)
	srv.SetMCP(defaults.MCP)
	srv.SetJobs(core.jobs)
	srv.SetSnapshots(defaults.Snapshots)
	srv.SetWorktrees(defaults.Worktrees)
	srv.SetFormatters(defaults.Formatter)
	srv.SetShares(shares)
	srv.SetPTY(defaults.PTY)
	srv.SetVCS(defaults.VCS)
	srv.SetWebSearch(core.websearch)
	srv.SetImage(core.image)
	srv.SetArtifacts(artifacts)
	srv.SetSchedules(schedules)
	srv.SetOAuth(oauthManager)
	// The API is reachable by any local process and by any page the browser
	// loads, so every request carries a token generated for this data
	// directory. A failure here is fatal for the server, not for the run.
	token, tokenErr := auth.Token(cfg.DataDir)
	if tokenErr != nil {
		log.Printf("auth: token unavailable, API is unauthenticated: %v", tokenErr)
	} else {
		srv.SetAuthToken(token)
	}
	application := &App{
		Config:    cfg,
		Agent:     defaults.Agent,
		Server:    srv,
		Store:     store,
		MCP:       defaults.MCP,
		Jobs:      core.jobs,
		Skills:    defaults.Skills,
		Plugins:   defaults.Plugins,
		Schedules: schedules,
		Shares:    shares,
		PTY:       defaults.PTY,
		Subagents: defaults.Subagents,
		Locations: set,
		Token:     token,
		shared:    core,
		cancel:    cancel,
	}
	// Only the app package knows how to build a workspace, so the server calls
	// back into it instead of importing the wiring.
	srv.SetLocationHooks(
		func(directory string) (string, error) {
			ref, err := application.OpenWorkspace(directory)
			return ref.Directory, err
		},
		application.CloseWorkspace,
	)
	return application, nil
}

// OpenWorkspace adds one directory to a running core. The first call for a
// directory builds its whole service tree; later calls are a no-op. The
// directory must exist.
func (a *App) OpenWorkspace(directory string) (location.Ref, error) {
	ref, err := location.Make(directory)
	if err != nil {
		return location.Ref{}, err
	}
	if _, ok := a.Locations.Get(ref.Key()); ok {
		return ref, nil
	}
	if !ref.Exists() {
		return location.Ref{}, fmt.Errorf("workspace: %s is not a directory", ref.Directory)
	}
	cfg, err := config.ForDir(ref.Directory, a.Config.Path)
	if err != nil {
		return location.Ref{}, err
	}
	cfg.DataDir = a.Config.DataDir
	if strings.TrimSpace(cfg.Addr) == "" {
		cfg.Addr = a.Config.Addr
	}
	services, err := a.shared.openWorkspace(ref, cfg)
	if err != nil {
		return location.Ref{}, err
	}
	a.Locations.Put(services)
	return ref, nil
}

// CloseWorkspace stops one workspace and forgets it.
func (a *App) CloseWorkspace(directory string) bool {
	services := a.Locations.Remove(directory)
	if services == nil {
		return false
	}
	services.Close()
	return true
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
		events, err := a.agentFor(item.SessionID).Resume(ctx, item.SessionID, item.ClaimID, provider.Selection{
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
	for _, services := range a.Locations.List() {
		if services.Subagents != nil {
			services.Subagents.Recover(ctx)
		}
	}
	if a.Server != nil {
		a.Server.ResumeMoves(ctx)
	}
	return nil
}

// agentFor is the agent loop of the workspace a session belongs to.
func (a *App) agentFor(sessionID string) *agent.Agent {
	if a.Locations != nil {
		if meta, err := a.Store.Meta(sessionID); err == nil && meta.Directory != "" {
			if services, ok := a.Locations.Resolve(meta.Directory); ok && services.Agent != nil {
				return services.Agent
			}
		}
		if services := a.Locations.Default(); services != nil && services.Agent != nil {
			return services.Agent
		}
	}
	return a.Agent
}

// Shutdown interrupts every running turn and waits for them to finish, or for
// ctx to end first.
func (a *App) Shutdown(ctx context.Context) error {
	a.cancel()
	err := a.Agent.Wait(ctx)
	for _, services := range a.Locations.List() {
		if services.Agent != nil && services.Agent != a.Agent {
			if waitErr := services.Agent.Wait(ctx); waitErr != nil && err == nil {
				err = waitErr
			}
		}
		services.Close()
	}
	if a.MCP != nil {
		a.MCP.Close()
	}
	if a.Plugins != nil {
		a.Plugins.Close()
	}
	if a.Schedules != nil {
		a.Schedules.Close()
	}
	if a.PTY != nil {
		a.PTY.CloseAll()
	}
	if a.Jobs != nil {
		a.Jobs.Close()
	}
	return err
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
			cacheTTL, _ := time.ParseDuration(model.CacheTTL)
			nativeCompaction := false
			compactionTrigger := 0
			if model.Compaction != nil {
				nativeCompaction = strings.EqualFold(model.Compaction.Type, "native")
				compactionTrigger = model.Compaction.TriggerTokens
			}
			models = append(models, provider.ModelSpec{
				ID:                model.ID,
				Name:              model.Name,
				ContextWindow:     window,
				Options:           model.Options,
				Variants:          model.Variants,
				NoTools:           noTools,
				Reasoning:         model.Reasoning,
				Attachment:        model.Attachment,
				CacheTTL:          cacheTTL,
				MaxOutput:         model.MaxOutputTokens,
				Type:              model.Type,
				API:               model.API,
				Family:            model.Family,
				Released:          model.Released,
				NativeCompaction:  nativeCompaction,
				CompactionTrigger: compactionTrigger,
			})
		}
		specs = append(specs, provider.ProviderSpec{
			ID:        id,
			Name:      p.Name,
			BaseURL:   p.BaseURL,
			APIKey:    key,
			API:       p.API,
			OAuth:     p.OAuth != "",
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

// autoShareSessions keeps every finished session shared when share.mode is
// auto. Subagent sessions are skipped so the sidebar only sees parent shares.
func autoShareSessions(ctx context.Context, runner *agent.Agent, store *session.Store, manager *share.Manager) {
	sub := runner.SubscribeAll()
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-sub.Events():
			if !ok {
				return
			}
			if item.Event.Type != event.RunEnd || strings.HasPrefix(item.SessionID, "sub_") {
				continue
			}
			go func(id string) {
				info, err := store.Info(id)
				if err != nil {
					return
				}
				messages, err := store.Messages(id)
				if err != nil {
					return
				}
				title := info.Title
				if strings.TrimSpace(title) == "" {
					title = "Session " + id
				}
				if _, err := manager.Create(id, title, messages); err != nil {
					log.Printf("share: auto %s: %v", id, err)
				}
			}(item.SessionID)
		}
	}
}

func filterMCPConfig(cfg mcp.Config, policies policy.Rules, logf func(string, ...any)) mcp.Config {
	for name := range cfg.Servers {
		if policies.Denied("mcp.use", name) {
			logf("policy: mcp server %q is blocked", name)
			delete(cfg.Servers, name)
		}
	}
	return cfg
}

func filterPluginConfig(cfg plugin.Config, policies policy.Rules, logf func(string, ...any)) plugin.Config {
	for name := range cfg.Plugins {
		if policies.Denied("plugin.use", name) {
			logf("policy: plugin %q is blocked", name)
			delete(cfg.Plugins, name)
		}
	}
	return cfg
}
