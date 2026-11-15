package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"lingzhu/internal/config"
	promptpkg "lingzhu/internal/prompt"
)

// projectInfo describes the workspace served by this core instance.
func (s *Server) projectInfo(w http.ResponseWriter, r *http.Request) {
	root := s.rootFor(r)
	if strings.TrimSpace(root) == "" {
		writeError(w, http.StatusBadRequest, errText("workspace is not configured"))
		return
	}
	cfg := s.configFor(r)
	sessions, _ := s.store.List()
	payload := map[string]any{
		"directory":    root,
		"name":         filepath.Base(root),
		"config":       cfg.Path,
		"instructions": promptpkg.InstructionsPath(root),
		"sessions":     len(sessions),
		"vcs":          nil,
	}
	if manager := s.vcsFor(r); manager != nil {
		if info, err := manager.Info(r.Context()); err == nil {
			payload["vcs"] = info
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

// listLocations reports every workspace this core serves, so a client can build
// a project switcher without spawning a core per directory.
func (s *Server) listLocations(w http.ResponseWriter, r *http.Request) {
	items := []map[string]any{}
	if s.locations != nil {
		for _, services := range s.locations.List() {
			items = append(items, map[string]any{
				"directory": services.Root(),
				"name":      services.Ref.Name(),
				"default":   s.locations.Default() == services,
				"available": services.Ref.Exists(),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"locations": items})
}

// openLocation adds one directory to this core. Opening an already open
// workspace is a no-op, so a client can call it on every project switch.
func (s *Server) openLocation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Directory string `json:"directory"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(body.Directory) == "" {
		writeError(w, http.StatusBadRequest, errText("directory is required"))
		return
	}
	if s.openLocationFn == nil {
		writeError(w, http.StatusNotImplemented, errText("this core serves a single workspace"))
		return
	}
	directory, err := s.openLocationFn(body.Directory)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"directory": directory})
}

// closeLocation drops one workspace and stops its tools, terminals and MCP
// servers. The default workspace cannot be closed while it is in use.
func (s *Server) closeLocation(w http.ResponseWriter, r *http.Request) {
	directory := strings.TrimSpace(r.URL.Query().Get("directory"))
	if directory == "" {
		writeError(w, http.StatusBadRequest, errText("directory is required"))
		return
	}
	if s.closeLocationFn == nil {
		writeError(w, http.StatusNotImplemented, errText("this core serves a single workspace"))
		return
	}
	if s.locations != nil && s.locations.Default() != nil && s.locations.Default().Root() == directory {
		writeError(w, http.StatusConflict, errText("the default workspace cannot be closed"))
		return
	}
	if !s.closeLocationFn(directory) {
		writeError(w, http.StatusNotFound, errText("workspace is not open"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sessionLocation describes where one session runs and its permission rules.
func (s *Server) sessionLocation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	meta, _ := s.store.Meta(id)
	target := s.targetFor(id)
	directory := s.rootFor(r)
	if meta.Directory != "" {
		directory = meta.Directory
	}
	permissions := s.configFor(r).Permission
	if permissions == nil {
		permissions = []config.Rule{}
	}
	parentTitle := ""
	if meta.Parent != "" {
		if info, err := s.store.Info(meta.Parent); err == nil {
			parentTitle = info.Title
		}
	}
	worktree := s.worktreeFor(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId":   id,
		"directory":   directory,
		"project":     filepath.Base(directory),
		"worktree":    worktree,
		"parent":      meta.Parent,
		"parentTitle": parentTitle,
		"provider":    target.ProviderID,
		"model":       target.Model,
		"agent":       meta.Agent,
		"permissions": permissions,
	})
}

// worktreeFor names the git worktree that owns this workspace, or an empty
// string when the workspace is the main checkout or git is unavailable.
func (s *Server) worktreeFor(r *http.Request) string {
	manager := s.worktreesFor(r)
	if manager == nil {
		return ""
	}
	entries, err := manager.List(r.Context())
	if err != nil {
		return ""
	}
	root := filepath.Clean(s.rootFor(r))
	for _, entry := range entries {
		if entry.Main {
			continue
		}
		if filepath.Clean(entry.Path) != root {
			continue
		}
		if entry.Branch != "" {
			return entry.Branch
		}
		return filepath.Base(entry.Path)
	}
	return ""
}
