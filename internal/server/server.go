// Package server turns the agent event stream into HTTP + SSE.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lingzhu/internal/agent"
	"lingzhu/internal/agents"
	"lingzhu/internal/artifact"
	"lingzhu/internal/auth"
	"lingzhu/internal/commands"
	"lingzhu/internal/config"
	"lingzhu/internal/event"
	"lingzhu/internal/format"
	"lingzhu/internal/imagegen"
	"lingzhu/internal/job"
	"lingzhu/internal/location"
	"lingzhu/internal/mcp"
	"lingzhu/internal/model"
	"lingzhu/internal/oauth"
	"lingzhu/internal/permission"
	"lingzhu/internal/plugin"
	"lingzhu/internal/provider"
	"lingzhu/internal/pty"
	"lingzhu/internal/question"
	"lingzhu/internal/schedule"
	"lingzhu/internal/session"
	"lingzhu/internal/share"
	"lingzhu/internal/skill"
	"lingzhu/internal/subagent"
	"lingzhu/internal/vcs"
	"lingzhu/internal/websearch"
	"lingzhu/internal/worktree"
)

type Server struct {
	agent      *agent.Agent
	store      *session.Store
	agents     *agents.Registry
	plugins    *plugin.Manager
	commands   *commands.Registry
	skills     *skill.Manager
	subagents  *subagent.Manager
	mcp        *mcp.Manager
	jobs       *job.Manager
	root       string
	snapshots  Snapshots
	worktrees  *worktree.Manager
	formatters *format.Manager
	shares     *share.Manager
	config     config.Config
	configMu   sync.Mutex
	vcs        *vcs.Manager
	pty        *pty.Manager
	oauth      *oauth.Manager
	websearch  *websearch.Manager
	image      *imagegen.Manager
	artifacts  *artifact.Manager
	schedules  *schedule.Manager
	pairings   *pairingManager
	done       chan struct{}
	once       sync.Once
	onShutdown func()
	moveMu     sync.Mutex
	moveWait   map[string]bool
	// authToken is the bearer token required for every non-public request.
	authToken string
	// locations holds every workspace this core serves. The fields above are
	// the default workspace's services; a session that names another directory
	// is routed to that workspace's tree instead.
	locations *location.Set
	// openLocationFn and closeLocationFn let the HTTP layer add or drop a
	// workspace; only the app package knows how to build one.
	openLocationFn  func(directory string) (string, error)
	closeLocationFn func(directory string) bool
}

// SetLocationHooks installs the callbacks that open and close a workspace.
func (s *Server) SetLocationHooks(open func(string) (string, error), close func(string) bool) {
	s.openLocationFn = open
	s.closeLocationFn = close
}

func New(a *agent.Agent, store *session.Store) *Server {
	return &Server{agent: a, store: store, done: make(chan struct{}), pairings: newPairingManager(), moveWait: map[string]bool{}}
}

// SetLocations attaches the multi-workspace service set. Without it the server
// keeps serving the single workspace it was built for.
func (s *Server) SetLocations(set *location.Set) {
	s.locations = set
}

// locationFor returns the services that own one session. A session whose
// directory is not open falls back to the default workspace, which is also what
// every session written before placement existed expects.
func (s *Server) locationFor(sessionID string) *location.Services {
	if s.locations == nil {
		return nil
	}
	if meta, err := s.store.Meta(sessionID); err == nil && meta.Directory != "" {
		if services, ok := s.locations.Resolve(meta.Directory); ok {
			return services
		}
	}
	return s.locations.Default()
}

// agentFor returns the agent loop that runs one session's turns.
func (s *Server) agentFor(sessionID string) *agent.Agent {
	if services := s.locationFor(sessionID); services != nil && services.Agent != nil {
		return services.Agent
	}
	return s.agent
}

// defaultAgentFor is the agent loop of the workspace a request names, which is
// what catalog endpoints answer with when no session is involved.
func (s *Server) defaultAgentFor(r *http.Request) *agent.Agent {
	if services := s.workspace(r); services != nil && services.Agent != nil {
		return services.Agent
	}
	return s.agent
}

// workspace resolves the workspace a request names. An explicit directory wins;
// otherwise the default workspace answers, so a client written for a
// single-workspace core keeps working unchanged.
func (s *Server) workspace(r *http.Request) *location.Services {
	if s.locations == nil {
		return nil
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("directory")); raw != "" {
		if services, ok := s.locations.Resolve(raw); ok {
			return services
		}
		return nil
	}
	return s.locations.Default()
}

// rootFor is the directory a workspace request runs in.
func (s *Server) rootFor(r *http.Request) string {
	if services := s.workspace(r); services != nil {
		return services.Root()
	}
	if strings.TrimSpace(s.root) != "" {
		return s.root
	}
	return s.config.Workdir
}

// agentsFor is the profile registry that owns a workspace request.
func (s *Server) agentsFor(r *http.Request) *agents.Registry {
	if services := s.workspace(r); services != nil && services.Agents != nil {
		return services.Agents
	}
	return s.agents
}

// pluginsFor is the plugin manager that owns a workspace request.
func (s *Server) pluginsFor(r *http.Request) *plugin.Manager {
	if services := s.workspace(r); services != nil && services.Plugins != nil {
		return services.Plugins
	}
	return s.plugins
}

// commandsFor is the command registry that owns a workspace request.
func (s *Server) commandsFor(r *http.Request) *commands.Registry {
	if services := s.workspace(r); services != nil && services.Commands != nil {
		return services.Commands
	}
	return s.commands
}

// skillsFor is the skill manager that owns a workspace request.
func (s *Server) skillsFor(r *http.Request) *skill.Manager {
	if services := s.workspace(r); services != nil && services.Skills != nil {
		return services.Skills
	}
	return s.skills
}

// mcpFor is the MCP manager that owns a workspace request.
func (s *Server) mcpFor(r *http.Request) *mcp.Manager {
	if services := s.workspace(r); services != nil && services.MCP != nil {
		return services.MCP
	}
	return s.mcp
}

// snapshotsFor is the snapshot manager that owns a workspace request.
func (s *Server) snapshotsFor(r *http.Request) Snapshots {
	if services := s.workspace(r); services != nil && services.Snapshots != nil {
		return services.Snapshots
	}
	return s.snapshots
}

// worktreesFor is the worktree manager that owns a workspace request.
func (s *Server) worktreesFor(r *http.Request) *worktree.Manager {
	if services := s.workspace(r); services != nil && services.Worktrees != nil {
		return services.Worktrees
	}
	return s.worktrees
}

// formattersFor is the formatter manager that owns a workspace request.
func (s *Server) formattersFor(r *http.Request) *format.Manager {
	if services := s.workspace(r); services != nil && services.Formatter != nil {
		return services.Formatter
	}
	return s.formatters
}

// vcsFor is the version-control manager that owns a workspace request.
func (s *Server) vcsFor(r *http.Request) *vcs.Manager {
	if services := s.workspace(r); services != nil && services.VCS != nil {
		return services.VCS
	}
	return s.vcs
}

// configFor is the resolved config that owns a workspace request.
func (s *Server) configFor(r *http.Request) config.Config {
	if services := s.workspace(r); services != nil {
		return services.Config()
	}
	return s.config
}

// sessionInWorkspace reports whether one session belongs to the workspace a
// request names. A session with no recorded directory predates placement and
// belongs to the default workspace, which is what a single-workspace core
// expects.
func (s *Server) sessionInWorkspace(r *http.Request, info session.Info) bool {
	services := s.workspace(r)
	if services == nil {
		return true
	}
	root := services.Root()
	if info.Directory == "" {
		if s.locations == nil {
			return true
		}
		def := s.locations.Default()
		return def != nil && def.Root() == root
	}
	return filepath.Clean(info.Directory) == filepath.Clean(root)
}

// SetAgents attaches the profile registry used by session and prompt APIs.
func (s *Server) SetAgents(registry *agents.Registry) {
	s.agents = registry
}

// announceAgent persists and publishes an agent switch, matching the reference
// session.agent.selected event. A repeat selection stays quiet, and so does the
// first selection of a session: before the first message there is no earlier
// turn to switch away from, so no divider belongs above that prompt.
func (s *Server) announceAgent(sessionID, agentID, previous string) {
	if agentID == "" || previous == "" || agentID == previous {
		return
	}
	if exists, err := s.store.HasEvents(sessionID); err != nil || !exists {
		return
	}
	ev := event.AgentSelectedEvent(agentID, previous)
	seq, err := s.store.Append(sessionID, ev)
	if err != nil {
		return
	}
	ev.Seq = seq
	s.agentFor(sessionID).Publish(sessionID, ev)
}

// sessionAgent is the agent a session was already answering with. A session
// that has not recorded one answers with the default, so adopting that default
// is not mistaken for a switch.
func (s *Server) sessionAgent(id string) string {
	if meta, err := s.store.Meta(id); err == nil && meta.Agent != "" {
		return meta.Agent
	}
	if services := s.locationFor(id); services != nil {
		if profile, ok := profileIn(services.Agents, ""); ok {
			return profile.ID
		}
	}
	if profile, ok := s.profile(""); ok {
		return profile.ID
	}
	return ""
}

// sessionModel is the provider/model a session was already answering with,
// falling back to the default target for the same reason as sessionAgent.
func (s *Server) sessionModel(id string) (string, string) {
	meta, _ := s.store.Meta(id)
	if meta.Provider != "" || meta.Model != "" {
		return meta.Provider, meta.Model
	}
	target := s.agentFor(id).DefaultTarget()
	return target.ProviderID, target.Model
}

// announceModel persists and publishes a provider/model switch, matching the
// reference session.model.selected event. A repeat selection stays quiet, and
// so does the first selection of a session: before the first message there is
// no earlier turn to switch away from.
func (s *Server) announceModel(sessionID, providerID, modelID, previousProvider, previousModel string) {
	if providerID == "" && modelID == "" {
		return
	}
	if previousProvider == "" && previousModel == "" {
		return
	}
	if providerID == previousProvider && modelID == previousModel {
		return
	}
	if exists, err := s.store.HasEvents(sessionID); err != nil || !exists {
		return
	}
	ev := event.ModelSelectedEvent(providerID, modelID, previousProvider, previousModel)
	seq, err := s.store.Append(sessionID, ev)
	if err != nil {
		return
	}
	ev.Seq = seq
	s.agentFor(sessionID).Publish(sessionID, ev)
}

// SetPlugins attaches plugin-provided commands and permission evaluation.
func (s *Server) SetPlugins(manager *plugin.Manager) {
	s.plugins = manager
}

// SetCommands attaches custom configured and markdown commands.
func (s *Server) SetCommands(registry *commands.Registry) {
	s.commands = registry
}

// SetSkills attaches the skill manager used by the session skill endpoint.
func (s *Server) SetSkills(manager *skill.Manager) {
	s.skills = manager
}

// SetSubagents attaches the manager that lists a session's child sessions.
func (s *Server) SetSubagents(manager *subagent.Manager) {
	s.subagents = manager
}

// SetJobs attaches the background job manager.
func (s *Server) SetJobs(manager *job.Manager) {
	s.jobs = manager
}

// SetWorkdir sets the resolved workspace root used by the file endpoints.
func (s *Server) SetWorkdir(dir string) {
	s.root = dir
}

// OnShutdown registers the callback that stops the process. It lets a desktop
// client shut the core down gracefully instead of killing it.
func (s *Server) OnShutdown(fn func()) {
	s.onShutdown = fn
}

// Close stops open event streams so shutdown can finish without waiting for
// clients that are still following a session.
func (s *Server) Close() {
	s.once.Do(func() { close(s.done) })
}

// SetAuthToken installs the bearer token every non-public request must carry.
// An empty token disables the check, which only tests do.
func (s *Server) SetAuthToken(token string) {
	s.authToken = token
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /project", s.projectInfo)
	mux.HandleFunc("GET /locations", s.listLocations)
	mux.HandleFunc("POST /locations", s.openLocation)
	mux.HandleFunc("DELETE /locations", s.closeLocation)
	mux.HandleFunc("GET /migration", s.migrationStatus)
	mux.HandleFunc("POST /migration", s.runMigration)
	mux.HandleFunc("GET /usage", s.workspaceUsage)
	mux.HandleFunc("GET /search", s.searchEvents)
	mux.HandleFunc("GET /sessions", s.listSessions)
	mux.HandleFunc("GET /sessions/{id}", s.getSession)
	mux.HandleFunc("PATCH /sessions/{id}", s.renameSession)
	mux.HandleFunc("GET /sessions/{id}/goal", s.getGoal)
	mux.HandleFunc("POST /sessions/{id}/goal", s.setGoal)
	mux.HandleFunc("DELETE /sessions/{id}", s.deleteSession)
	mux.HandleFunc("GET /sessions/{id}/messages", s.messages)
	mux.HandleFunc("GET /sessions/{id}/message", s.messages)
	mux.HandleFunc("GET /sessions/{id}/log", s.sessionLog)
	mux.HandleFunc("GET /sessions/{id}/environment", s.sessionEnvironment)
	mux.HandleFunc("GET /sessions/{id}/usage", s.sessionUsage)
	mux.HandleFunc("GET /sessions/{id}/reference", s.sessionReference)
	mux.HandleFunc("GET /sessions/{id}/location", s.sessionLocation)
	mux.HandleFunc("GET /sessions/{id}/media", s.sessionMedia)
	mux.HandleFunc("GET /sessions/{id}/pairing", s.listPairings)
	mux.HandleFunc("POST /sessions/{id}/pairing", s.createPairing)
	mux.HandleFunc("POST /sessions/{id}/pairing/{code}", s.redeemPairing)
	mux.HandleFunc("DELETE /sessions/{id}/pairing/{code}", s.revokePairing)
	mux.HandleFunc("GET /sessions/{id}/form", s.listForms)
	mux.HandleFunc("POST /sessions/{id}/form", s.createForm)
	mux.HandleFunc("POST /sessions/{id}/form/{formID}", s.replyForm)
	mux.HandleFunc("POST /sessions/{id}/synthetic", s.synthetic)
	mux.HandleFunc("GET /skills", s.listSkills)
	mux.HandleFunc("GET /mcp", s.listMCP)
	mux.HandleFunc("POST /mcp/{server}/tool", s.callMCPTool)
	mux.HandleFunc("POST /mcp/{server}/resource", s.readMCPResource)
	mux.HandleFunc("POST /sessions/{id}/skill", s.runSkill)
	mux.HandleFunc("POST /sessions/{id}/shell", s.runSessionShell)
	mux.HandleFunc("GET /sessions/{id}/shell", s.listSessionJobs)
	mux.HandleFunc("GET /sessions/{id}/subagents", s.listSessionSubagents)
	mux.HandleFunc("POST /sessions/{id}/background", s.startBackground)
	mux.HandleFunc("GET /sessions/{id}/background", s.listSessionJobs)
	mux.HandleFunc("DELETE /sessions/{id}/background/{jobID}", s.cancelBackgroundJob)
	mux.HandleFunc("GET /sessions/{id}/background/{jobID}/output", s.jobOutput)
	mux.HandleFunc("GET /background", s.listWorkspaceJobs)
	mux.HandleFunc("POST /sessions/{id}/wait", s.waitSession)
	mux.HandleFunc("GET /sessions/{id}/bundle", s.exportBundle)
	mux.HandleFunc("POST /sessions/import", s.importBundle)
	mux.HandleFunc("POST /sessions/{id}/fork", s.forkSession)
	mux.HandleFunc("POST /sessions/{id}/move", s.moveSession)
	mux.HandleFunc("GET /sessions/{id}/context", s.context)
	mux.HandleFunc("GET /sessions/{id}/diff", s.sessionDiff)
	mux.HandleFunc("POST /sessions/{id}/command", s.runCommand)
	mux.HandleFunc("GET /sessions/{id}/inbox", s.listInbox)
	mux.HandleFunc("DELETE /sessions/{id}/inbox/{inboxID}", s.cancelInbox)
	mux.HandleFunc("PATCH /sessions/{id}/inbox/{inboxID}", s.updateInbox)
	mux.HandleFunc("GET /sessions/{id}/revert", s.revertState)
	mux.HandleFunc("POST /sessions/{id}/revert", s.revert)
	mux.HandleFunc("POST /sessions/{id}/reapply", s.reapply)
	mux.HandleFunc("GET /sessions/{id}/events", s.events)
	mux.HandleFunc("GET /events", s.allEvents)
	mux.HandleFunc("POST /sessions/{id}/interrupt", s.interrupt)
	mux.HandleFunc("POST /sessions/{id}/compact", s.compact)
	mux.HandleFunc("POST /sessions/{id}/summary", s.summary)
	mux.HandleFunc("GET /sessions/{id}/permission", s.listPermissions)
	mux.HandleFunc("GET /sessions/{id}/permissions", s.sessionPermissions)
	mux.HandleFunc("POST /sessions/{id}/permissions", s.setSessionPermissions)
	mux.HandleFunc("POST /sessions/{id}/permission/{rid}", s.replyPermission)
	mux.HandleFunc("GET /agents", s.listAgents)
	mux.HandleFunc("GET /agents/{id}", s.getAgent)
	mux.HandleFunc("GET /providers", s.listProviders)
	mux.HandleFunc("GET /providers/{id}", s.getProvider)
	mux.HandleFunc("GET /models", s.listModels)
	mux.HandleFunc("GET /models/default", s.defaultModel)
	mux.HandleFunc("GET /config", s.getConfig)
	mux.HandleFunc("PATCH /config", s.patchConfig)
	mux.HandleFunc("GET /commands", s.listCommands)
	mux.HandleFunc("GET /plugins", s.listPlugins)
	mux.HandleFunc("POST /plugins/{plugin}/tool", s.callPluginTool)
	mux.HandleFunc("GET /files", s.listFiles)
	mux.HandleFunc("GET /files/search", s.searchFiles)
	mux.HandleFunc("GET /files/grep", s.grepFiles)
	mux.HandleFunc("POST /files", s.createEntry)
	mux.HandleFunc("GET /file", s.readFile)
	mux.HandleFunc("PUT /file", s.writeFile)
	mux.HandleFunc("DELETE /file", s.deleteFile)
	mux.HandleFunc("POST /file/move", s.moveFile)
	mux.HandleFunc("GET /worktrees", s.listWorktrees)
	mux.HandleFunc("POST /worktrees", s.createWorktree)
	mux.HandleFunc("DELETE /worktrees", s.removeWorktree)
	mux.HandleFunc("GET /formatters", s.listFormatters)
	mux.HandleFunc("POST /format", s.formatFile)
	mux.HandleFunc("POST /sessions/{id}/share", s.shareSession)
	mux.HandleFunc("GET /sessions/{id}/share", s.shareStatus)
	mux.HandleFunc("DELETE /sessions/{id}/share", s.unshareSession)
	mux.HandleFunc("GET /sessions/{id}/export", s.exportSession)
	mux.HandleFunc("GET /share", s.shareConfig)
	mux.HandleFunc("GET /vcs", s.withVCS(vcsInfo))
	mux.HandleFunc("GET /vcs/base", s.withVCS(vcsBase))
	mux.HandleFunc("GET /vcs/status", s.withVCS(vcsStatus))
	mux.HandleFunc("GET /vcs/branch", s.withVCS(vcsBranches))
	mux.HandleFunc("GET /vcs/diff", s.withVCS(vcsDiff))
	mux.HandleFunc("POST /vcs/init", s.withVCS(vcsInit))
	mux.HandleFunc("GET /pty", s.withPTY(listPTY))
	mux.HandleFunc("POST /pty", s.withPTY(createPTY))
	mux.HandleFunc("GET /pty/{id}", s.withPTY(getPTY))
	mux.HandleFunc("PUT /pty/{id}", s.withPTY(updatePTY))
	mux.HandleFunc("DELETE /pty/{id}", s.withPTY(deletePTY))
	mux.HandleFunc("POST /pty/{id}/input", s.withPTY(ptyInput))
	mux.HandleFunc("GET /pty/{id}/output", s.withPTY(ptyOutput))
	mux.HandleFunc("GET /pty/{id}/connect", s.withPTY(ptyConnect))
	mux.HandleFunc("GET /shell", s.withPTY(listShell))
	mux.HandleFunc("POST /shell", s.withPTY(createShell))
	mux.HandleFunc("GET /shell/{id}", s.withPTY(getShell))
	mux.HandleFunc("GET /shell/{id}/output", s.withPTY(shellOutput))
	mux.HandleFunc("DELETE /shell/{id}", s.withPTY(deleteShell))
	mux.HandleFunc("GET /sessions/{id}/terminal", s.withPTY(listSessionTerminals))
	mux.HandleFunc("POST /sessions/{id}/terminal", s.withPTY(createSessionTerminal))
	mux.HandleFunc("GET /sessions/{id}/terminal/read", s.withPTY(readSessionTerminal))
	mux.HandleFunc("POST /persistent-pty/shutdown", s.withPTY(shutdownPersistentPTY))
	mux.HandleFunc("POST /persistent-pty/handoff", s.withPTY(handoffPersistentPTY))
	mux.HandleFunc("POST /persistent-pty/import", s.withPTY(importPersistentPTY))
	mux.HandleFunc("GET /persistent-pty/{id}", s.withPTY(getPersistentPTY))
	mux.HandleFunc("PUT /persistent-pty/{id}", s.withPTY(updatePersistentPTY))
	mux.HandleFunc("GET /persistent-pty/{id}/snapshot", s.withPTY(snapshotPersistentPTY))
	mux.HandleFunc("DELETE /persistent-pty/{id}", s.withPTY(deletePersistentPTY))
	mux.HandleFunc("POST /persistent-pty/{id}/connect-token", s.withPTY(persistentPTYToken))
	mux.HandleFunc("GET /persistent-pty/{id}/connect", s.withPTY(connectPersistentPTY))
	mux.HandleFunc("POST /persistent-pty/{id}/input", s.withPTY(inputPersistentPTY))
	mux.HandleFunc("GET /persistent-pty/{id}/output", s.withPTY(outputPersistentPTY))
	mux.HandleFunc("GET /share/{token}", s.serveShare)
	mux.HandleFunc("GET /oauth", s.listOAuth)
	mux.HandleFunc("GET /oauth/{name}", s.oauthStatus)
	mux.HandleFunc("POST /oauth/{name}/refresh", s.oauthRefresh)
	mux.HandleFunc("POST /oauth/{name}/authorize", s.oauthAuthorize)
	mux.HandleFunc("POST /oauth/{name}/token", s.oauthToken)
	mux.HandleFunc("DELETE /oauth/{name}", s.oauthRevoke)
	mux.HandleFunc("GET /websearch", s.webSearchStatus)
	mux.HandleFunc("GET /image", s.imageStatus)
	mux.HandleFunc("POST /image", s.generateImage)
	mux.HandleFunc("GET /artifacts/providers", s.withArtifacts(artifactProviders))
	mux.HandleFunc("GET /artifacts", s.withArtifacts(listArtifacts))
	mux.HandleFunc("POST /artifacts", s.withArtifacts(createArtifact))
	mux.HandleFunc("PATCH /artifacts/{id}", s.withArtifacts(updateArtifact))
	mux.HandleFunc("GET /artifacts/{id}", s.withArtifacts(getArtifact))
	mux.HandleFunc("GET /artifacts/{id}/raw/{path...}", s.withArtifacts(serveArtifactFile))
	mux.HandleFunc("DELETE /artifacts/{id}", s.withArtifacts(deleteArtifact))
	mux.HandleFunc("GET /schedules", s.listSchedules)
	mux.HandleFunc("POST /schedules", s.createSchedule)
	mux.HandleFunc("PATCH /schedules/{id}", s.updateSchedule)
	mux.HandleFunc("DELETE /schedules/{id}", s.deleteSchedule)
	mux.HandleFunc("POST /websearch", s.webSearch)
	mux.HandleFunc("GET /sessions/{id}/question", s.listQuestions)
	mux.HandleFunc("POST /sessions/{id}/question/{qid}", s.replyQuestion)
	mux.HandleFunc("POST /sessions/{id}/prompt", s.prompt)
	mux.HandleFunc("POST /sessions/{id}/input", s.input)
	mux.HandleFunc("POST /shutdown", s.shutdown)
	return auth.Middleware(s.authToken, guardBody(mux))
}

// maxRequestBody bounds a JSON request body. Uploads carry their own, smaller
// limit; this is the outer ceiling for every other endpoint.
const maxRequestBody = 8 << 20

// guardBody caps request bodies and answers the CORS preflight a browser sends
// before a cross-origin call. The preflight never reaches a handler, because a
// cross-origin call is refused by the auth middleware.
func guardBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", "GET, POST, PATCH, DELETE, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Body != nil && r.ContentLength != 0 {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	profiles := []agents.Profile{}
	defaultID := "build"
	if registry := s.agentsFor(r); registry != nil {
		profiles = registry.Primary()
		defaultID = registry.Default().ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": profiles, "default": defaultID})
}

// getAgent returns one profile by id. It serves hidden and subagent profiles
// too, so a client can label a historical message or a child session whose
// agent is no longer selectable.
func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, errText("agent id is required"))
		return
	}
	registry := s.agentsFor(r)
	if registry == nil {
		if id == "build" {
			writeJSON(w, http.StatusOK, agents.Profile{ID: "build", Name: "Build", Mode: agents.ModePrimary})
			return
		}
		writeError(w, http.StatusNotFound, errText("agent not found: "+id))
		return
	}
	profile, ok := registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errText("agent not found: "+id))
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

// profile resolves a selectable primary agent.
func (s *Server) profile(id string) (agents.Profile, bool) {
	return profileIn(s.agents, id)
}

// profileIn resolves a selectable primary agent inside one workspace registry.
func profileIn(registry *agents.Registry, id string) (agents.Profile, bool) {
	if registry == nil {
		if id == "" || id == "build" {
			return agents.Profile{ID: "build", Name: "Build", Mode: agents.ModePrimary}, true
		}
		return agents.Profile{}, false
	}
	if strings.TrimSpace(id) == "" {
		return registry.Default(), true
	}
	profile, ok := registry.Get(id)
	if !ok || profile.Hidden || profile.Mode == agents.ModeSubagent {
		return agents.Profile{}, false
	}
	return profile, true
}

func (s *Server) listCommands(w http.ResponseWriter, r *http.Request) {
	items := []plugin.Command{}
	seen := map[string]bool{}
	if manager := s.pluginsFor(r); manager != nil {
		for _, item := range manager.Commands() {
			items = append(items, item)
			seen[item.Name] = true
		}
	}
	if registry := s.commandsFor(r); registry != nil {
		for _, item := range registry.Commands() {
			if seen[item.Name] {
				continue
			}
			items = append(items, plugin.Command{
				Name:        item.Name,
				Description: item.Description,
				Template:    item.Template,
				Plugin:      item.Plugin,
			})
		}
	}
	if manager := s.mcpFor(r); manager != nil {
		for _, item := range manager.Prompts() {
			name := mcp.PromptCommandName(item.Server, item.Name)
			if seen[name] {
				continue
			}
			items = append(items, plugin.Command{
				Name:        name,
				Description: item.Description,
				Plugin:      "mcp",
			})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"commands": items})
}

// expand resolves a leading slash command to its plugin or custom template.
func (s *Server) expand(text string) string {
	return s.expandIn(nil, text)
}

// expandIn resolves a leading slash command inside one workspace. A nil request
// keeps the default workspace, which is what background callers use.
func (s *Server) expandIn(r *http.Request, text string) string {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "/") {
		body := strings.TrimPrefix(trimmed, "/")
		name, args, _ := strings.Cut(body, " ")
		if manager := s.mcpFor(r); manager != nil {
			if rendered, ok, err := manager.ExpandPrompt(context.Background(), name, strings.TrimSpace(args)); ok {
				if err != nil {
					return "MCP prompt error: " + err.Error()
				}
				return rendered
			}
		}
	}
	if manager := s.pluginsFor(r); manager != nil {
		if expanded, ok := manager.Expand(text); ok {
			return expanded
		}
	}
	if registry := s.commandsFor(r); registry != nil {
		if expanded, ok := registry.Expand(text); ok {
			return expanded
		}
	}
	return text
}

// shutdown stops the core gracefully. Clients that spawned the core use it
// instead of killing the process.
func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	if s.onShutdown == nil {
		writeError(w, http.StatusNotImplemented, errText("shutdown is not supported"))
		return
	}
	w.WriteHeader(http.StatusAccepted)
	go s.onShutdown()
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	infos := make([]session.Info, 0, len(all))
	for _, info := range all {
		if !s.sessionInWorkspace(r, info) {
			continue
		}
		info.Running = s.agentFor(info.ID).Running(info.ID)
		infos = append(infos, info)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": infos})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	info, err := s.store.Info(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	info.Running = s.agentFor(info.ID).Running(info.ID)
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) renameSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Title    *string `json:"title"`
		Provider *string `json:"provider"`
		Model    *string `json:"model"`
		Agent    *string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Title == nil && body.Provider == nil && body.Model == nil && body.Agent == nil {
		writeError(w, http.StatusBadRequest, errText("nothing to update"))
		return
	}
	if !s.requireSession(w, r, id) {
		return
	}
	if body.Title != nil {
		if strings.TrimSpace(*body.Title) == "" {
			writeError(w, http.StatusBadRequest, errText("title is empty"))
			return
		}
		if err := s.store.SetTitle(id, *body.Title); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	if body.Agent != nil {
		profile, ok := profileIn(s.agentsFor(r), strings.TrimSpace(*body.Agent))
		if !ok {
			writeError(w, http.StatusBadRequest, errText("unknown agent"))
			return
		}
		previous := s.sessionAgent(id)
		if err := s.store.SetAgent(id, profile.ID); err != nil {
			writeStoreError(w, err)
			return
		}
		s.announceAgent(id, profile.ID, previous)
		if profile.Model != "" && body.Provider == nil && body.Model == nil {
			providerID, modelID, err := agents.ValidateModel(profile.Model)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			if err := s.store.SetModel(id, providerID, modelID); err != nil {
				writeStoreError(w, err)
				return
			}
		}
	}
	if body.Provider != nil || body.Model != nil {
		previousProvider, previousModel := s.sessionModel(id)
		selection := provider.Selection{Provider: previousProvider, Model: previousModel}
		if body.Provider != nil {
			selection.Provider = *body.Provider
		}
		if body.Model != nil {
			selection.Model = *body.Model
		}
		target, err := s.agentFor(id).Resolve(selection)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.store.SetModel(id, target.ProviderID, target.Model); err != nil {
			writeStoreError(w, err)
			return
		}
		s.announceModel(id, target.ProviderID, target.Model, previousProvider, previousModel)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.agentFor(id).Running(id) {
		writeError(w, http.StatusConflict, agent.ErrBusy)
		return
	}
	if err := s.store.Delete(id); err != nil {
		writeStoreError(w, err)
		return
	}
	if s.shares != nil {
		_ = s.shares.Revoke(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiMessage is one rebuilt message plus the durable time it entered the
// conversation. Created is API/UI metadata only; model requests never see it.
type apiMessage struct {
	model.Message
	Created           time.Time `json:"created"`
	Agent             string    `json:"agent,omitempty"`
	Provider          string    `json:"provider,omitempty"`
	Model             string    `json:"model,omitempty"`
	Compaction        bool      `json:"compaction,omitempty"`
	CompactionCovered int       `json:"compactionCovered,omitempty"`
	CompactionReason  string    `json:"compactionReason,omitempty"`
}

func publicMessageRecords(records []session.MessageRecord) []apiMessage {
	out := make([]apiMessage, len(records))
	for index, record := range records {
		out[index] = apiMessage{
			Message:           publicMessages([]model.Message{record.Message})[0],
			Created:           record.Created,
			Agent:             record.Agent,
			Provider:          record.Provider,
			Model:             record.Model,
			Compaction:        record.Compaction,
			CompactionCovered: record.CompactionCovered,
			CompactionReason:  record.CompactionReason,
		}
	}
	return out
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	records, err := s.store.MessageRecords(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": publicMessageRecords(records)})
}

// sessionLog returns every durable event of one session without streaming.
func (s *Server) sessionLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	events, err := s.store.Events(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if events == nil {
		events = []event.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// synthetic injects externally produced text into a session. Idle sessions
// start a synthetic turn; running sessions read it before the next model call.
func (s *Server) synthetic(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	body.Text = s.expandIn(r, body.Text)
	if strings.TrimSpace(body.Text) == "" {
		writeError(w, http.StatusBadRequest, errText("text is empty"))
		return
	}
	if err := s.agentFor(id).Notify(id, event.SyntheticEvent(body.Text)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}

// waitSession blocks until the session has no active turn. The optional
// timeout_ms bounds the wait; zero uses 30 seconds and the maximum is 120.
func (s *Server) waitSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	body := struct {
		TimeoutMS int `json:"timeout_ms"`
	}{}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	timeout := 30 * time.Second
	if body.TimeoutMS > 0 {
		timeout = time.Duration(body.TimeoutMS) * time.Millisecond
	}
	if timeout > 120*time.Second {
		timeout = 120 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for s.agentFor(id).Running(id) {
		select {
		case <-r.Context().Done():
			writeError(w, http.StatusRequestTimeout, errText("wait cancelled"))
			return
		case <-deadline.C:
			writeError(w, http.StatusRequestTimeout, errText("session is still running"))
			return
		case <-ticker.C:
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"idle": true})
}

// context returns the rebuilt conversation and what it costs against the
// window, so a client can show how full the context is.
func (s *Server) context(w http.ResponseWriter, r *http.Request) {
	current, err := s.store.Context(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	target := s.targetFor(r.PathValue("id"))
	meta, _ := s.store.Meta(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": publicMessages(current.Messages),
		"tokens":   current.Tokens,
		"window":   target.Window,
		"provider": target.ProviderID,
		"model":    target.Model,
		"agent":    meta.Agent,
		"running":  s.agentFor(r.PathValue("id")).Running(r.PathValue("id")),
	})
}

// events replays durable events after a sequence number and, with follow=true,
// keeps streaming live events of the session.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	follow := r.URL.Query().Get("follow") == "true"

	// Subscribe before replaying so events appended during the replay are not
	// lost; duplicates are skipped by sequence.
	sub := s.agentFor(id).Subscribe(id)
	defer sub.Close()

	replay, err := s.store.EventsAfter(id, after)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	stream, err := newSSEWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	last := after
	for _, ev := range replay {
		if !stream.write(string(ev.Type), ev) {
			return
		}
		last = ev.Seq
	}
	stream.flush()
	if !follow {
		return
	}

	for {
		select {
		case <-s.done:
			return
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.Events():
			if !ok {
				return
			}
			if ev.Seq != 0 && ev.Seq <= last {
				continue
			}
			if ev.Seq > last {
				last = ev.Seq
			}
			if !stream.write(string(ev.Type), ev) {
				return
			}
			stream.flush()
		}
	}
}

// interrupt cancels the running turn of a session.
func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"interrupted": s.agentFor(r.PathValue("id")).Interrupt(r.PathValue("id"))})
}

// compact summarizes the session on demand.
func (s *Server) compact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	target := s.targetFor(id)
	err := s.agentFor(id).Compact(context.Background(), id, provider.Selection{Provider: target.ProviderID, Model: target.Model})
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, agent.ErrBusy):
		writeError(w, http.StatusConflict, err)
	case strings.HasPrefix(err.Error(), "compact: nothing to compact"):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeStoreError(w, err)
	}
}

// summary runs the hidden summary agent over a session without mutating it.
func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	target := s.targetFor(id)
	text, err := s.agentFor(id).Summarize(r.Context(), id, provider.Selection{Provider: target.ProviderID, Model: target.Model})
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"summary": text})
	case errors.Is(err, agent.ErrBusy):
		writeError(w, http.StatusConflict, err)
	default:
		writeStoreError(w, err)
	}
}

// targetFor resolves the session's saved selection, falling back to the
// configured default when the provider or model no longer exists.
func (s *Server) targetFor(id string) provider.Target {
	meta, _ := s.store.Meta(id)
	target, err := s.agentFor(id).Resolve(provider.Selection{Provider: meta.Provider, Model: meta.Model})
	if err != nil {
		return s.agentFor(id).DefaultTarget()
	}
	return target
}

func (s *Server) listPermissions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"requests": s.agentFor(r.PathValue("id")).Pending(r.PathValue("id"))})
}

// listQuestions returns the pending questions of a session.
func (s *Server) listQuestions(w http.ResponseWriter, r *http.Request) {
	questions := s.agentFor(r.PathValue("id")).PendingQuestions(r.PathValue("id"))
	if questions == nil {
		questions = []question.Request{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"questions": questions})
}

// replyQuestion answers or dismisses a pending question.
func (s *Server) replyQuestion(w http.ResponseWriter, r *http.Request) {
	var body question.Response
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	err := s.agentFor(r.PathValue("id")).ReplyQuestion(r.PathValue("id"), r.PathValue("qid"), body)
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var missing question.ErrNotFound
	if errors.As(err, &missing) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

// replyPermission answers a pending permission request.
func (s *Server) replyPermission(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision string `json:"decision"`
		Message  string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	decision := permission.Decision(body.Decision)
	switch decision {
	case permission.Once, permission.Always, permission.Reject:
	default:
		writeError(w, http.StatusBadRequest, errText("decision must be once, always or reject"))
		return
	}
	if err := s.agentFor(r.PathValue("id")).Reply(r.PathValue("rid"), decision, body.Message); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// prompt starts one conversation turn and streams its events as SSE. The turn
// is owned by the session, so closing the connection does not stop it; use the
// interrupt endpoint for that.
func (s *Server) prompt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text        string             `json:"text"`
		Provider    string             `json:"provider"`
		Model       string             `json:"model"`
		Agent       string             `json:"agent"`
		Persist     *bool              `json:"persist"`
		Attachments []model.Attachment `json:"attachments"`
		Comments    []model.Comment    `json:"comments"`
		References  []model.Reference  `json:"references"`
		Voice       bool               `json:"voice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	workspace := s.workspace(r)
	if name := slashName(body.Text); name != "" {
		if command, ok := s.customCommandIn(r, name); ok {
			if body.Agent == "" {
				body.Agent = command.Agent
			}
			if body.Provider == "" && body.Model == "" && command.Model != "" {
				providerID, modelID, err := agents.ValidateModel(command.Model)
				if err != nil {
					writeError(w, http.StatusBadRequest, err)
					return
				}
				body.Provider = providerID
				body.Model = modelID
			}
		}
	}
	// `/goal` is a session command, not a prompt: it arms the goal and lets the
	// core send the kickoff itself, so the objective never becomes a user turn.
	if name := slashName(body.Text); name == "goal" {
		objective := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body.Text), "/goal"))
		s.handleGoalPrompt(w, r, objective, body.Provider, body.Model)
		return
	}
	body.Text = s.expandIn(r, body.Text)
	if body.Text == "" {
		writeError(w, http.StatusBadRequest, errText("text is empty"))
		return
	}
	attachments, err := normalizeAttachments(body.Attachments, s.configFor(r).Media)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	persist := body.Persist == nil || *body.Persist

	id := r.PathValue("id")
	meta, _ := s.store.Meta(id)
	if persist && workspace != nil && meta.Directory == "" {
		// A session created here belongs to the workspace that answered, so a
		// later request can route it back to the same tools and terminals.
		if err := s.store.SetDirectory(id, workspace.Root()); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	agentID := strings.TrimSpace(body.Agent)
	if agentID == "" {
		agentID = meta.Agent
	}
	profile, ok := profileIn(s.agentsFor(r), agentID)
	if !ok {
		writeError(w, http.StatusBadRequest, errText("unknown agent"))
		return
	}
	if persist && body.Agent != "" {
		previous := s.sessionAgent(id)
		if err := s.store.SetAgent(id, profile.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.announceAgent(id, profile.ID, previous)
	}

	selection := provider.Selection{Provider: meta.Provider, Model: meta.Model}
	if body.Provider == "" && body.Model == "" && profile.Model != "" && (body.Agent != "" || selection.Provider == "" || selection.Model == "") {
		providerID, modelID, err := agents.ValidateModel(profile.Model)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		selection = provider.Selection{Provider: providerID, Model: modelID}
	}
	if body.Provider != "" || body.Model != "" {
		selection = provider.Selection{Provider: body.Provider, Model: body.Model}
	}
	target, err := s.agentFor(id).Resolve(selection)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if hasMediaAttachment(attachments) && !target.Attachment {
		writeError(w, http.StatusBadRequest, errAttachmentsUnsupported)
		return
	}
	if persist {
		// Capture the previous selection before the write, so an explicit pick
		// is announced as a change from what the session answered with before.
		previousProvider, previousModel := s.sessionModel(id)
		if err := s.store.SetModel(id, target.ProviderID, target.Model); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// Only an explicit selection draws the switch separator; the first
		// prompt of a session adopting its default model is not a change.
		if body.Provider != "" || body.Model != "" {
			s.announceModel(id, target.ProviderID, target.Model, previousProvider, previousModel)
		}
	}

	selection = provider.Selection{Provider: target.ProviderID, Model: target.Model}
	var events <-chan event.Event
	extra := map[string]any{}
	if len(body.Comments) > 0 {
		extra["comments"] = body.Comments
	}
	if len(body.References) > 0 {
		extra["references"] = body.References
	}
	if body.Voice {
		extra["voice"] = true
	}
	if len(extra) > 0 {
		events, err = s.agentFor(id).RunWithMeta(context.Background(), id, body.Text, attachments, selection, extra)
	} else if len(attachments) > 0 {
		events, err = s.agentFor(id).RunWithAttachments(context.Background(), id, body.Text, attachments, selection)
	} else {
		events, err = s.agentFor(id).Run(context.Background(), id, body.Text, selection)
	}
	if err != nil {
		if errors.Is(err, agent.ErrBusy) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}

	stream, err := newSSEWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	for {
		select {
		case <-s.done:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if !stream.write(string(ev.Type), ev) {
				return
			}
			stream.flush()
		}
	}
}

// input appends a prompt to the active turn. It never starts a run: clients
// call prompt when idle, and input only while an SSE stream is already open.
func (s *Server) input(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text     string `json:"text"`
		Delivery string `json:"delivery"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	body.Text = s.expandIn(r, body.Text)
	if strings.TrimSpace(body.Text) == "" {
		writeError(w, http.StatusBadRequest, errText("text is empty"))
		return
	}
	delivery := strings.TrimSpace(body.Delivery)
	if delivery == "" {
		delivery = "steer"
	}
	if delivery != "steer" && delivery != "queue" {
		writeError(w, http.StatusBadRequest, errText("delivery must be steer or queue"))
		return
	}
	if _, err := s.agentFor(r.PathValue("id")).EnqueueDelivery(r.PathValue("id"), body.Text, delivery); err != nil {
		if errors.Is(err, agent.ErrNotRunning) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusTooManyRequests, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "delivery": delivery})
}

// subscribeAll follows live events from every workspace this core serves. Each
// workspace has its own hub, so a single subscription would miss the projects
// that are not the default one. Workspaces opened while the stream is running
// are picked up too, because a client opens a project after it connects.
func (s *Server) subscribeAll() (<-chan event.SessionEvent, func()) {
	out := make(chan event.SessionEvent, 64)
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() { close(done) })
	}

	var (
		mu      sync.Mutex
		seen    = map[*agent.Agent]bool{}
		stopped bool
	)
	// attach subscribes to one workspace and forwards its events. It reports
	// whether the agent was already subscribed.
	attach := func(loop *agent.Agent) bool {
		if loop == nil {
			return true
		}
		mu.Lock()
		if seen[loop] || stopped {
			mu.Unlock()
			return true
		}
		seen[loop] = true
		mu.Unlock()

		sub := loop.SubscribeAll()
		go func() {
			defer sub.Close()
			for {
				select {
				case <-done:
					return
				case item, ok := <-sub.Events():
					if !ok {
						return
					}
					select {
					case out <- item:
					case <-done:
						return
					}
				}
			}
		}()
		return false
	}

	loops := func() []*agent.Agent {
		all := []*agent.Agent{s.agent}
		if s.locations != nil {
			for _, services := range s.locations.List() {
				if services.Agent != nil {
					all = append(all, services.Agent)
				}
			}
		}
		return all
	}

	for _, loop := range loops() {
		attach(loop)
	}

	// A workspace opened after this stream started has its own hub, so poll the
	// set until the stream closes. The tick is slow enough to cost nothing and
	// fast enough that a newly opened project streams immediately.
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				for _, loop := range loops() {
					attach(loop)
				}
			}
		}
	}()

	go func() {
		<-done
		close(out)
	}()
	return out, stop
}

// allEvents streams events from every session, each tagged with its session ID.
// With after set it first replays the durable events past that cross-session
// position, so a client that reconnects does not lose the events in the gap.
func (s *Server) allEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	// Subscribe before replaying so events appended during the replay are not
	// lost; positions already sent are skipped below.
	live, closeLive := s.subscribeAll()
	defer closeLive()
	stream, err := newSSEWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	last := after
	for after > 0 {
		replay, replayErr := s.store.GlobalAfter(last, 500)
		if replayErr != nil {
			writeError(w, http.StatusInternalServerError, replayErr)
			return
		}
		for _, item := range replay {
			if !s.writeGlobalEvent(stream, item.SessionID, item.Event) {
				return
			}
			last = item.Event.Global
		}
		if len(replay) < 500 {
			break
		}
	}
	stream.flush()

	for {
		select {
		case <-s.done:
			return
		case <-r.Context().Done():
			return
		case item, ok := <-live:
			if !ok {
				return
			}
			// Live events carry the same cross-session position, so a replay
			// followed by live delivery never repeats one.
			if item.Event.Global != 0 && item.Event.Global <= last {
				continue
			}
			if item.Event.Global > last {
				last = item.Event.Global
			}
			if !s.writeGlobalEvent(stream, item.SessionID, item.Event) {
				return
			}
			stream.flush()
		}
	}
}

// writeGlobalEvent emits one all-session frame with its session and position.
// The session's workspace is included so a client does not have to resolve it
// with a second request per event; the directory is read from the session
// placement the core already stores.
func (s *Server) writeGlobalEvent(stream *sseWriter, sessionID string, ev event.Event) bool {
	payload := map[string]any{
		"sessionId": sessionID,
		"seq":       ev.Seq,
		"global":    ev.Global,
		"type":      ev.Type,
		"at":        ev.At,
		"data":      ev.Data,
	}
	if directory := s.sessionDirectory(sessionID); directory != "" {
		payload["directory"] = directory
	}
	return stream.write(string(ev.Type), payload)
}

// sessionDirectory is the workspace a session belongs to. An unplaced session
// belongs to the default workspace, which is what a single-workspace core
// expects.
func (s *Server) sessionDirectory(sessionID string) string {
	meta, err := s.store.Meta(sessionID)
	if err == nil && meta.Directory != "" {
		return meta.Directory
	}
	if s.locations != nil {
		if def := s.locations.Default(); def != nil {
			return def.Root()
		}
	}
	return s.root
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// writeStoreError maps store errors: missing sessions are 404, invalid ids 400.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, errText("session not found"))
	case strings.HasPrefix(err.Error(), "invalid session id"):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

type errText string

func (e errText) Error() string { return string(e) }
