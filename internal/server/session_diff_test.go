package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"lingzhu/internal/event"
	"lingzhu/internal/snapshot"
)

func TestSessionDiffSinceFirstTurn(t *testing.T) {
	workdir := t.TempDir()
	manager, err := snapshot.New(workdir, t.TempDir())
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	srv, store := newSessionServer(t, func(s *Server) { s.SetSnapshots(manager) })
	if err := os.WriteFile(filepath.Join(workdir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := manager.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	seq, err := store.Append("s1", event.User("change it"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.TurnSnapshotEvent(seq, base)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "b.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/diff", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Base  string              `json:"base"`
		Head  string              `json:"head"`
		Diffs []snapshot.FileDiff `json:"diffs"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Base != base || payload.Head == "" || len(payload.Diffs) != 2 {
		t.Fatalf("unexpected diff payload: %+v", payload)
	}
	files := map[string]snapshot.FileDiff{}
	for _, diff := range payload.Diffs {
		files[diff.File] = diff
	}
	if files["a.txt"].Additions != 1 || files["b.txt"].Status != "added" {
		t.Fatalf("unexpected file diffs: %+v", files)
	}
}
