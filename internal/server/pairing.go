package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultPairingTTL = 60 * time.Second
	maxPairingTTL     = 5 * time.Minute
)

type pairingEntry struct {
	Code      string    `json:"code"`
	SessionID string    `json:"sessionId"`
	ExpiresAt time.Time `json:"expiresAt"`
	ExpiresIn int       `json:"expiresIn"`
}

// pairingManager holds short-lived, single-use pairing codes in memory.
type pairingManager struct {
	mu    sync.Mutex
	items map[string]pairingEntry
	now   func() time.Time
}

func newPairingManager() *pairingManager {
	return &pairingManager{items: map[string]pairingEntry{}, now: time.Now}
}

func pairingKey(sessionID, code string) string {
	return sessionID + "\x00" + code
}

func (m *pairingManager) create(sessionID string, ttl time.Duration) (pairingEntry, error) {
	for attempt := 0; attempt < 8; attempt++ {
		buf := make([]byte, 4)
		if _, err := rand.Read(buf); err != nil {
			return pairingEntry{}, err
		}
		code := hex.EncodeToString(buf)
		m.mu.Lock()
		now := m.now()
		m.purgeLocked(now)
		key := pairingKey(sessionID, code)
		if _, exists := m.items[key]; exists {
			m.mu.Unlock()
			continue
		}
		entry := pairingEntry{
			Code:      code,
			SessionID: sessionID,
			ExpiresAt: now.Add(ttl).UTC(),
			ExpiresIn: int(ttl.Seconds()),
		}
		m.items[key] = entry
		m.mu.Unlock()
		return entry, nil
	}
	return pairingEntry{}, errors.New("could not allocate a pairing code")
}

func (m *pairingManager) list(sessionID string) []pairingEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.purgeLocked(now)
	entries := []pairingEntry{}
	for _, entry := range m.items {
		if entry.SessionID == sessionID {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ExpiresAt.Before(entries[j].ExpiresAt) })
	return entries
}

func (m *pairingManager) redeem(sessionID, code string) (pairingEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.purgeLocked(now)
	key := pairingKey(sessionID, code)
	entry, ok := m.items[key]
	if !ok {
		return pairingEntry{}, false
	}
	delete(m.items, key)
	return entry, true
}

func (m *pairingManager) remove(sessionID, code string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := pairingKey(sessionID, code)
	if _, ok := m.items[key]; !ok {
		return false
	}
	delete(m.items, key)
	return true
}

func (m *pairingManager) purgeLocked(now time.Time) {
	for key, entry := range m.items {
		if !entry.ExpiresAt.After(now) {
			delete(m.items, key)
		}
	}
}

// createPairing issues one short-lived pairing code for a session.
func (s *Server) createPairing(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pairingSession(w, r)
	if !ok {
		return
	}
	var body struct {
		TTLSeconds int `json:"ttlSeconds"`
	}
	if r.Body != nil {
		_ = decodeOptionalJSON(r, &body)
	}
	ttl := defaultPairingTTL
	if body.TTLSeconds > 0 {
		ttl = time.Duration(body.TTLSeconds) * time.Second
	}
	if ttl > maxPairingTTL {
		ttl = maxPairingTTL
	}
	entry, err := s.pairings.create(id, ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"pairing": entry})
}

// listPairings returns active pairing codes for a session.
func (s *Server) listPairings(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pairingSession(w, r)
	if !ok {
		return
	}
	entries := s.pairings.list(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId": id,
		"pairings":  entries,
		"count":     len(entries),
	})
}

// redeemPairing consumes one code and returns the session location.
func (s *Server) redeemPairing(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pairingSession(w, r)
	if !ok {
		return
	}
	code := strings.ToLower(strings.TrimSpace(r.PathValue("code")))
	entry, ok := s.pairings.redeem(id, code)
	if !ok {
		writeError(w, http.StatusNotFound, errText("pairing code not found or expired"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"paired":    true,
		"sessionId": id,
		"directory": s.rootFor(r),
		"pairedAt":  s.pairings.now().UTC(),
		"expiresAt": entry.ExpiresAt,
	})
}

// revokePairing removes one active code.
func (s *Server) revokePairing(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pairingSession(w, r)
	if !ok {
		return
	}
	code := strings.ToLower(strings.TrimSpace(r.PathValue("code")))
	if !s.pairings.remove(id, code) {
		writeError(w, http.StatusNotFound, errText("pairing code not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pairingSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, err := s.store.Info(id); err != nil {
		writeStoreError(w, err)
		return "", false
	}
	return id, true
}

// decodeOptionalJSON tolerates an empty request body.
func decodeOptionalJSON(r *http.Request, out any) error {
	if r.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(r.Body).Decode(out)
}
