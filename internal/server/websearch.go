package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"lingzhu/internal/websearch"
)

// SetWebSearch attaches the search service; nil or unconfigured disables it.
func (s *Server) SetWebSearch(manager *websearch.Manager) {
	s.websearch = manager
}

func (s *Server) webSearchStatus(w http.ResponseWriter, r *http.Request) {
	status := websearch.Status{}
	if s.websearch != nil {
		status = s.websearch.Status()
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) webSearch(w http.ResponseWriter, r *http.Request) {
	if s.websearch == nil || !s.websearch.Configured() {
		writeError(w, http.StatusNotImplemented, errText("websearch is not configured"))
		return
	}
	var body struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(body.Query) == "" {
		writeError(w, http.StatusBadRequest, errText("query is empty"))
		return
	}
	results, err := s.websearch.Search(r.Context(), body.Query)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if results == nil {
		results = []websearch.Result{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
