package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"lingzhu/internal/schedule"
)

// SetSchedules attaches the schedule manager; nil disables the endpoint.
func (s *Server) SetSchedules(manager *schedule.Manager) {
	s.schedules = manager
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	items := []schedule.Item{}
	if s.schedules != nil {
		if sessionID := strings.TrimSpace(r.URL.Query().Get("sessionId")); sessionID != "" {
			items = s.schedules.List(sessionID)
		} else {
			items = s.schedules.All()
		}
	}
	if items == nil {
		items = []schedule.Item{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": items})
}

// createSchedule stores a new schedule owned by one session.
func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	if s.schedules == nil {
		writeError(w, http.StatusNotImplemented, errText("schedules are not available"))
		return
	}
	var body struct {
		SessionID string        `json:"sessionId"`
		Name      string        `json:"name"`
		Prompt    string        `json:"prompt"`
		When      schedule.When `json:"when"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sessionID := strings.TrimSpace(body.SessionID)
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, errText("sessionId is required"))
		return
	}
	if !s.requireSession(w, r, sessionID) {
		return
	}
	item, err := s.schedules.Create(sessionID, schedule.CreateRequest{
		Name:   body.Name,
		Prompt: body.Prompt,
		When:   body.When,
	})
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"schedule": item})
}

// deleteSchedule removes one schedule owned by the given session.
// updateSchedule edits, pauses, or resumes one schedule.
func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request) {
	if s.schedules == nil {
		writeError(w, http.StatusNotImplemented, errText("schedules are not available"))
		return
	}
	var body struct {
		SessionID string         `json:"sessionId"`
		Action    string         `json:"action"`
		Name      *string        `json:"name"`
		Prompt    *string        `json:"prompt"`
		When      *schedule.When `json:"when"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sessionID := strings.TrimSpace(body.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(r.URL.Query().Get("sessionId"))
	}
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, errText("sessionId is required"))
		return
	}
	id := r.PathValue("id")
	action := strings.ToLower(strings.TrimSpace(body.Action))
	var (
		item schedule.Item
		err  error
	)
	switch action {
	case "", "update":
		item, err = s.schedules.Update(id, sessionID, schedule.UpdateRequest{
			Name:   body.Name,
			Prompt: body.Prompt,
			When:   body.When,
		})
	case "pause":
		item, err = s.schedules.Pause(id, sessionID)
	case "resume":
		item, err = s.schedules.Resume(id, sessionID)
	default:
		writeError(w, http.StatusBadRequest, errText("action must be update, pause or resume"))
		return
	}
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedule": item})
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if s.schedules == nil {
		writeError(w, http.StatusNotImplemented, errText("schedules are not available"))
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("sessionId"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, errText("sessionId is required"))
		return
	}
	if err := s.schedules.Delete(r.PathValue("id"), sessionID); err != nil {
		writeScheduleError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeScheduleError(w http.ResponseWriter, err error) {
	if strings.Contains(err.Error(), "not found") {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}
