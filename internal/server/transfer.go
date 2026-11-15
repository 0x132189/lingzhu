package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/session"
)

func (s *Server) exportBundle(w http.ResponseWriter, r *http.Request) {
	bundle, err := s.store.Export(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bundle": bundle})
}

func (s *Server) importBundle(w http.ResponseWriter, r *http.Request) {
	var bundle session.Bundle
	if err := json.NewDecoder(r.Body).Decode(&bundle); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.reanchorSnapshots(r, r.Context(), &bundle)
	info, err := s.store.Import(bundle)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"session": info})
}

// reanchorSnapshots points transported turn snapshots at this workspace. The
// source commits live in the sending worktree's shadow repository and cannot be
// restored here, so every transported turn is anchored to the target at import
// time. The original commit is kept for diagnostics.
func (s *Server) reanchorSnapshots(r *http.Request, ctx context.Context, bundle *session.Bundle) {
	snapshots := s.snapshotsFor(r)
	if snapshots == nil {
		return
	}
	needed := false
	for _, ev := range bundle.Events {
		if ev.Type == event.TurnSnapshot {
			needed = true
			break
		}
	}
	if !needed {
		return
	}
	commit, err := snapshots.Capture(ctx)
	if err != nil || commit == "" {
		return
	}
	for i := range bundle.Events {
		ev := bundle.Events[i]
		if ev.Type != event.TurnSnapshot {
			continue
		}
		data := make(map[string]any, len(ev.Data)+2)
		for key, value := range ev.Data {
			data[key] = value
		}
		if source, _ := data["snapshot"].(string); source != "" && source != commit {
			data["sourceSnapshot"] = source
			data["reanchored"] = true
		}
		data["snapshot"] = commit
		ev.Data = data
		bundle.Events[i] = ev
	}
}

// moveSession relocates one session. A directory inside this core only updates
// the session's placement, because the workspace tree already exists; a
// targetUrl sends the whole bundle to another core and deletes the local copy
// after the target imported it. While a turn is running a cross-core transfer
// is queued as a durable control item and applied at the boundary.
func (s *Server) moveSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Directory string `json:"directory"`
		TargetURL string `json:"targetUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !s.requireSession(w, r, id) {
		return
	}
	if directory := strings.TrimSpace(body.Directory); directory != "" {
		s.moveSessionLocal(w, r, id, directory)
		return
	}
	target, err := url.Parse(strings.TrimSpace(body.TargetURL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		writeError(w, http.StatusBadRequest, errText("targetUrl must be an http(s) URL"))
		return
	}
	if s.agentFor(id).Running(id) {
		ev := event.MoveQueuedEvent(target.String())
		seq, err := s.store.Append(id, ev)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		ev.Seq = seq
		s.agentFor(id).Publish(id, ev)
		s.startMove(context.Background(), id, target.String())
		writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "sessionId": id})
		return
	}
	title, err := s.transferSession(id, target.String())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"moved": true, "sessionId": id, "title": title})
}

// moveSessionLocal rehomes a session inside this core. The destination must be
// open already, so the caller opens it with POST /locations first; that keeps
// a move from silently starting a second set of tools for a typo.
func (s *Server) moveSessionLocal(w http.ResponseWriter, r *http.Request, id, directory string) {
	if s.locations == nil {
		writeError(w, http.StatusNotImplemented, errText("this core serves a single workspace"))
		return
	}
	destination, ok := s.locations.Resolve(directory)
	if !ok {
		writeError(w, http.StatusNotFound, errText("destination workspace is not open"))
		return
	}
	if s.agentFor(id).Running(id) {
		writeError(w, http.StatusConflict, errText("session is running"))
		return
	}
	if err := s.store.SetDirectory(id, destination.Root()); err != nil {
		writeStoreError(w, err)
		return
	}
	ev := event.SessionMovedEvent(destination.Root())
	if seq, err := s.store.Append(id, ev); err == nil {
		ev.Seq = seq
		s.agentFor(id).Publish(id, ev)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"moved":     true,
		"sessionId": id,
		"directory": destination.Root(),
	})
}

// ResumeMoves re-arms queued transfers after a process restart.
func (s *Server) ResumeMoves(ctx context.Context) {
	sessions, err := s.store.List()
	if err != nil {
		return
	}
	for _, info := range sessions {
		events, err := s.store.Events(info.ID)
		if err != nil {
			continue
		}
		target := ""
		for _, ev := range events {
			switch ev.Type {
			case event.MoveQueued:
				target = stringEvent(ev, "target")
			case event.MoveFailed:
				target = ""
			}
		}
		if target != "" {
			s.startMove(ctx, info.ID, target)
		}
	}
}

func (s *Server) startMove(ctx context.Context, id, target string) {
	s.moveMu.Lock()
	if s.moveWait[id] {
		s.moveMu.Unlock()
		return
	}
	s.moveWait[id] = true
	s.moveMu.Unlock()
	go s.applyQueuedMove(ctx, id, target)
}

func (s *Server) finishMove(id string) {
	s.moveMu.Lock()
	delete(s.moveWait, id)
	s.moveMu.Unlock()
}

func (s *Server) applyQueuedMove(ctx context.Context, id, target string) {
	defer s.finishMove(id)
	wait, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := s.agentFor(id).WaitIdle(wait, id); err != nil {
		return
	}
	if _, err := s.transferSession(id, target); err != nil {
		ev := event.MoveFailedEvent(err.Error())
		if seq, appendErr := s.store.Append(id, ev); appendErr == nil {
			ev.Seq = seq
			s.agentFor(id).Publish(id, ev)
		}
	}
}

// transferSession exports, posts and deletes one session. Move control events
// are stripped from the bundle so the target does not replay the transfer.
func (s *Server) transferSession(id, targetURL string) (string, error) {
	bundle, err := s.store.Export(id)
	if err != nil {
		return "", err
	}
	filtered := bundle.Events[:0]
	for _, ev := range bundle.Events {
		if ev.Type == event.MoveQueued || ev.Type == event.MoveFailed {
			continue
		}
		filtered = append(filtered, ev)
	}
	bundle.Events = filtered
	// The destination is a different core with its own directories, so the
	// sender's placement cannot be carried over. Clearing it lets the target
	// adopt its own default workspace instead of routing the session back to a
	// path that does not exist there.
	bundle.Directory = ""
	payload, err := json.Marshal(bundle)
	if err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(targetURL, "/") + "/sessions/import"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("target core: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("target core: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	if err := s.store.Delete(id); err != nil {
		return "", err
	}
	if s.shares != nil {
		_ = s.shares.Revoke(id)
	}
	return bundle.Title, nil
}

func stringEvent(ev event.Event, key string) string {
	value, _ := ev.Data[key].(string)
	return value
}
