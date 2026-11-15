package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"lingzhu/internal/event"
	"lingzhu/internal/snapshot"
)

func revertFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func revertRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRevertRestoresFilesAndHistory(t *testing.T) {
	server := newTestServer(t)
	workdir := t.TempDir()
	snapshots, err := snapshot.New(workdir, t.TempDir())
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	server.SetSnapshots(snapshots)

	target := filepath.Join(workdir, "a.txt")
	added := filepath.Join(workdir, "b.txt")
	late := filepath.Join(workdir, "c.txt")
	revertFile(t, target, "one")
	first, err := snapshots.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	firstSeq, err := server.store.Append("s1", event.User("first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.Append("s1", event.TurnSnapshotEvent(firstSeq, first)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.Append("s1", event.Assistant("first answer")); err != nil {
		t.Fatal(err)
	}

	revertFile(t, target, "two")
	revertFile(t, added, "added")
	second, err := snapshots.Capture(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	secondSeq, err := server.store.Append("s1", event.User("second"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.Append("s1", event.TurnSnapshotEvent(secondSeq, second)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.Append("s1", event.Assistant("second answer")); err != nil {
		t.Fatal(err)
	}

	revertFile(t, target, "three")
	revertFile(t, late, "late")

	body := strings.NewReader(`{"turnSeq":` + strconv.FormatInt(secondSeq, 10) + `}`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/revert", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected revert status: %d %s", rec.Code, rec.Body.String())
	}
	if got := revertRead(t, target); got != "two" {
		t.Fatalf("a.txt not restored: %q", got)
	}
	if got := revertRead(t, added); got != "added" {
		t.Fatalf("b.txt should survive: %q", got)
	}
	if _, err := os.Stat(late); !os.IsNotExist(err) {
		t.Fatalf("c.txt should be removed, err=%v", err)
	}

	messages, err := server.store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	rendered, _ := json.Marshal(messages)
	if strings.Contains(string(rendered), "second") {
		t.Fatalf("reverted turn is still visible: %s", rendered)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/revert", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"active":true`) {
		t.Fatalf("unexpected revert state: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/reapply", strings.NewReader(`{}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected reapply status: %d %s", rec.Code, rec.Body.String())
	}
	if got := revertRead(t, target); got != "three" {
		t.Fatalf("reapply did not restore current files: %q", got)
	}
	if got := revertRead(t, late); got != "late" {
		t.Fatalf("reapply did not restore the late file: %q", got)
	}
	messages, err = server.store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	rendered, _ = json.Marshal(messages)
	if !strings.Contains(string(rendered), "second answer") {
		t.Fatalf("reapply did not restore history: %s", rendered)
	}
}
