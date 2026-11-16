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
	"lingzhu/internal/provider"
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
	// A moved or interrupted session arrives with an open claim or pending
	// input; continue it here instead of waiting for the next process start.
	if recovery, recoveryErr := s.store.Recovery(info.ID); recoveryErr == nil && (recovery.ClaimID != "" || len(recovery.Inputs) > 0) {
		meta, _ := s.store.Meta(info.ID)
		go func() {
			stream, resumeErr := s.agentFor(info.ID).Resume(context.Background(), info.ID, recovery.ClaimID, provider.Selection{
				Provider: meta.Provider,
				Model:    meta.Model,
			})
			if resumeErr != nil {
				return
			}
			for range stream {
			}
		}()
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

// queuedMove is one move request waiting for a safe turn boundary.
type queuedMove struct {
	inputID   string
	target    string
	directory string
}

// moveSession relocates one session. A directory inside this core only updates
// the session's placement, because the workspace tree already exists; a
// targetUrl sends the whole bundle to another core and deletes the local copy
// after the target imported it. While a turn runs the request becomes a control
// item: the turn stops at its next safe boundary and keeps its claim, so the
// destination continues the same work instead of waiting for a full idle turn.
func (s *Server) moveSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Directory string `json:"directory"`
		TargetURL string `json:"targetUrl"`
		Delivery  string `json:"delivery"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !s.requireSession(w, r, id) {
		return
	}
	delivery := strings.TrimSpace(body.Delivery)
	if delivery != "" && delivery != "steer" && delivery != "queue" {
		writeError(w, http.StatusBadRequest, errText("delivery must be steer or queue"))
		return
	}
	if directory := strings.TrimSpace(body.Directory); directory != "" {
		if s.locations == nil {
			writeError(w, http.StatusNotImplemented, errText("this core serves a single workspace"))
			return
		}
		if _, ok := s.locations.Resolve(directory); !ok {
			writeError(w, http.StatusNotFound, errText("destination workspace is not open"))
			return
		}
		s.applyOrQueueMove(w, r, id, queuedMove{directory: directory}, delivery)
		return
	}
	target, err := url.Parse(strings.TrimSpace(body.TargetURL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		writeError(w, http.StatusBadRequest, errText("targetUrl must be an http(s) URL"))
		return
	}
	s.applyOrQueueMove(w, r, id, queuedMove{target: target.String()}, delivery)
}

// applyOrQueueMove admits the move as a control item when a turn is active; an
// idle session applies it now.
func (s *Server) applyOrQueueMove(w http.ResponseWriter, r *http.Request, id string, move queuedMove, delivery string) {
	moveID, queued, err := s.agentFor(id).QueueMove(id, move.target, move.directory, delivery)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if queued {
		move.inputID = moveID
		s.startMove(context.Background(), id, move)
		writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "sessionId": id})
		return
	}
	if move.target != "" {
		title, err := s.transferSession(id, move.target)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"moved": true, "sessionId": id, "title": title})
		return
	}
	destination, ok := s.locations.Resolve(move.directory)
	if !ok {
		writeError(w, http.StatusNotFound, errText("destination workspace is not open"))
		return
	}
	if err := s.applyLocalMove(id, destination.Root(), ""); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"moved":     true,
		"sessionId": id,
		"directory": destination.Root(),
	})
}

// applyLocalMove switches the session's placement and continues the open claim
// at the destination.
func (s *Server) applyLocalMove(id, directory, moveID string) error {
	if err := s.store.SetDirectory(id, directory); err != nil {
		return err
	}
	ev := event.SessionMovedForInput(directory, moveID)
	if seq, err := s.store.Append(id, ev); err == nil {
		ev.Seq = seq
		s.agentFor(id).Publish(id, ev)
	}
	s.resumeMoved(id)
	return nil
}

// resumeMoved continues the claim a move handoff left open.
func (s *Server) resumeMoved(id string) {
	recovery, err := s.store.Recovery(id)
	if err != nil || recovery.ClaimID == "" {
		return
	}
	meta, _ := s.store.Meta(id)
	go func() {
		stream, resumeErr := s.agentFor(id).Resume(context.Background(), id, recovery.ClaimID, provider.Selection{
			Provider: meta.Provider,
			Model:    meta.Model,
		})
		if resumeErr != nil {
			return
		}
		for range stream {
		}
	}()
}

// ResumeMoves re-arms queued moves after a process restart.
func (s *Server) ResumeMoves(ctx context.Context) {
	recoveries, err := s.store.Recoveries()
	if err != nil {
		return
	}
	for _, recovery := range recoveries {
		for _, item := range recovery.Inputs {
			if item.Kind != session.KindMove {
				continue
			}
			target, _ := item.Data["target"].(string)
			directory, _ := item.Data["directory"].(string)
			s.startMove(ctx, recovery.SessionID, queuedMove{
				inputID:   item.ID,
				target:    target,
				directory: directory,
			})
		}
	}
}

func (s *Server) startMove(ctx context.Context, id string, move queuedMove) {
	s.moveMu.Lock()
	if s.moveWait[id] {
		s.moveMu.Unlock()
		return
	}
	s.moveWait[id] = true
	s.moveMu.Unlock()
	go s.applyQueuedMove(ctx, id, move)
}

func (s *Server) finishMove(id string) {
	s.moveMu.Lock()
	delete(s.moveWait, id)
	s.moveMu.Unlock()
}

func (s *Server) applyQueuedMove(ctx context.Context, id string, move queuedMove) {
	defer s.finishMove(id)
	wait, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := s.agentFor(id).WaitIdle(wait, id); err != nil {
		return
	}
	if move.target != "" {
		if _, err := s.transferSession(id, move.target); err != nil {
			s.failMove(id, move.inputID, err)
		}
		return
	}
	if s.locations == nil {
		s.failMove(id, move.inputID, errText("this core serves a single workspace"))
		return
	}
	destination, ok := s.locations.Resolve(move.directory)
	if !ok {
		s.failMove(id, move.inputID, errText("destination workspace is not open"))
		return
	}
	if err := s.applyLocalMove(id, destination.Root(), move.inputID); err != nil {
		s.failMove(id, move.inputID, err)
	}
}

func (s *Server) failMove(id, moveID string, err error) {
	ev := event.MoveFailedEventForInput(err.Error(), moveID)
	if seq, appendErr := s.store.Append(id, ev); appendErr == nil {
		ev.Seq = seq
		s.agentFor(id).Publish(id, ev)
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
