package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/session"
	"lingzhu/internal/share"
)

func TestShareEndpoints(t *testing.T) {
	server := newTestServer(t)
	manager, err := share.New(t.TempDir(), share.Config{})
	if err != nil {
		t.Fatal(err)
	}
	server.SetShares(manager)
	if _, err := server.store.Append("s1", event.User("hello share")); err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.Append("s1", event.Assistant("shared answer")); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/share", strings.NewReader(`{}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected share status: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Share share.Record `json:"share"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Share.Token == "" || created.Share.Path != "/share/"+created.Share.Token {
		t.Fatalf("unexpected share record: %+v", created.Share)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, created.Share.Path, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "shared answer") || !strings.Contains(rec.Body.String(), "hello share") {
		t.Fatalf("unexpected share page: %d %s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("unexpected content type: %q", contentType)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/share", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"shared":true`) {
		t.Fatalf("unexpected share status: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/export", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "# ") || !strings.Contains(rec.Body.String(), "shared answer") {
		t.Fatalf("unexpected export: %d %s", rec.Code, rec.Body.String())
	}
	if disposition := rec.Header().Get("Content-Disposition"); !strings.Contains(disposition, ".md") {
		t.Fatalf("unexpected export disposition: %q", disposition)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sessions/s1/share", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected unshare status: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, created.Share.Path, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoked share should 404, got %d", rec.Code)
	}
}

func TestDeleteSessionRevokesShare(t *testing.T) {
	server := newTestServer(t)
	manager, err := share.New(t.TempDir(), share.Config{})
	if err != nil {
		t.Fatal(err)
	}
	server.SetShares(manager)
	if _, err := server.store.Append("s1", event.User("hello")); err != nil {
		t.Fatal(err)
	}
	record, err := manager.Create("s1", "Hello", mustMessages(t, server.store, "s1"))
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sessions/s1", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected delete status: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, record.Path, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("share should be revoked with the session, got %d", rec.Code)
	}
}

func mustMessages(t *testing.T, store *session.Store, id string) []model.Message {
	t.Helper()
	messages, err := store.Messages(id)
	if err != nil {
		t.Fatal(err)
	}
	return messages
}

func TestShareConfigEndpoint(t *testing.T) {
	server := newTestServer(t)
	manager, err := share.New(t.TempDir(), share.Config{Endpoint: "https://share.example/upload", APIKey: "secret-key", Secret: "secret", Expiry: "7d"})
	if err != nil {
		t.Fatal(err)
	}
	server.SetShares(manager)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/share", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Share share.Status `json:"share"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Share.Mode != "manual" || !payload.Share.Enabled || !payload.Share.Remote || payload.Share.Expiry != "7d" {
		t.Fatalf("unexpected share config: %+v", payload.Share)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("share config leaked credentials: %s", rec.Body.String())
	}
}

func TestShareDisabledModeRejectsCreate(t *testing.T) {
	server := newTestServer(t)
	manager, err := share.New(t.TempDir(), share.Config{Mode: "disabled"})
	if err != nil {
		t.Fatal(err)
	}
	server.SetShares(manager)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/share", strings.NewReader(`{}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled sharing should 403, got %d %s", rec.Code, rec.Body.String())
	}
}
