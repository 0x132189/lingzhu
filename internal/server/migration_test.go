package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"lingzhu/internal/session"
)

type migrationPayload struct {
	Migration session.MigrationStatus `json:"migration"`
	Database  string                  `json:"database"`
}

func TestMigrationStatusAndRun(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/migration", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for migration, got %d: %s", rec.Code, rec.Body.String())
	}
	var payload migrationPayload
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Migration.SchemaVersion < 1 {
		t.Fatalf("schema should be migrated: %+v", payload.Migration)
	}
	if payload.Migration.SessionDir == "" || payload.Database != filepath.Join(payload.Migration.SessionDir, "sessions.db") {
		t.Fatalf("unexpected database location: %+v", payload)
	}
	if payload.Migration.Sessions != 1 || payload.Migration.Events < 1 {
		t.Fatalf("unexpected counts: %+v", payload.Migration)
	}
	if payload.Migration.LegacyBackups == nil || len(payload.Migration.LegacyBackups) != 0 {
		t.Fatalf("unexpected legacy backups: %+v", payload.Migration.LegacyBackups)
	}

	backup := filepath.Join(payload.Migration.SessionDir, "old.jsonl")
	if err := os.WriteFile(backup, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/migration", nil))
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range payload.Migration.LegacyBackups {
		if name == "old.jsonl" {
			found = true
		}
	}
	if !found {
		t.Fatalf("legacy backup not listed: %+v", payload.Migration.LegacyBackups)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/migration", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for re-run, got %d: %s", rec.Code, rec.Body.String())
	}
	var after migrationPayload
	if err := json.NewDecoder(rec.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	if after.Migration.SchemaVersion != payload.Migration.SchemaVersion {
		t.Fatalf("schema changed by re-run: %+v -> %+v", payload.Migration, after.Migration)
	}
	if after.Migration.Sessions != 2 {
		t.Fatalf("legacy session was not imported: %+v", after.Migration)
	}
}
