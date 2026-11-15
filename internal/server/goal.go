package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"lingzhu/internal/agent"
	"lingzhu/internal/event"
	"lingzhu/internal/goal"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
)

// getGoal returns the session's goal, or null when none is set.
func (s *Server) getGoal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"goal": s.agentFor(id).Goal(id)})
}

// setGoal creates a goal, or applies one of the control verbs pause, resume
// and clear.
func (s *Server) setGoal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Objective string `json:"objective"`
		Action    string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	info, err := s.store.Info(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	selection := goalSelection(info)

	switch strings.ToLower(strings.TrimSpace(body.Action)) {
	case "pause":
		if err := s.agentFor(id).PauseGoal(id, "Paused by the user"); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"goal": s.agentFor(id).Goal(id)})
		return
	case "resume":
		stream, err := s.agentFor(id).ResumeGoal(r.Context(), id, selection)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		s.streamEvents(w, id, stream)
		return
	case "clear":
		if err := s.agentFor(id).ClearGoal(id); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"goal": nil})
		return
	case "":
	default:
		writeError(w, http.StatusBadRequest, errors.New("action must be pause, resume or clear"))
		return
	}

	text := strings.TrimSpace(body.Objective)
	if text == "" {
		writeError(w, http.StatusBadRequest, errors.New("objective is required"))
		return
	}
	if len([]rune(text)) > goal.MaxObjective {
		writeError(w, http.StatusBadRequest, errors.New("objective is too long"))
		return
	}
	stream, err := s.agentFor(id).StartGoal(r.Context(), id, text, selection)
	if err != nil {
		if errors.Is(err, agent.ErrBusy) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.streamEvents(w, id, stream)
}

// handleGoalPrompt applies `/goal` typed into the composer. The control verbs
// pause, resume and clear act on the current goal; anything else is a new
// objective.
func (s *Server) handleGoalPrompt(w http.ResponseWriter, r *http.Request, text, providerID, modelID string) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "pause":
		if err := s.agentFor(id).PauseGoal(id, "Paused by the user"); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"goal": s.agentFor(id).Goal(id)})
		return
	case "resume":
		stream, err := s.agentFor(id).ResumeGoal(context.Background(), id, provider.Selection{Provider: providerID, Model: modelID})
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		s.streamEvents(w, id, stream)
		return
	case "clear":
		if err := s.agentFor(id).ClearGoal(id); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"goal": nil})
		return
	}
	if text == "" {
		// A bare `/goal` reports the current goal instead of starting one.
		if current := s.agentFor(id).Goal(id); current != nil {
			writeJSON(w, http.StatusOK, map[string]any{"goal": current, "message": goal.Describe(*current)})
			return
		}
		writeError(w, http.StatusBadRequest, errors.New("Usage: /goal <objective> | pause | resume | clear"))
		return
	}
	if len([]rune(text)) > goal.MaxObjective {
		writeError(w, http.StatusBadRequest, errors.New("objective is too long"))
		return
	}
	selection := provider.Selection{Provider: providerID, Model: modelID}
	if selection.Provider == "" && selection.Model == "" {
		if info, err := s.store.Info(id); err == nil {
			selection = goalSelection(info)
		}
	}
	stream, err := s.agentFor(id).StartGoal(context.Background(), id, text, selection)
	if err != nil {
		if errors.Is(err, agent.ErrBusy) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.streamEvents(w, id, stream)
}

// goalSelection resolves the session's stored model so a goal turn reuses it.
func goalSelection(info session.Info) provider.Selection {
	if info.Provider != "" || info.Model != "" {
		return provider.Selection{Provider: info.Provider, Model: info.Model}
	}
	return provider.Selection{}
}

// streamEvents relays a run's events over SSE.
func (s *Server) streamEvents(w http.ResponseWriter, _ string, events <-chan event.Event) {
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
