package server

import (
	"encoding/json"
	"log"
	"net/http"
)

// sseWriter owns one event-stream response. Headers are written once and every
// frame goes through the same encoder, so all endpoints stream identically.
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

// newSSEWriter validates streaming support and commits the response headers.
func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errText("streaming unsupported")
	}
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	return &sseWriter{w: w, f: flusher}, nil
}

// write emits one named frame. A marshal failure is logged and skipped: one bad
// event must not tear down a healthy stream.
func (s *sseWriter) write(eventType string, payload any) bool {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("marshal event: %v", err)
		return true
	}
	if _, err := s.w.Write([]byte("event: " + eventType + "\n")); err != nil {
		return false
	}
	if _, err := s.w.Write([]byte("data: ")); err != nil {
		return false
	}
	if _, err := s.w.Write(data); err != nil {
		return false
	}
	_, err = s.w.Write([]byte("\n\n"))
	return err == nil
}

func (s *sseWriter) flush() { s.f.Flush() }
