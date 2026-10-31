// Package server turns the agent event stream into HTTP + SSE.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"lingzhu/internal/agent"
	"lingzhu/internal/event"
	"lingzhu/internal/permission"
	"lingzhu/internal/session"
)

type Server struct {
	agent      *agent.Agent
	store      *session.Store
	done       chan struct{}
	once       sync.Once
	onShutdown func()
}

func New(a *agent.Agent, store *session.Store) *Server {
	return &Server{agent: a, store: store, done: make(chan struct{})}
}

// OnShutdown registers the callback that stops the process. It lets a desktop
// client shut the core down gracefully instead of killing it.
func (s *Server) OnShutdown(fn func()) {
	s.onShutdown = fn
}

// Close stops open event streams so shutdown can finish without waiting for
// clients that are still following a session.
func (s *Server) Close() {
	s.once.Do(func() { close(s.done) })
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /sessions", s.listSessions)
	mux.HandleFunc("GET /sessions/{id}", s.getSession)
	mux.HandleFunc("PATCH /sessions/{id}", s.renameSession)
	mux.HandleFunc("DELETE /sessions/{id}", s.deleteSession)
	mux.HandleFunc("GET /sessions/{id}/messages", s.messages)
	mux.HandleFunc("GET /sessions/{id}/context", s.context)
	mux.HandleFunc("GET /sessions/{id}/events", s.events)
	mux.HandleFunc("POST /sessions/{id}/interrupt", s.interrupt)
	mux.HandleFunc("POST /sessions/{id}/compact", s.compact)
	mux.HandleFunc("GET /sessions/{id}/permission", s.listPermissions)
	mux.HandleFunc("POST /sessions/{id}/permission/{rid}", s.replyPermission)
	mux.HandleFunc("POST /sessions/{id}/prompt", s.prompt)
	mux.HandleFunc("POST /shutdown", s.shutdown)
	return mux
}

// shutdown stops the core gracefully. Clients that spawned the core use it
// instead of killing the process.
func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	if s.onShutdown == nil {
		writeError(w, http.StatusNotImplemented, errText("shutdown is not supported"))
		return
	}
	w.WriteHeader(http.StatusAccepted)
	go s.onShutdown()
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	infos, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": infos})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	info, err := s.store.Info(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) renameSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		writeError(w, http.StatusBadRequest, errText("title is empty"))
		return
	}
	if err := s.store.SetTitle(r.PathValue("id"), body.Title); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.agent.Running(id) {
		writeError(w, http.StatusConflict, agent.ErrBusy)
		return
	}
	if err := s.store.Delete(id); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	messages, err := s.store.Messages(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
}

// context returns the rebuilt conversation and what it costs against the
// window, so a client can show how full the context is.
func (s *Server) context(w http.ResponseWriter, r *http.Request) {
	current, err := s.store.Context(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": current.Messages,
		"tokens":   current.Tokens,
		"window":   s.agent.Window(),
	})
}

// events replays durable events after a sequence number and, with follow=true,
// keeps streaming live events of the session.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	follow := r.URL.Query().Get("follow") == "true"

	// Subscribe before replaying so events appended during the replay are not
	// lost; duplicates are skipped by sequence.
	sub := s.agent.Subscribe(id)
	defer sub.Close()

	replay, err := s.store.EventsAfter(id, after)
	if err != nil {
		writeStoreError(w, err)
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

	last := after
	for _, ev := range replay {
		if !writeEvent(w, ev) {
			return
		}
		last = ev.Seq
	}
	flusher.Flush()
	if !follow {
		return
	}

	for {
		select {
		case <-s.done:
			return
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.Events():
			if !ok {
				return
			}
			if ev.Seq != 0 && ev.Seq <= last {
				continue
			}
			if ev.Seq > last {
				last = ev.Seq
			}
			if !writeEvent(w, ev) {
				return
			}
			flusher.Flush()
		}
	}
}

// interrupt cancels the running turn of a session.
func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"interrupted": s.agent.Interrupt(r.PathValue("id"))})
}

// compact summarizes the session on demand.
func (s *Server) compact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.agent.Compact(context.Background(), id)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, agent.ErrBusy):
		writeError(w, http.StatusConflict, err)
	case strings.HasPrefix(err.Error(), "compact: nothing to compact"):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeStoreError(w, err)
	}
}

func (s *Server) listPermissions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"requests": s.agent.Pending(r.PathValue("id"))})
}

// replyPermission answers a pending permission request.
func (s *Server) replyPermission(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision string `json:"decision"`
		Message  string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	decision := permission.Decision(body.Decision)
	switch decision {
	case permission.Once, permission.Always, permission.Reject:
	default:
		writeError(w, http.StatusBadRequest, errText("decision must be once, always or reject"))
		return
	}
	if err := s.agent.Reply(r.PathValue("rid"), decision, body.Message); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// prompt starts one conversation turn and streams its events as SSE. The turn
// is owned by the session, so closing the connection does not stop it; use the
// interrupt endpoint for that.
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

	events, err := s.agent.Run(context.Background(), r.PathValue("id"), body.Text)
	if err != nil {
		if errors.Is(err, agent.ErrBusy) {
			writeError(w, http.StatusConflict, err)
			return
		}
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

	for {
		select {
		case <-s.done:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if !writeEvent(w, ev) {
				return
			}
			flusher.Flush()
		}
	}
}

// writeEvent emits one SSE frame.
func writeEvent(w http.ResponseWriter, ev event.Event) bool {
	payload, err := json.Marshal(ev)
	if err != nil {
		log.Printf("marshal event: %v", err)
		return true
	}
	if _, err := w.Write([]byte("event: " + string(ev.Type) + "\n")); err != nil {
		return false
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return false
	}
	if _, err := w.Write(payload); err != nil {
		return false
	}
	_, err = w.Write([]byte("\n\n"))
	return err == nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// writeStoreError maps store errors: missing sessions are 404, invalid ids 400.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, errText("session not found"))
	case strings.HasPrefix(err.Error(), "invalid session id"):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

type errText string

func (e errText) Error() string { return string(e) }
