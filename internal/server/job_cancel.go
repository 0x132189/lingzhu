package server

import (
	"net/http"
	"strings"
)

const maxJobOutputBytes = 64 * 1024

// jobOutput returns the tail of one background command's captured output.
func (s *Server) jobOutput(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if !s.requireSession(w, r, sessionID) {
		return
	}
	if s.jobs == nil {
		writeError(w, http.StatusNotImplemented, errText("background jobs are not available"))
		return
	}
	id := r.PathValue("jobID")
	item, ok := s.jobs.Get(id)
	if !ok || item.SessionID != sessionID {
		writeError(w, http.StatusNotFound, errText("job not found"))
		return
	}
	output, running, ok := s.jobs.Output(id, maxJobOutputBytes)
	if !ok {
		writeError(w, http.StatusNotFound, errText("job not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": item, "output": output, "running": running})
}

// cancelBackgroundJob stops one running background command.
func (s *Server) cancelBackgroundJob(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if !s.requireSession(w, r, sessionID) {
		return
	}
	if s.jobs == nil {
		writeError(w, http.StatusNotImplemented, errText("background jobs are not available"))
		return
	}
	id := r.PathValue("jobID")
	item, ok := s.jobs.Get(id)
	if !ok || item.SessionID != sessionID {
		writeError(w, http.StatusNotFound, errText("job not found"))
		return
	}
	updated, err := s.jobs.Cancel(id)
	if err != nil {
		if strings.Contains(err.Error(), "not running") {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": updated})
}
