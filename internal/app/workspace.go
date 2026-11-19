package app

import (
	"context"
	"sync"
	"time"

	"lingzhu/internal/agent"
	"lingzhu/internal/agents"
	apptools "lingzhu/internal/apptools"
	"lingzhu/internal/artifact"
	customcommands "lingzhu/internal/commands"
	"lingzhu/internal/config"
	"lingzhu/internal/format"
	"lingzhu/internal/global"
	"lingzhu/internal/imagegen"
	"lingzhu/internal/job"
	"lingzhu/internal/location"
	"lingzhu/internal/mcp"
	"lingzhu/internal/oauth"
	"lingzhu/internal/permission"
	"lingzhu/internal/plugin"
	"lingzhu/internal/prompt"
	"lingzhu/internal/provider"
	"lingzhu/internal/pty"
	"lingzhu/internal/question"
	"lingzhu/internal/schedule"
	"lingzhu/internal/session"
	"lingzhu/internal/share"
	"lingzhu/internal/skill"
	"lingzhu/internal/snapshot"
	"lingzhu/internal/subagent"
	"lingzhu/internal/tool"
	"lingzhu/internal/vcs"
	"lingzhu/internal/websearch"
	"lingzhu/internal/worktree"
)

// shared is what every workspace in one core uses: the session store, the
// managers keyed by data directory, and the model-independent helpers. Nothing
// here reads a project's files, so sharing it cannot leak one project into
// another.
type shared struct {
	base      context.Context
	store     *session.Store
	jobs      *job.Manager
	questions *question.Manager
	artifacts *artifact.Manager
	schedules *schedule.Manager
	shares    *share.Manager
	oauth     *oauth.Manager
	websearch *websearch.Manager
	image     *imagegen.Manager
	log       func(string, ...any)
}

// openWorkspace builds the service tree for one directory. Every manager it
// creates reads that directory's own files, so two workspaces in one process
// keep separate tools, snapshots, terminals, MCP servers and profiles.
func (s *shared) openWorkspace(ref location.Ref, cfg config.Config) (*location.Services, error) {
	workdir := ref.Directory
	policies := cfg.Policies

	discovered := agents.Discover(agents.Roots(cfg.Path, workdir), s.log)
	if len(discovered) > 0 {
		s.log("agents: %s loaded %d markdown", ref.Name(), len(discovered))
	}
	profiles := agents.New(cfg.Agents.Overlay(discovered), cfg.DefaultAgent)
	go watchAgents(s.base, profiles, cfg, workdir)

	formatters := format.New(workdir, cfg.Formatter, s.log)
	snapshots, snapErr := snapshot.New(workdir, global.Cache())
	if snapErr != nil {
		s.log("snapshots: %s disabled: %v", ref.Name(), snapErr)
	}
	worktrees, wtErr := worktree.New(workdir, cfg.DataDir)
	if wtErr != nil {
		s.log("worktrees: %s disabled: %v", ref.Name(), wtErr)
	}
	terminals, ptyErr := pty.New(workdir, cfg.Shell, cfg.DataDir)
	if ptyErr != nil {
		s.log("pty: %s disabled: %v", ref.Name(), ptyErr)
	}

	skillRoots := skill.Roots(cfg.Skills.Paths, cfg.Path, workdir)
	if len(cfg.Skills.URLs) > 0 {
		result := skill.PullStatus(s.base, cfg.Skills.URLs, global.Cache(), s.log)
		if len(result.Roots) > 0 {
			s.log("skills: %s pulled %d remote", ref.Name(), len(result.Roots))
		}
		for _, item := range result.Errors {
			s.log("skills: %s source %s: %s", ref.Name(), item.URL, item.Error)
		}
		skillRoots = append(result.Roots, skillRoots...)
	}
	skills := skill.DiscoverWithFiles(skillRoots, s.log, cfg.Skills.MaxFiles)
	skillKeep := func(id string) bool { return !policies.Denied("skill.use", id) }
	skills.Filter(skillKeep)
	skillState := newSkillsState(skills, skillRoots, cfg, workdir, s.log, skillKeep)
	skillCtx, stopSkills := context.WithCancel(s.base)
	go skillState.watch(skillCtx)

	commandRegistry := customcommands.New(
		cfg.Commands,
		customcommands.Discover(customcommands.Roots(cfg.Path, workdir), cfg.Commands, s.log),
	)

	registry := tool.NewRegistry()
	for _, t := range tool.Builtins(cfg.Shell, workdir, s.jobs, cfg.Media) {
		registry.Add(t)
	}
	registry.Add(tool.Direct(question.NewTool(s.questions)))
	registry.Add(tool.Direct(skill.NewTool(skills)))
	if s.artifacts.Enabled() {
		for _, t := range artifact.Tools(s.artifacts) {
			registry.Add(tool.Codemode(t))
		}
	}
	if s.schedules != nil {
		for _, t := range schedule.Tools(s.schedules) {
			registry.Add(tool.Codemode(t))
		}
	}
	if s.image.Configured() {
		registry.Add(tool.Direct(imagegen.Tool(s.image, workdir)))
	}
	if s.websearch.Configured() {
		registry.Add(tool.Direct(websearch.Tool(s.websearch)))
	}

	var mcpTokens mcp.TokenFunc
	if s.oauth != nil {
		mcpTokens = func(ctx context.Context, endpoint string, force bool) (string, error) {
			if force {
				token, err := s.oauth.Refresh(ctx, endpoint)
				return token.AccessToken, err
			}
			token, _, err := s.oauth.AccessTokenOptional(ctx, endpoint)
			return token, err
		}
	}
	mcpManager := mcp.StartBackground(s.base, filterMCPConfig(cfg.MCP, policies, s.log), workdir, s.log, mcpTokens)
	if s.oauth != nil {
		mcpManager.SetOAuth(s.oauth, oauthRedirect(cfg.Addr))
	}
	// MCP catalogs can change while the workspace is open. Keep the registry
	// in sync without letting a remote server shadow a built-in or app tool.
	var mcpToolsMu sync.Mutex
	mcpToolNames := map[string]bool{}
	syncMCPTools := func() {
		mcpToolsMu.Lock()
		defer mcpToolsMu.Unlock()
		next := mcpManager.Tools()
		nextNames := make(map[string]bool, len(next))
		for _, item := range next {
			nextNames[item.Name()] = true
		}
		for name := range mcpToolNames {
			if !nextNames[name] {
				registry.Remove(name)
				delete(mcpToolNames, name)
			}
		}
		for _, item := range next {
			name := item.Name()
			if _, exists := registry.Get(name); exists && !mcpToolNames[name] {
				s.log("mcp: %s tool %q collides with an existing tool", ref.Name(), name)
				continue
			}
			if aware, ok := item.(interface{ Codemode() bool }); ok && !aware.Codemode() {
				registry.Add(item)
			} else {
				registry.Add(tool.Codemode(item))
			}
			mcpToolNames[name] = true
		}
	}
	for _, t := range mcp.ResourceTools(mcpManager) {
		registry.Add(tool.Codemode(t))
	}
	for _, t := range mcp.EventTools(mcpManager) {
		registry.Add(tool.Codemode(t))
	}
	plugins := plugin.Start(s.base, filterPluginConfig(cfg.Plugins, policies, s.log), workdir, s.log)
	for _, t := range plugins.Tools() {
		if _, exists := registry.Get(t.Name()); exists {
			s.log("plugin: %s tool %q overrides an existing tool", ref.Name(), t.Name())
		}
		registry.Add(tool.Codemode(t))
	}
	execTool := tool.NewExecute(registry, workdir)
	execTool.AllTools = cfg.CodemodeOnly
	registry.Add(execTool)

	catalog, err := buildCatalog(cfg)
	if err != nil {
		stopSkills()
		plugins.Close()
		mcpManager.Close()
		return nil, err
	}
	catalog.SetPolicies(policies)
	if s.oauth != nil {
		for id, p := range cfg.Providers {
			if p.OAuth == "" {
				continue
			}
			name := p.OAuth
			if _, ok := s.oauth.Status(name); !ok {
				s.log("oauth: provider %s references unknown endpoint %q", id, name)
				continue
			}
			if !catalog.SetTokenSource(id, func(ctx context.Context) (string, error) {
				return s.oauth.AccessToken(ctx, name)
			}) {
				s.log("oauth: provider %s does not accept a token source", id)
			}
		}
	}

	rules := make(permission.Rules, 0, len(cfg.Permission))
	for _, r := range cfg.Permission {
		rules = append(rules, permission.Rule{Action: r.Action, Resource: r.Resource, Effect: permission.Effect(r.Effect)})
	}
	saved := make(permission.Rules, 0, len(cfg.PermissionAlways))
	for _, r := range cfg.PermissionAlways {
		saved = append(saved, permission.Rule{Action: r.Action, Resource: r.Resource, Effect: permission.Effect(r.Effect)})
	}
	perm := permission.New(rules)
	perm.SetSaved(saved)
	perm.SetPolicies(policies)
	perm.SetEvaluator(plugins.Evaluate)
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

	runner := agent.New(s.store, catalog, registry, perm, agent.Options{
		MaxSteps: cfg.MaxSteps,
		SystemPartsFunc: func(sessionID string, target provider.Target) prompt.Parts {
			profile := profiles.Default()
			if sessionID != "" {
				if meta, err := s.store.Meta(sessionID); err == nil {
					profile = profiles.Resolve(meta.Agent)
				}
			}
			allowed := func(name string) bool { return profile.AllowsTool(name) }
			defs := registry.Definitions()
			names := make([]string, 0, len(defs))
			for _, def := range defs {
				if cfg.CodemodeOnly && def.Name != "execute" {
					continue
				}
				if !allowed(def.Name) {
					continue
				}
				names = append(names, def.Name)
			}
			catalogForPrompt := registry.CatalogFor(allowed)
			if cfg.CodemodeOnly {
				catalogForPrompt = registry.CatalogExecuteOnlyFor(allowed)
			}
			var servers []prompt.MCPServer
			mcpTools := mcpManager.Tools()
			for _, item := range mcpManager.Instructions() {
				visible := false
				for _, candidate := range mcpTools {
					owner, ok := candidate.(interface{ MCPServer() string })
					if ok && owner.MCPServer() == item.Server && profile.AllowsTool(candidate.Name()) {
						visible = true
						break
					}
				}
				if visible {
					servers = append(servers, prompt.MCPServer{Name: item.Server, Instructions: item.Instructions})
				}
			}
			var references []prompt.Reference
			for _, item := range cfg.References {
				references = append(references, prompt.Reference{
					Name:        item.Name,
					Path:        item.Path,
					Description: item.Description,
				})
			}
			// A custom agent system replaces the model-specific base prompt.
			// Plan stays a tail reminder, so it is excluded here.
			agentSystem := ""
			if profile.ID != "plan" {
				agentSystem = profile.System
			}
			availableSkills := skills.Summaries()
			rules := append(permission.Rules(nil), profile.Permissions...)
			if sessionID != "" {
				if extra, err := s.store.Permissions(sessionID); err == nil {
					rules = append(rules, extra...)
				}
			}
			if len(rules) > 0 {
				filtered := availableSkills[:0]
				for _, item := range availableSkills {
					if rules.Evaluate("skill", item.ID) != permission.Deny {
						filtered = append(filtered, item)
					}
				}
				availableSkills = filtered
			}
			return prompt.BuildParts(prompt.Context{
				Workdir:      workdir,
				Shell:        cfg.Shell,
				Extra:        cfg.SystemPrompt,
				Skills:       availableSkills,
				AgentSystem:  agentSystem,
				MCP:          servers,
				References:   references,
				MCPCodemode:  cfg.CodemodeOnly,
				Tools:        names,
				Catalog:      catalogForPrompt,
				Instructions: cfg.Instructions,
				Model:        target.Model,
				RequestModel: target.RequestModel,
				ModelName:    target.ModelName,
				ModelPrompts: cfg.ModelPrompts,
				Now:          time.Now(),
			})
		},
		CompactKeep:  cfg.CompactKeep,
		Precompute:   true,
		CodemodeOnly: cfg.CodemodeOnly,
		Titles:       cfg.Titles,
		Questions:    s.questions,
		Agents:       profiles,
		Warming:      cfg.Warming,
		Snapshots:    snapshots,
		Formatter:    formatters,
		Hooks:        plugins,
		Base:         s.base,
	})

	mcpManager.SetSampling(samplingHandler(runner))
	mcpManager.SetElicitation(elicitationHandler(runner))
	subagents := subagent.New(runner, s.store, s.log)
	subagents.SetAgents(profiles)
	subagents.SetDepth(cfg.SubagentDepth)
	registry.Add(tool.Direct(subagents.Tool()))
	for _, t := range apptools.Tools(s.store, worktrees, catalog) {
		registry.Add(t)
	}
	// Register MCP last so a remote server cannot shadow a built-in, app or
	// plugin tool, and keep the registry current when a catalog changes.
	syncMCPTools()
	mcpManager.SetOnChange(syncMCPTools)

	var vcsManager *vcs.Manager
	if manager, err := vcs.New(workdir); err == nil {
		vcsManager = manager
	} else {
		s.log("vcs: %s disabled: %v", ref.Name(), err)
	}

	services := &location.Services{
		Ref:           ref,
		Agent:         runner,
		Agents:        profiles,
		Tools:         registry,
		Permission:    perm,
		Snapshots:     snapshots,
		Formatter:     formatters,
		Worktrees:     worktrees,
		PTY:           terminals,
		VCS:           vcsManager,
		MCP:           mcpManager,
		Plugins:       plugins,
		Skills:        skills,
		Commands:      commandRegistry,
		Subagents:     subagents,
		RefreshSkills: skillState.refresh,
		CreateSkill:   skillState.CreateSkill,
		DeleteSkill:   skillState.DeleteSkill,
		SkillErrors:   skillState.SourceErrors,
	}
	services.SetConfig(cfg)
	services.CloseFn = func() {
		stopSkills()
		plugins.Close()
		mcpManager.Close()
		if terminals != nil {
			terminals.CloseAll()
		}
	}
	return services, nil
}
