package server

import (
	"net/http"
	"strconv"
	"strings"

	"lingzhu/internal/share"
)

// SetShares attaches the share manager; nil disables sharing endpoints.
func (s *Server) SetShares(manager *share.Manager) {
	s.shares = manager
}

func (s *Server) shareSession(w http.ResponseWriter, r *http.Request) {
	if s.shares == nil {
		writeError(w, http.StatusNotImplemented, errText("sharing is not available"))
		return
	}
	if !s.shares.Enabled() {
		writeError(w, http.StatusForbidden, errText("sharing is disabled"))
		return
	}
	id := r.PathValue("id")
	info, err := s.store.Info(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	messages, err := s.store.Messages(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	title := info.Title
	if strings.TrimSpace(title) == "" {
		title = "Session " + id
	}
	record, err := s.shares.Create(id, title, messages)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"share": record})
}

func (s *Server) shareStatus(w http.ResponseWriter, r *http.Request) {
	if s.shares == nil {
		writeJSON(w, http.StatusOK, map[string]any{"shared": false})
		return
	}
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	record, ok, err := s.shares.BySession(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"shared": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shared": true, "share": record})
}

func (s *Server) unshareSession(w http.ResponseWriter, r *http.Request) {
	if s.shares != nil {
		if err := s.shares.Revoke(r.PathValue("id")); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) serveShare(w http.ResponseWriter, r *http.Request) {
	if s.shares == nil {
		http.NotFound(w, r)
		return
	}
	_, page, err := s.shares.Get(r.PathValue("token"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

func (s *Server) exportSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, err := s.store.Info(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	messages, err := s.store.Messages(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	title := info.Title
	if strings.TrimSpace(title) == "" {
		title = "Session " + id
	}
	body := share.RenderMarkdown(title, messages)
	filename := safeFilename(title) + ".md"
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

func safeFilename(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "session"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func (s *Server) shareConfig(w http.ResponseWriter, r *http.Request) {
	status := share.Status{Mode: "disabled"}
	if s.shares != nil {
		status = s.shares.Status()
	}
	writeJSON(w, http.StatusOK, map[string]any{"share": status})
}
