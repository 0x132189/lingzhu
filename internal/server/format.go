package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"lingzhu/internal/format"
)

// SetFormatters attaches the formatter manager; nil disables format endpoints.
func (s *Server) SetFormatters(manager *format.Manager) {
	s.formatters = manager
}

func (s *Server) listFormatters(w http.ResponseWriter, r *http.Request) {
	items := []format.Status{}
	if manager := s.formattersFor(r); manager != nil {
		items = manager.Status()
	}
	writeJSON(w, http.StatusOK, map[string]any{"formatters": items})
}

func (s *Server) formatFile(w http.ResponseWriter, r *http.Request) {
	manager := s.formattersFor(r)
	if manager == nil {
		writeError(w, http.StatusNotImplemented, errText("formatters are not configured"))
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	path, ok := insideRoot(manager, body.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, errText("path must stay inside the workspace"))
		return
	}
	result, err := manager.Format(r.Context(), path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"format": result})
}

// insideRoot resolves a request path relative to the formatter workspace and
// rejects anything outside it.
func insideRoot(manager *format.Manager, raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || manager == nil {
		return "", false
	}
	root := manager.Root()
	if root == "" {
		return "", false
	}
	path := raw
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return path, true
}
