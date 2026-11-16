package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
)

func TestInboxEndpoints(t *testing.T) {
	server := newTestServer(t)
	if _, err := server.store.Append("s1", event.User("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.QueueInputDelivery("s1", "inp_1", "steer me", "queue"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.store.QueueInput("s1", "inp_2", "second"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/inbox", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected list status: %d %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Data []struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
			Type      string `json:"type"`
			Delivery  string `json:"delivery"`
			Payload   struct {
				Text string `json:"text"`
			} `json:"payload"`
			Time struct {
				Created string `json:"created"`
			} `json:"time"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data) != 2 {
		t.Fatalf("unexpected inbox: %s", rec.Body.String())
	}
	first := listed.Data[0]
	if first.ID != "inp_1" || first.SessionID != "s1" || first.Type != "user" || first.Delivery != "queue" || first.Payload.Text != "steer me" || first.Time.Created == "" {
		t.Fatalf("unexpected first item: %+v", first)
	}
	if listed.Data[1].Delivery != "steer" {
		t.Fatalf("default delivery should be steer: %+v", listed.Data[1])
	}

	// Parking a steer as a queued prompt must not start a turn, so the item
	// stays visible.
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/sessions/s1/inbox/inp_2", strings.NewReader(`{"delivery":"queue"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected patch status: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/sessions/s1/inbox/inp_1", strings.NewReader(`{"delivery":"bogus"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid delivery should 400: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/sessions/s1/inbox/missing", strings.NewReader(`{"delivery":"steer"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("missing pending item should 409: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sessions/s1/inbox/inp_1", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected delete status: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/inbox", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data) != 1 || listed.Data[0].ID != "inp_2" || listed.Data[0].Delivery != "queue" {
		t.Fatalf("unexpected inbox after cancel: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/missing/inbox", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing session should 404: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sessions/s1/inbox/unknown", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancelling an unknown item should be a no-op: %d", rec.Code)
	}
}

// A queued compaction is visible in the inbox with an identity and can be
// cancelled before the boundary runs it.
func TestInboxListsAndCancelsCompaction(t *testing.T) {
	p := &blockProvider{started: make(chan struct{}, 1)}
	srv := httptest.NewServer(newServerWith(t, p).Handler())
	defer srv.Close()

	resp := prompt(t, srv.URL, "s1", "hello")
	defer resp.Body.Close()
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not reach the provider")
	}
	compact := post(t, srv.URL+"/sessions/s1/compact", `{"delivery":"steer"}`)
	if compact.StatusCode != http.StatusNoContent {
		t.Fatalf("compact: want 204, got %d", compact.StatusCode)
	}

	respList, err := http.Get(srv.URL + "/sessions/s1/inbox")
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Data []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Delivery string `json:"delivery"`
		} `json:"data"`
	}
	if err := json.NewDecoder(respList.Body).Decode(&listed); err != nil {
		respList.Body.Close()
		t.Fatal(err)
	}
	respList.Body.Close()
	compactionID := ""
	for _, item := range listed.Data {
		if item.Type == "compaction" {
			compactionID = item.ID
		}
	}
	if compactionID == "" {
		t.Fatalf("queued compaction missing from the inbox: %+v", listed.Data)
	}

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/sessions/s1/inbox/"+compactionID, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cancelled.Body.Close()
	if cancelled.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel compaction: want 204, got %d", cancelled.StatusCode)
	}
	respList, err = http.Get(srv.URL + "/sessions/s1/inbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(respList.Body).Decode(&listed); err != nil {
		respList.Body.Close()
		t.Fatal(err)
	}
	respList.Body.Close()
	for _, item := range listed.Data {
		if item.Type == "compaction" {
			t.Fatalf("cancelled compaction is still pending: %+v", listed.Data)
		}
	}
	post(t, srv.URL+"/sessions/s1/interrupt", "")
	_, _ = io.ReadAll(resp.Body)
}

// Changing a parked item to steer is a request for it to run, so the mutation
// itself must wake an idle session.
func TestInboxSteeringWakesIdleSession(t *testing.T) {
	server := newTestServer(t)
	if _, err := server.store.QueueInputDelivery("s1", "inp_1", "wake me", "queue"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/sessions/s1/inbox/inp_1", strings.NewReader(`{"delivery":"steer"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected patch status: %d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		messages, err := server.store.Messages("s1")
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) == 2 && messages[0].Content == "wake me" && messages[1].Role == model.RoleAssistant {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("steering change did not wake the session: %+v", messages)
		}
		time.Sleep(10 * time.Millisecond)
	}
	recovery, err := server.store.Recovery("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.Inputs) != 0 || recovery.ClaimID != "" {
		t.Fatalf("steering input should be delivered exactly once: %+v", recovery)
	}
}
