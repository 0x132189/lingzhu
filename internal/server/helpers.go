package server

import (
	"encoding/json"
	"net/http"
)

// requireSession answers 404/400 for a missing or malformed session id and
// reports whether the handler should continue. It replaces the same four lines
// that nearly every session handler used to repeat.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request, id string) bool {
	if _, err := s.store.Info(id); err != nil {
		writeStoreError(w, err)
		return false
	}
	return true
}

// jsonDecode reads a request body into out. It exists so a handler can decode
// without naming the decoder, which keeps one-line helpers readable.
func jsonDecode(r *http.Request, out any) error {
	return json.NewDecoder(r.Body).Decode(out)
}
