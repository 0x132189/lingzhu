// Package server turns the agent event stream into HTTP + SSE.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"lingzhu/internal/agent"
	"lingzhu/internal/permission"
	"lingzhu/internal/provider"
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
	mux.HandleFunc("GET /events", s.allEvents)
	mux.HandleFunc("POST /sessions/{id}/interrupt", s.interrupt)
	mux.HandleFunc("POST /sessions/{id}/compact", s.compact)
	mux.HandleFunc("GET /sessions/{id}/permission", s.listPermissions)
	mux.HandleFunc("POST /sessions/{id}/permission/{rid}", s.replyPermission)
	mux.HandleFunc("POST /sessions/{id}/prompt", s.prompt)
	mux.HandleFunc("POST /sessions/{id}/input", s.input)
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
	for i := range infos {
		infos[i].Running = s.agent.Running(infos[i].ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": infos})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	info, err := s.store.Info(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	info.Running = s.agent.Running(info.ID)
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) renameSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Title    *string `json:"title"`
		Provider *string `json:"provider"`
		Model    *string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Title == nil && body.Provider == nil && body.Model == nil {
		writeError(w, http.StatusBadRequest, errText("nothing to update"))
		return
	}
	if _, err := s.store.Info(id); err != nil {
		writeStoreError(w, err)
		return
	}
	if body.Title != nil {
		if strings.TrimSpace(*body.Title) == "" {
			writeError(w, http.StatusBadRequest, errText("title is empty"))
			return
		}
		if err := s.store.SetTitle(id, *body.Title); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	if body.Provider != nil || body.Model != nil {
		meta, _ := s.store.Meta(id)
		selection := provider.Selection{Provider: meta.Provider, Model: meta.Model}
		if body.Provider != nil {
			selection.Provider = *body.Provider
		}
		if body.Model != nil {
			selection.Model = *body.Model
		}
		target, err := s.agent.Resolve(selection)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.store.SetModel(id, target.ProviderID, target.Model); err != nil {
			writeStoreError(w, err)
			return
		}
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
	target := s.targetFor(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": current.Messages,
		"tokens":   current.Tokens,
		"window":   target.Window,
		"provider": target.ProviderID,
		"model":    target.Model,
		"running":  s.agent.Running(r.PathValue("id")),
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
	stream, err := newSSEWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	last := after
	for _, ev := range replay {
		if !stream.write(string(ev.Type), ev) {
			return
		}
		last = ev.Seq
	}
	stream.flush()
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
			if !stream.write(string(ev.Type), ev) {
				return
			}
			stream.flush()
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
	target := s.targetFor(id)
	err := s.agent.Compact(context.Background(), id, provider.Selection{Provider: target.ProviderID, Model: target.Model})
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

// targetFor resolves the session's saved selection, falling back to the
// configured default when the provider or model no longer exists.
func (s *Server) targetFor(id string) provider.Target {
	meta, _ := s.store.Meta(id)
	target, err := s.agent.Resolve(provider.Selection{Provider: meta.Provider, Model: meta.Model})
	if err != nil {
		return s.agent.DefaultTarget()
	}
	return target
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
		Text     string `json:"text"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Text == "" {
		writeError(w, http.StatusBadRequest, errText("text is empty"))
		return
	}

	id := r.PathValue("id")
	meta, _ := s.store.Meta(id)
	selection := provider.Selection{Provider: meta.Provider, Model: meta.Model}
	if body.Provider != "" || body.Model != "" {
		selection = provider.Selection{Provider: body.Provider, Model: body.Model}
	}
	target, err := s.agent.Resolve(selection)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.SetModel(id, target.ProviderID, target.Model); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	events, err := s.agent.Run(context.Background(), id, body.Text, provider.Selection{Provider: target.ProviderID, Model: target.Model})
	if err != nil {
		if errors.Is(err, agent.ErrBusy) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}

	stream, err := newSSEWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	for {
		select {
		case <-s.done:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if !stream.write(string(ev.Type), ev) {
				return
			}
			stream.flush()
		}
	}
}

// input appends a prompt to the active turn. It never starts a run: clients
// call prompt when idle, and input only while an SSE stream is already open.
func (s *Server) input(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeError(w, http.StatusBadRequest, errText("text is empty"))
		return
	}
	if _, err := s.agent.Enqueue(r.PathValue("id"), body.Text); err != nil {
		if errors.Is(err, agent.ErrNotRunning) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusTooManyRequests, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}

// allEvents streams events from every session, each tagged with its session ID.
// It carries live events only; clients replay history through the per-session
// endpoint.
func (s *Server) allEvents(w http.ResponseWriter, r *http.Request) {
	sub := s.agent.SubscribeAll()
	defer sub.Close()

	stream, err := newSSEWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	stream.flush()

	for {
		select {
		case <-s.done:
			return
		case <-r.Context().Done():
			return
		case item, ok := <-sub.Events():
			if !ok {
				return
			}
			payload := map[string]any{
				"sessionId": item.SessionID,
				"seq":       item.Event.Seq,
				"type":      item.Event.Type,
				"at":        item.Event.At,
				"data":      item.Event.Data,
			}
			if !stream.write(string(item.Event.Type), payload) {
				return
			}
			stream.flush()
		}
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
