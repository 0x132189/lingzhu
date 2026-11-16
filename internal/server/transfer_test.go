package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/snapshot"
)

func TestMoveSessionBetweenCores(t *testing.T) {
	source := newTestServer(t)
	target := newTestServer(t)
	targetHTTP := httptest.NewServer(target.Handler())
	defer targetHTTP.Close()

	if _, err := source.store.Append("s1", event.User("move me")); err != nil {
		t.Fatal(err)
	}
	if _, err := source.store.Append("s1", event.Assistant("moved answer")); err != nil {
		t.Fatal(err)
	}
	if err := source.store.SetTitle("s1", "Transfer test"); err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{"targetUrl":"` + targetHTTP.URL + `"}`)
	rec := httptest.NewRecorder()
	source.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/move", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected move status: %d %s", rec.Code, rec.Body.String())
	}
	var moved struct {
		Moved     bool   `json:"moved"`
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &moved); err != nil {
		t.Fatal(err)
	}
	if !moved.Moved || moved.SessionID != "s1" {
		t.Fatalf("unexpected move response: %s", rec.Body.String())
	}

	messages, err := target.store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Content != "move me" || messages[1].Content != "moved answer" {
		t.Fatalf("target lost history: %+v", messages)
	}
	info, err := target.store.Info("s1")
	if err != nil || info.Title != "Transfer test" {
		t.Fatalf("target metadata missing: %+v %v", info, err)
	}
	if _, err := source.store.Info("s1"); !os.IsNotExist(err) {
		t.Fatalf("source should be empty after move: %v", err)
	}
}

func TestMoveDuringTurnQueuesAndAppliesAtBoundary(t *testing.T) {
	blocked := &blockProvider{started: make(chan struct{}, 1)}
	source := newServerWith(t, blocked)
	target := newTestServer(t)
	targetHTTP := httptest.NewServer(target.Handler())
	defer targetHTTP.Close()

	stream, err := source.agent.Run(context.Background(), "s1", "blocking turn", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range stream {
		}
	}()
	select {
	case <-blocked.started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}

	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"targetUrl":"` + targetHTTP.URL + `"}`)
	source.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/move", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("running move should be queued, got %d: %s", rec.Code, rec.Body.String())
	}

	source.agent.Interrupt("s1", false)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := target.store.Info("s1"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued move was not applied after the turn boundary")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := source.store.Info("s1"); !os.IsNotExist(err) {
		t.Fatalf("source should be deleted after queued move: %v", err)
	}
}

func TestQueuedMoveRecoversAtStartup(t *testing.T) {
	source := newTestServer(t)
	target := newTestServer(t)
	targetHTTP := httptest.NewServer(target.Handler())
	defer targetHTTP.Close()

	if _, err := source.store.Append("s1", event.User("move me")); err != nil {
		t.Fatal(err)
	}
	if _, err := source.store.Append("s1", event.MoveQueuedEvent(targetHTTP.URL)); err != nil {
		t.Fatal(err)
	}
	source.ResumeMoves(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := target.store.Info("s1"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued move was not applied")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := source.store.Info("s1"); !os.IsNotExist(err) {
		t.Fatalf("source should be deleted after queued move: %v", err)
	}
}

func TestImportRejectsDuplicate(t *testing.T) {
	server := newTestServer(t)
	if _, err := server.store.Append("s1", event.User("hello")); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/bundle", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected bundle status: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Bundle json.RawMessage `json:"bundle"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/import", strings.NewReader(string(payload.Bundle))))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate import should conflict: %d %s", rec.Code, rec.Body.String())
	}
}

func TestImportReanchorsSnapshots(t *testing.T) {
	workdir := t.TempDir()
	manager, err := snapshot.New(workdir, t.TempDir())
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "target.txt"), []byte("target state"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t)
	server.SetSnapshots(manager)

	sourceCommit := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	bundle := session.Bundle{
		ID:    "s1",
		Title: "Moved with snapshot",
		Events: []event.Event{
			event.User("hello"),
			event.New(event.TurnSnapshot, map[string]any{"turnSeq": int64(1), "snapshot": sourceCommit}),
			event.Assistant("world"),
		},
	}
	payload, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/import", strings.NewReader(string(payload))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected import status: %d %s", rec.Code, rec.Body.String())
	}

	events, err := server.store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Type != event.TurnSnapshot {
			continue
		}
		found = true
		snapshotID, _ := ev.Data["snapshot"].(string)
		if snapshotID == "" || snapshotID == sourceCommit || len(snapshotID) < 7 {
			t.Fatalf("snapshot was not re-anchored: %+v", ev.Data)
		}
		if ev.Data["sourceSnapshot"] != sourceCommit || ev.Data["reanchored"] != true {
			t.Fatalf("source snapshot not recorded: %+v", ev.Data)
		}
		if err := manager.Restore(t.Context(), snapshotID); err != nil {
			t.Fatalf("re-anchored snapshot cannot be restored: %v", err)
		}
	}
	if !found {
		t.Fatal("turn snapshot event missing after import")
	}
}

func TestImportWithoutSnapshotsKeepsCommit(t *testing.T) {
	server := newTestServer(t)
	sourceCommit := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	bundle := session.Bundle{
		ID: "s2",
		Events: []event.Event{
			event.User("hello"),
			event.New(event.TurnSnapshot, map[string]any{"turnSeq": int64(1), "snapshot": sourceCommit}),
			event.Assistant("world"),
		},
	}
	payload, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/import", strings.NewReader(string(payload))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected import status: %d %s", rec.Code, rec.Body.String())
	}
	events, err := server.store.Events("s2")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == event.TurnSnapshot {
			if ev.Data["snapshot"] != sourceCommit {
				t.Fatalf("snapshot should be untouched without a manager: %+v", ev.Data)
			}
		}
	}
}
