package server

import (
	"net/http"
	"sort"
	"time"

	"lingzhu/internal/event"
)

type usageTotals struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

// usageFor aggregates every provider usage event of one session.
func usageFor(events []event.Event) (usageTotals, int, *usageTotals, time.Time) {
	var totals usageTotals
	calls := 0
	var last *usageTotals
	var lastAt time.Time
	for _, ev := range events {
		if ev.Type != event.Usage {
			continue
		}
		item := usageTotals{
			Input:      usageInt(ev.Data, "input"),
			Output:     usageInt(ev.Data, "output"),
			CacheRead:  usageInt(ev.Data, "cacheRead"),
			CacheWrite: usageInt(ev.Data, "cacheWrite"),
		}
		totals.Input += item.Input
		totals.Output += item.Output
		totals.CacheRead += item.CacheRead
		totals.CacheWrite += item.CacheWrite
		calls++
		last = &item
		lastAt = ev.At
	}
	return totals, calls, last, lastAt
}

// sessionUsage aggregates every provider usage event of one session.
func (s *Server) sessionUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	events, err := s.store.Events(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	totals, calls, last, lastAt := usageFor(events)
	payload := map[string]any{
		"sessionId":  id,
		"calls":      calls,
		"input":      totals.Input,
		"output":     totals.Output,
		"cacheRead":  totals.CacheRead,
		"cacheWrite": totals.CacheWrite,
		"total":      totals.Input + totals.Output + totals.CacheRead + totals.CacheWrite,
	}
	if last != nil {
		payload["last"] = last
		payload["lastAt"] = lastAt.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, payload)
}

// UsageSession is one session's usage row in the workspace summary.
type UsageSession struct {
	SessionID  string `json:"sessionId"`
	Title      string `json:"title,omitempty"`
	Calls      int    `json:"calls"`
	Input      int    `json:"input"`
	Output     int    `json:"output"`
	CacheRead  int    `json:"cacheRead"`
	CacheWrite int    `json:"cacheWrite"`
	Total      int    `json:"total"`
}

// workspaceUsage aggregates usage across every session of the workspace.
func (s *Server) workspaceUsage(w http.ResponseWriter, r *http.Request) {
	infos, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var totals usageTotals
	calls := 0
	rows := []UsageSession{}
	for _, info := range infos {
		if !s.sessionInWorkspace(r, info) {
			continue
		}
		events, err := s.store.Events(info.ID)
		if err != nil {
			continue
		}
		sessionTotals, sessionCalls, _, _ := usageFor(events)
		if sessionCalls == 0 {
			continue
		}
		totals.Input += sessionTotals.Input
		totals.Output += sessionTotals.Output
		totals.CacheRead += sessionTotals.CacheRead
		totals.CacheWrite += sessionTotals.CacheWrite
		calls += sessionCalls
		rows = append(rows, UsageSession{
			SessionID:  info.ID,
			Title:      info.Title,
			Calls:      sessionCalls,
			Input:      sessionTotals.Input,
			Output:     sessionTotals.Output,
			CacheRead:  sessionTotals.CacheRead,
			CacheWrite: sessionTotals.CacheWrite,
			Total:      sessionTotals.Input + sessionTotals.Output + sessionTotals.CacheRead + sessionTotals.CacheWrite,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Total != rows[j].Total {
			return rows[i].Total > rows[j].Total
		}
		return rows[i].SessionID < rows[j].SessionID
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"calls":      calls,
		"input":      totals.Input,
		"output":     totals.Output,
		"cacheRead":  totals.CacheRead,
		"cacheWrite": totals.CacheWrite,
		"total":      totals.Input + totals.Output + totals.CacheRead + totals.CacheWrite,
		"sessions":   rows,
	})
}

func usageInt(data map[string]any, key string) int {
	switch value := data[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	}
	return 0
}
