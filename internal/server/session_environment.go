package server

import (
	"net/http"
	"runtime"
	"strings"

	"lingzhu/internal/config"
)

// sessionEnvironment describes the process environment a session runs in:
// workspace, shell prefix, selected model and live running state.
func (s *Server) sessionEnvironment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	meta, _ := s.store.Meta(id)
	target := s.targetFor(id)
	cfg := s.configFor(r)
	shell := cfg.Shell
	if len(shell) == 0 {
		shell = config.DefaultShell()
	}
	directory := s.rootFor(r)
	if meta.Directory != "" {
		directory = meta.Directory
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       id,
		"workdir":  strings.TrimSpace(directory),
		"shell":    shell,
		"running":  s.agentFor(id).Running(id),
		"provider": target.ProviderID,
		"model":    target.Model,
		"agent":    meta.Agent,
		"os":       runtime.GOOS,
		"arch":     runtime.GOARCH,
	})
}
