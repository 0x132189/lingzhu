package server

import (
	"encoding/json"
	"net/http"
	"time"

	"lingzhu/internal/session"
)

type inboxTime struct {
	Created string `json:"created"`
}

type inboxPayload struct {
	Text string         `json:"text"`
	Data map[string]any `json:"data,omitempty"`
}

type inboxItem struct {
	ID        string       `json:"id"`
	SessionID string       `json:"sessionID"`
	Time      inboxTime    `json:"time"`
	Type      string       `json:"type"`
	Kind      string       `json:"kind,omitempty"`
	Payload   inboxPayload `json:"payload"`
	Delivery  string       `json:"delivery"`
}

func (s *Server) listInbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	recovery, err := s.store.Recovery(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	items := make([]inboxItem, 0, len(recovery.Inputs))
	for _, input := range recovery.Inputs {
		created := input.Created
		if created.IsZero() {
			created = time.Now().UTC()
		}
		kind := input.Kind
		if kind == "" {
			kind = session.KindPrompt
		}
		itemType := "user"
		switch kind {
		case session.KindSynthetic:
			itemType = "synthetic"
		case session.KindCompaction:
			itemType = "compaction"
		case session.KindMove:
			itemType = "move"
		}
		items = append(items, inboxItem{
			ID:        input.ID,
			SessionID: id,
			Time:      inboxTime{Created: created.UTC().Format(time.RFC3339Nano)},
			Type:      itemType,
			Kind:      kind,
			Payload:   inboxPayload{Text: input.Text, Data: input.Data},
			Delivery:  normalizeDelivery(input.Delivery),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": items})
}

func normalizeDelivery(delivery string) string {
	if delivery == "" {
		return "steer"
	}
	return delivery
}

func (s *Server) cancelInbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	inputID := r.PathValue("inboxID")
	if inputID != "" {
		if ev, err := s.store.CancelInput(id, inputID); err == nil {
			s.agentFor(id).Publish(id, ev)
		}
		s.agentFor(id).CancelInput(id, inputID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateInbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	var body struct {
		Delivery string `json:"delivery"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	delivery := body.Delivery
	if delivery != "steer" && delivery != "queue" {
		writeError(w, http.StatusBadRequest, errText("delivery must be steer or queue"))
		return
	}
	inputID := r.PathValue("inboxID")
	recovery, err := s.store.Recovery(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	found := false
	for _, input := range recovery.Inputs {
		if input.ID == inputID {
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusConflict, errText("input is no longer pending"))
		return
	}
	ev, err := s.store.UpdateInputDelivery(id, inputID, delivery)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.agentFor(id).Publish(id, ev)
	s.agentFor(id).SetInputDelivery(id, inputID, delivery)
	if delivery == "steer" {
		// Steering wakes execution; queueing and cancellation do not. Nothing
		// else would start the turn a parked item now belongs to.
		s.agentFor(id).Wake(id)
	}
	w.WriteHeader(http.StatusNoContent)
}
