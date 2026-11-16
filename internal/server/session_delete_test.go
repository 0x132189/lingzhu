package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"lingzhu/internal/event"
)

// A session the core no longer has must answer 404, never an empty transcript:
// a client that receives `200 []` cannot tell a deleted session from one that
// never spoke, and renders a blank conversation for a row it still lists.
func TestMessagesRejectsAMissingSession(t *testing.T) {
	srv, _ := newMultiServer(t, t.TempDir(), t.TempDir())
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/sessions/s-missing/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for a missing session, got %d", resp.StatusCode)
	}
}

// Deleting a session announces it on the live stream, so a cached list in
// another client drops the row instead of keeping one that opens empty.
func TestDeleteAnnouncesSessionDeleted(t *testing.T) {
	srv, store := newMultiServer(t, t.TempDir(), t.TempDir())
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	if _, err := store.Append("s1", event.User("seed")); err != nil {
		t.Fatal(err)
	}
	sub := srv.agent.SubscribeAll()
	defer sub.Close()

	resp, err := http.NewRequest(http.MethodDelete, httpSrv.URL+"/sessions/s1", nil)
	if err != nil {
		t.Fatal(err)
	}
	del, err := http.DefaultClient.Do(resp)
	if err != nil {
		t.Fatal(err)
	}
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", del.StatusCode)
	}

	for {
		select {
		case item, ok := <-sub.Events():
			if !ok {
				t.Fatal("the live stream closed before the deletion was announced")
			}
			if item.Event.Type != event.SessionDeleted {
				continue
			}
			if item.SessionID != "s1" {
				t.Fatalf("want the deleted session, got %q", item.SessionID)
			}
			if id, _ := item.Event.Data["sessionID"].(string); id != "s1" {
				t.Fatalf("the event must name its session: %+v", item.Event.Data)
			}
			// The placement is gone with the session, so the notice names the
			// workspace itself; otherwise a client cannot tell which cached list
			// still holds the row.
			if directory, _ := item.Event.Data["directory"].(string); directory == "" {
				t.Fatalf("the event must name its workspace: %+v", item.Event.Data)
			}
			return
		default:
			t.Fatal("deleting a session must publish session.deleted")
		}
	}
}

// An unknown directory must list nothing. Falling back to the whole store would
// hand one workspace another workspace's sessions.
func TestUnknownDirectoryListsNoSessions(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	srv, store := newMultiServer(t, first, second)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	if _, err := store.Append("s1", event.User("seed")); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDirectory("s1", first); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(httpSrv.URL + "/sessions?directory=" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Sessions) != 0 {
		t.Fatalf("an unknown workspace must list nothing, got %+v", payload.Sessions)
	}

	// The workspace that owns the session still sees it.
	owned, err := http.Get(httpSrv.URL + "/sessions?directory=" + first)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Body.Close()
	if err := json.NewDecoder(owned.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Sessions) != 1 || payload.Sessions[0].ID != "s1" {
		t.Fatalf("the owning workspace must still list it, got %+v", payload.Sessions)
	}
}
