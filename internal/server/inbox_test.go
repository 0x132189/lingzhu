package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/event"
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

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/sessions/s1/inbox/inp_1", strings.NewReader(`{"delivery":"steer"}`)))
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
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/sessions/s1/inbox/inp_2", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected delete status: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/s1/inbox", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data) != 1 || listed.Data[0].ID != "inp_1" || listed.Data[0].Delivery != "steer" {
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
