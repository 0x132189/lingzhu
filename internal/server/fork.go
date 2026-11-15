package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"lingzhu/internal/session"
)

func (s *Server) forkSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Before int64  `json:"before"`
		Title  string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Before < 0 {
		writeError(w, http.StatusBadRequest, errText("before must not be negative"))
		return
	}
	newID, err := s.forkID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	info, err := s.store.Fork(id, session.ForkOptions{
		ID:        newID,
		Title:     strings.TrimSpace(body.Title),
		BeforeSeq: body.Before,
	})
	if err != nil {
		if strings.Contains(err.Error(), "fork point has no events") {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"session": info})
}

func (s *Server) forkID() (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		id := "fork_" + hex.EncodeToString(buf)
		exists, err := s.store.HasEvents(id)
		if err != nil {
			return "", err
		}
		if !exists {
			return id, nil
		}
	}
	return "", errText("could not allocate a fork id")
}
