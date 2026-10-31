// Package server 把 agent 的事件流转成 HTTP + SSE 协议。
package server

import (
	"encoding/json"
	"log"
	"net/http"

	"lingzhu/internal/agent"
	"lingzhu/internal/session"
)

type Server struct {
	agent *agent.Agent
	store *session.Store
}

func New(a *agent.Agent, store *session.Store) *Server {
	return &Server{agent: a, store: store}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		ids, err := s.store.List()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": ids})
	})
	mux.HandleFunc("GET /sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		messages, err := s.store.Messages(r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
	})
	mux.HandleFunc("POST /sessions/{id}/prompt", s.prompt)
	return mux
}

// prompt 以 SSE 返回一轮对话的全部事件。
func (s *Server) prompt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Text == "" {
		writeError(w, http.StatusBadRequest, errText("text is empty"))
		return
	}

	events, err := s.agent.Run(r.Context(), r.PathValue("id"), body.Text)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errText("streaming unsupported"))
		return
	}
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	for ev := range events {
		payload, err := json.Marshal(ev)
		if err != nil {
			log.Printf("marshal event: %v", err)
			continue
		}
		_, _ = w.Write([]byte("event: " + string(ev.Type) + "\n"))
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(payload)
		_, _ = w.Write([]byte("\n\n"))
		flusher.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

type errText string

func (e errText) Error() string { return string(e) }
