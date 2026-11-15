package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/snapshot"
)

// Snapshots is the worktree snapshot service used by revert endpoints.
type Snapshots interface {
	Capture(ctx context.Context) (string, error)
	Restore(ctx context.Context, commit string) error
	Changed(ctx context.Context, from, to string) ([]string, error)
	Diff(ctx context.Context, from, to string) ([]snapshot.FileDiff, error)
}

// SetSnapshots attaches the snapshot service; nil disables revert endpoints.
func (s *Server) SetSnapshots(snapshots Snapshots) {
	s.snapshots = snapshots
}

type revertBody struct {
	TurnSeq int64 `json:"turnSeq"`
}

// revert restores the worktree to the start of one user turn and hides that
// turn and everything after it until a reapply.
func (s *Server) revert(w http.ResponseWriter, r *http.Request) {
	snapshots := s.snapshotsFor(r)
	if snapshots == nil {
		writeError(w, http.StatusNotImplemented, errText("snapshots are not available"))
		return
	}
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	if s.agentFor(id).Running(id) {
		writeError(w, http.StatusConflict, errText("session is running"))
		return
	}
	var body revertBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	events, err := s.store.Events(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, staged := activeRevert(events); staged {
		writeError(w, http.StatusConflict, errText("a revert is already staged"))
		return
	}
	target, ok := targetTurn(events, body.TurnSeq)
	if !ok {
		writeError(w, http.StatusBadRequest, errText("turn not found"))
		return
	}
	snapshotID, ok := turnSnapshot(events, target.Seq)
	if !ok {
		writeError(w, http.StatusBadRequest, errText("this turn has no snapshot"))
		return
	}

	ctx := r.Context()
	current, err := snapshots.Capture(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	files, err := snapshots.Changed(ctx, snapshotID, current)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := snapshots.Restore(ctx, snapshotID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.emitEvent(id, event.RevertStagedEvent(target.Seq, snapshotID, current, files)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reverted": true,
		"turnSeq":  target.Seq,
		"snapshot": snapshotID,
		"current":  current,
		"files":    files,
	})
}

// reapply restores the worktree and history saved by the latest revert.
func (s *Server) reapply(w http.ResponseWriter, r *http.Request) {
	snapshots := s.snapshotsFor(r)
	if snapshots == nil {
		writeError(w, http.StatusNotImplemented, errText("snapshots are not available"))
		return
	}
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	if s.agentFor(id).Running(id) {
		writeError(w, http.StatusConflict, errText("session is running"))
		return
	}
	events, err := s.store.Events(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	revert, staged := activeRevert(events)
	if !staged {
		writeError(w, http.StatusConflict, errText("no revert is staged"))
		return
	}
	current := eventText(revert, "current")
	if current == "" {
		writeError(w, http.StatusBadRequest, errText("revert has no saved state"))
		return
	}
	if err := snapshots.Restore(r.Context(), current); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	turnSeq := eventInt(revert, "turnSeq")
	if err := s.emitEvent(id, event.ReapplyStagedEvent(turnSeq)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reapplied": true,
		"turnSeq":   turnSeq,
		"snapshot":  current,
	})
}

// revertTurn is one selectable user turn in the revert view.
type revertTurn struct {
	Seq         int64     `json:"seq"`
	Text        string    `json:"text"`
	Created     time.Time `json:"created"`
	HasSnapshot bool      `json:"hasSnapshot"`
}

// revertTurns lists the newest user turns and whether each has a snapshot.
func revertTurns(events []event.Event) []revertTurn {
	snapshots := map[int64]bool{}
	for _, ev := range events {
		if ev.Type == event.TurnSnapshot {
			if snapshot := eventText(ev, "snapshot"); snapshot != "" {
				snapshots[eventInt(ev, "turnSeq")] = true
			}
		}
	}
	out := make([]revertTurn, 0, 16)
	for i := len(events) - 1; i >= 0 && len(out) < 50; i-- {
		ev := events[i]
		if ev.Type != event.UserMessage {
			continue
		}
		text := eventText(ev, "text")
		if len(text) > 200 {
			text = text[:200]
		}
		out = append(out, revertTurn{Seq: ev.Seq, Text: text, Created: ev.At, HasSnapshot: snapshots[ev.Seq]})
	}
	return out
}

// revertState reports the staged revert, if any, and the revertible turns.
func (s *Server) revertState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	events, err := s.store.Events(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	turns := revertTurns(events)
	revert, staged := activeRevert(events)
	if !staged {
		writeJSON(w, http.StatusOK, map[string]any{"active": false, "turns": turns})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active":   true,
		"turnSeq":  eventInt(revert, "turnSeq"),
		"snapshot": eventText(revert, "snapshot"),
		"files":    eventStrings(revert, "files"),
		"turns":    turns,
	})
}

func (s *Server) emitEvent(id string, ev event.Event) error {
	seq, err := s.store.Append(id, ev)
	if err != nil {
		return err
	}
	ev.Seq = seq
	s.agentFor(id).Publish(id, ev)
	return nil
}

// activeRevert returns the latest revert unless a later reapply cleared it.
func activeRevert(events []event.Event) (event.Event, bool) {
	index := -1
	for i, ev := range events {
		if ev.Type == event.SessionReverted {
			index = i
		}
	}
	if index < 0 {
		return event.Event{}, false
	}
	for _, ev := range events[index+1:] {
		if ev.Type == event.SessionReapplied {
			return event.Event{}, false
		}
	}
	return events[index], true
}

func targetTurn(events []event.Event, seq int64) (event.Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Type != event.UserMessage {
			continue
		}
		if seq == 0 || ev.Seq == seq {
			return ev, true
		}
	}
	return event.Event{}, false
}

func turnSnapshot(events []event.Event, turnSeq int64) (string, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Type == event.TurnSnapshot && eventInt(ev, "turnSeq") == turnSeq {
			if snapshot := eventText(ev, "snapshot"); snapshot != "" {
				return snapshot, true
			}
		}
	}
	return "", false
}

func eventText(ev event.Event, key string) string {
	value, _ := ev.Data[key].(string)
	return strings.TrimSpace(value)
}

func eventStrings(ev event.Event, key string) []string {
	raw, ok := ev.Data[key]
	if !ok {
		return []string{}
	}
	switch value := raw.(type) {
	case []string:
		return value
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	return []string{}
}

func eventInt(ev event.Event, key string) int64 {
	switch value := ev.Data[key].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	}
	return 0
}
