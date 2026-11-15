package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/session"
)

func appendMediaEvents(t *testing.T, store *session.Store) {
	t.Helper()
	image := base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
	text := base64.StdEncoding.EncodeToString([]byte("TEXTDATA"))
	pdf := base64.StdEncoding.EncodeToString([]byte("PDFDATA"))
	first := event.ToolResultWithAttachments("call_1", "read", "attached", "", false, []model.Attachment{
		{Mime: "image/png", Name: "a.png", Data: image},
		{Mime: "text/plain", Name: "a.txt", Data: text},
	})
	second := event.ToolResultWithAttachments("call_2", "image_generate", "generated", "", false, []model.Attachment{
		{Mime: "application/pdf", Name: "b.pdf", Data: pdf},
		{Mime: "image/jpeg", Name: "c.jpg", Data: image},
	})
	for _, ev := range []event.Event{first, second} {
		if _, err := store.Append("s1", ev); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSessionMedia(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	seedSession(t, store, "s2")
	appendMediaEvents(t, store)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/media", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for media, got %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		SessionID string      `json:"sessionId"`
		Media     []mediaItem `json:"media"`
		Count     int         `json:"count"`
		Truncated bool        `json:"truncated"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.SessionID != "s1" || payload.Count != 3 || payload.Truncated {
		t.Fatalf("unexpected media header: %+v", payload)
	}
	if len(payload.Media) != 3 {
		t.Fatalf("want 3 media items, got %+v", payload.Media)
	}
	wantMimes := []string{"image/png", "application/pdf", "image/jpeg"}
	for index, item := range payload.Media {
		if item.Mime != wantMimes[index] {
			t.Fatalf("media %d mime = %q, want %q", index, item.Mime, wantMimes[index])
		}
		if item.ID == "" || item.Data == "" || item.Seq == 0 || item.At.IsZero() {
			t.Fatalf("incomplete media item: %+v", item)
		}
	}
	if payload.Media[0].Tool != "read" || payload.Media[0].Name != "a.png" || payload.Media[0].Size != 7 {
		t.Fatalf("unexpected first media item: %+v", payload.Media[0])
	}
	if payload.Media[1].Tool != "image_generate" || payload.Media[1].Size != 7 {
		t.Fatalf("unexpected second media item: %+v", payload.Media[1])
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/media?limit=1", nil))
	var limited struct {
		Media     []mediaItem `json:"media"`
		Truncated bool        `json:"truncated"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&limited); err != nil {
		t.Fatal(err)
	}
	if len(limited.Media) != 1 || !limited.Truncated {
		t.Fatalf("limit should truncate: %+v", limited)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s2/media", nil))
	var empty struct {
		Media []mediaItem `json:"media"`
		Count int         `json:"count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&empty); err != nil {
		t.Fatal(err)
	}
	if empty.Count != 0 || empty.Media == nil {
		t.Fatalf("empty session should return an empty list: %+v", empty)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/missing/media", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing session should 404, got %d", rec.Code)
	}
}
