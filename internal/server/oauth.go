package server

import (
	"encoding/json"
	"net/http"

	"lingzhu/internal/oauth"
)

// SetOAuth attaches the OAuth manager; nil disables OAuth endpoints.
func (s *Server) SetOAuth(manager *oauth.Manager) {
	s.oauth = manager
}

func (s *Server) listOAuth(w http.ResponseWriter, r *http.Request) {
	items := []oauth.Status{}
	if s.oauth != nil {
		items = s.oauth.List()
	}
	writeJSON(w, http.StatusOK, map[string]any{"oauth": items})
}

func (s *Server) oauthStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	status, ok := s.oauth.Status(name)
	if !ok {
		writeError(w, http.StatusNotFound, errText("oauth endpoint is not configured"))
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) oauthRefresh(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		writeError(w, http.StatusNotImplemented, errText("oauth is not configured"))
		return
	}
	name := r.PathValue("name")
	if _, err := s.oauth.Refresh(r.Context(), name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	status, _ := s.oauth.Status(name)
	writeJSON(w, http.StatusOK, map[string]any{"refreshed": true, "oauth": status})
}

func (s *Server) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		writeError(w, http.StatusNotImplemented, errText("oauth is not configured"))
		return
	}
	authURL, state, err := s.oauth.StartAuthorization(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": authURL, "state": state})
}

func (s *Server) oauthToken(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		writeError(w, http.StatusNotImplemented, errText("oauth is not configured"))
		return
	}
	var body struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("name")
	if _, err := s.oauth.ExchangeWithState(r.Context(), name, body.Code, body.State); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	status, _ := s.oauth.Status(name)
	writeJSON(w, http.StatusOK, map[string]any{"authorized": true, "oauth": status})
}

func (s *Server) oauthRevoke(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.oauth.Revoke(r.PathValue("name")); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
