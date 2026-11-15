package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"lingzhu/internal/worktree"
)

// SetWorktrees attaches the worktree manager; nil disables the endpoints.
func (s *Server) SetWorktrees(manager *worktree.Manager) {
	s.worktrees = manager
}

func (s *Server) listWorktrees(w http.ResponseWriter, r *http.Request) {
	manager := s.worktreesFor(r)
	if manager == nil {
		writeError(w, http.StatusNotImplemented, errText("worktrees require a git repository"))
		return
	}
	items, err := manager.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if items == nil {
		items = []worktree.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"worktrees": items, "repo": manager.Repo()})
}

func (s *Server) createWorktree(w http.ResponseWriter, r *http.Request) {
	manager := s.worktreesFor(r)
	if manager == nil {
		writeError(w, http.StatusNotImplemented, errText("worktrees require a git repository"))
		return
	}
	var body struct {
		Name   string `json:"name"`
		Branch string `json:"branch"`
		Base   string `json:"base"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	entry, err := manager.Create(r.Context(), body.Name, body.Branch, body.Base)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"worktree": entry})
}

func (s *Server) removeWorktree(w http.ResponseWriter, r *http.Request) {
	manager := s.worktreesFor(r)
	if manager == nil {
		writeError(w, http.StatusNotImplemented, errText("worktrees require a git repository"))
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeError(w, http.StatusBadRequest, errText("path is required"))
		return
	}
	force := r.URL.Query().Get("force") == "true"
	if err := manager.Remove(r.Context(), path, force); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
