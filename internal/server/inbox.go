package server

import (
	"encoding/json"
	"net/http"
	"time"
)

type inboxTime struct {
	Created string `json:"created"`
}

type inboxPayload struct {
	Text string `json:"text"`
}

type inboxItem struct {
	ID        string       `json:"id"`
	SessionID string       `json:"sessionID"`
	Time      inboxTime    `json:"time"`
	Type      string       `json:"type"`
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
		delivery := input.Delivery
		if delivery == "" {
			delivery = "steer"
		}
		items = append(items, inboxItem{
			ID:        input.ID,
			SessionID: id,
			Time:      inboxTime{Created: created.UTC().Format(time.RFC3339Nano)},
			Type:      "user",
			Payload:   inboxPayload{Text: input.Text},
			Delivery:  delivery,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": items})
}

func (s *Server) cancelInbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	inputID := r.PathValue("inboxID")
	if inputID != "" {
		_, _ = s.store.CancelInput(id, inputID)
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
	if err := s.store.UpdateInputDelivery(id, inputID, delivery); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.agentFor(id).SetInputDelivery(id, inputID, delivery)
	w.WriteHeader(http.StatusNoContent)
}
