package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"lingzhu/internal/event"
	"lingzhu/internal/permission"
)

type sessionPermissionsBody struct {
	Permissions permission.Rules `json:"permissions"`
}

// sessionPermissions returns the session-level overlay.
func (s *Server) sessionPermissions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	rules, err := s.store.Permissions(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"permissions": rules})
}

// setSessionPermissions replaces the session-level overlay. The agent profile's
// rules stay first, so a session can only narrow what the agent allows.
func (s *Server) setSessionPermissions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	var body sessionPermissionsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	for _, rule := range body.Permissions {
		action := strings.TrimSpace(rule.Action)
		if action == "" || (rule.Effect != permission.Allow && rule.Effect != permission.Ask && rule.Effect != permission.Deny) {
			writeError(w, http.StatusBadRequest, errText("invalid permission rule"))
			return
		}
	}
	ev := event.PermissionsEvent(body.Permissions)
	seq, err := s.store.Append(id, ev)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	ev.Seq = seq
	s.agentFor(id).Publish(id, ev)
	writeJSON(w, http.StatusOK, map[string]any{"permissions": body.Permissions})
}
