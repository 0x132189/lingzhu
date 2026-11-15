package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SearchHit is one matching durable event across the workspace.
type SearchHit struct {
	SessionID string    `json:"sessionId"`
	Title     string    `json:"title,omitempty"`
	Seq       int64     `json:"seq"`
	Type      string    `json:"type"`
	At        time.Time `json:"at"`
	Snippet   string    `json:"snippet"`
}

// searchEvents searches durable event payloads across every session.
func (s *Server) searchEvents(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, errText("query is empty"))
		return
	}
	limit := 20
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, errText("limit must be a positive integer"))
			return
		}
		limit = parsed
	}
	if limit > 100 {
		limit = 100
	}
	infos, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	needle := strings.ToLower(query)
	hits := []SearchHit{}
	for _, info := range infos {
		if !s.sessionInWorkspace(r, info) {
			continue
		}
		events, err := s.store.Events(info.ID)
		if err != nil {
			continue
		}
		for i := len(events) - 1; i >= 0 && len(hits) < limit; i-- {
			event := events[i]
			payload, err := json.Marshal(event.Data)
			if err != nil {
				continue
			}
			text := string(payload)
			index := strings.Index(strings.ToLower(text), needle)
			if index < 0 {
				continue
			}
			hits = append(hits, SearchHit{
				SessionID: info.ID,
				Title:     info.Title,
				Seq:       event.Seq,
				Type:      string(event.Type),
				At:        event.At,
				Snippet:   eventSnippet(text, index, len(query)),
			})
		}
		if len(hits) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
}

// eventSnippet returns about 120 runes around the match.
func eventSnippet(text string, byteIndex, queryLen int) string {
	runes := []rune(text)
	before := len([]rune(text[:byteIndex]))
	start := before - 60
	if start < 0 {
		start = 0
	}
	end := before + queryLen + 60
	if end > len(runes) {
		end = len(runes)
	}
	snippet := string(runes[start:end])
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(runes) {
		snippet += "…"
	}
	return snippet
}
