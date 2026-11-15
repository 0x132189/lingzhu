package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/event"
)

func TestForkSessionEndpoint(t *testing.T) {
	server := newTestServer(t)
	for _, ev := range []event.Event{
		event.User("hello"),
		event.Assistant("hi"),
		event.User("second"),
		event.Assistant("second answer"),
	} {
		if _, err := server.store.Append("s1", ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := server.store.SetTitle("s1", "Original"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/fork", strings.NewReader(`{}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected fork status: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Session struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(payload.Session.ID, "fork_") || payload.Session.Title != "Original (fork)" {
		t.Fatalf("unexpected fork: %+v", payload.Session)
	}
	copied, err := server.store.Messages(payload.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != 4 || copied[0].Content != "hello" || copied[3].Content != "second answer" {
		t.Fatalf("unexpected fork messages: %+v", copied)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/fork", strings.NewReader(`{"before":3,"title":"Cut"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected truncated fork status: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	cut, err := server.store.Messages(payload.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Session.Title != "Cut" || len(cut) != 2 || cut[1].Content != "hi" {
		t.Fatalf("unexpected truncated fork: %+v %+v", payload.Session, cut)
	}

	for _, tc := range []struct {
		path string
		body string
		want int
	}{
		{"/sessions/missing/fork", `{}`, http.StatusNotFound},
		{"/sessions/s1/fork", `{"before":-1}`, http.StatusBadRequest},
		{"/sessions/s1/fork", `{"before":1}`, http.StatusBadRequest},
	} {
		rec = httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Fatalf("%s %s: want %d, got %d %s", tc.path, tc.body, tc.want, rec.Code, rec.Body.String())
		}
	}
}
