package server

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	"lingzhu/internal/tool"
)

// sessionReference searches the workspace for a literal symbol or text term.
func (s *Server) sessionReference(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	root := s.rootFor(r)
	if strings.TrimSpace(root) == "" {
		writeError(w, http.StatusBadRequest, errText("workspace is not configured"))
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, errText("q is required"))
		return
	}
	subdir := r.URL.Query().Get("path")
	abs, rel, err := resolveWorkspacePath(s.rootFor(r), subdir)
	if err != nil {
		writeFileError(w, err)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		writeFileError(w, err)
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, errText("path is not a directory"))
		return
	}
	limit := 0
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		if parsed, parseErr := strconv.Atoi(value); parseErr == nil {
			limit = parsed
		}
	}
	caseSensitive := isTrue(r.URL.Query().Get("case_sensitive"))
	references, truncated, err := tool.SearchReferences(root, rel, query, limit, caseSensitive)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if references == nil {
		references = []tool.Reference{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":      query,
		"path":       rel,
		"references": references,
		"truncated":  truncated,
	})
}
