package server

import (
	"net/http"

	"lingzhu/internal/subagent"
)

// listSessionSubagents returns the child sessions this session still runs, so
// the composer can show them beside background shell work.
func (s *Server) listSessionSubagents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	items := []subagent.Child{}
	if s.subagents != nil {
		for _, child := range s.subagents.Children(id) {
			if loop := s.agentFor(child.ID); loop != nil && !loop.Running(child.ID) {
				continue
			}
			items = append(items, child)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subagents": items})
}
