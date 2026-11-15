package server

import (
	"net/http"

	"lingzhu/internal/event"
	"lingzhu/internal/snapshot"
)

// sessionDiff returns the worktree changes since the first captured turn of a
// session. It is the data behind the desktop review panel.
func (s *Server) sessionDiff(w http.ResponseWriter, r *http.Request) {
	snapshots := s.snapshotsFor(r)
	if snapshots == nil {
		writeError(w, http.StatusNotImplemented, errText("snapshots are not available"))
		return
	}
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	events, err := s.store.Events(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	base := ""
	for _, ev := range events {
		if ev.Type != event.TurnSnapshot {
			continue
		}
		base = eventText(ev, "snapshot")
		if base != "" {
			break
		}
	}
	if base == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"base":  "",
			"head":  "",
			"diffs": []snapshot.FileDiff{},
		})
		return
	}
	head, err := snapshots.Capture(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	diffs, err := snapshots.Diff(r.Context(), base, head)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if diffs == nil {
		diffs = []snapshot.FileDiff{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"base":  base,
		"head":  head,
		"diffs": diffs,
	})
}
