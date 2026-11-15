package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
)

const (
	defaultMediaLimit = 50
	maxMediaLimit     = 200
)

type mediaItem struct {
	ID   string    `json:"id"`
	Tool string    `json:"tool"`
	Mime string    `json:"mime"`
	Name string    `json:"name,omitempty"`
	Data string    `json:"data"`
	Size int       `json:"size"`
	Seq  int64     `json:"seq"`
	At   time.Time `json:"at"`
}

// sessionMedia lists image and PDF attachments returned by tool calls.
func (s *Server) sessionMedia(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	limit := defaultMediaLimit
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > maxMediaLimit {
		limit = maxMediaLimit
	}
	events, err := s.store.Events(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	items := []mediaItem{}
	truncated := false
	for _, ev := range events {
		if ev.Type != event.ToolResult {
			continue
		}
		raw, ok := ev.Data["attachments"]
		if !ok || raw == nil {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var attachments []model.Attachment
		if err := json.Unmarshal(encoded, &attachments); err != nil {
			continue
		}
		tool, _ := ev.Data["name"].(string)
		for index, attachment := range attachments {
			mime := strings.ToLower(strings.TrimSpace(attachment.Mime))
			if !strings.HasPrefix(mime, "image/") && mime != "application/pdf" {
				continue
			}
			if len(items) >= limit {
				truncated = true
				break
			}
			size := 0
			if data, err := base64.StdEncoding.DecodeString(attachment.Data); err == nil {
				size = len(data)
			}
			items = append(items, mediaItem{
				ID:   strconv.FormatInt(ev.Seq, 10) + "-" + strconv.Itoa(index),
				Tool: tool,
				Mime: mime,
				Name: attachment.Name,
				Data: attachment.Data,
				Size: size,
				Seq:  ev.Seq,
				At:   ev.At,
			})
		}
		if truncated {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId": id,
		"media":     items,
		"count":     len(items),
		"truncated": truncated,
	})
}
