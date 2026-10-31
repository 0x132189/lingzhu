package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionLifecycle(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	resp := prompt(t, srv.URL, "s1", "hello")
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	// Rename.
	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/sessions/s1", strings.NewReader(`{"title":"Renamed"}`))
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	renamed.Body.Close()
	if renamed.StatusCode != http.StatusNoContent {
		t.Fatalf("rename: want 204, got %d", renamed.StatusCode)
	}

	var info struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	get(t, srv.URL+"/sessions/s1", &info)
	if info.ID != "s1" || info.Title != "Renamed" {
		t.Fatalf("unexpected session info: %+v", info)
	}

	var list struct {
		Sessions []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"sessions"`
	}
	get(t, srv.URL+"/sessions", &list)
	if len(list.Sessions) != 1 || list.Sessions[0].Title != "Renamed" {
		t.Fatalf("unexpected session list: %+v", list.Sessions)
	}

	var context struct {
		Tokens   int `json:"tokens"`
		Window   int `json:"window"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	get(t, srv.URL+"/sessions/s1/context", &context)
	if len(context.Messages) != 2 || context.Tokens <= 0 {
		t.Fatalf("unexpected context: %+v", context)
	}

	// Delete.
	req, err = http.NewRequest(http.MethodDelete, srv.URL+"/sessions/s1", nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	deleted.Body.Close()
	if deleted.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d", deleted.StatusCode)
	}
	missing, err := http.Get(srv.URL + "/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 after delete, got %d", missing.StatusCode)
	}
}

func TestInterruptStopsRunAndRejectsConcurrentTurn(t *testing.T) {
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

	busy := prompt(t, srv.URL, "s1", "again")
	busy.Body.Close()
	if busy.StatusCode != http.StatusConflict {
		t.Fatalf("want 409 for a concurrent turn, got %d", busy.StatusCode)
	}

	interrupted := post(t, srv.URL+"/sessions/s1/interrupt", "")
	if interrupted.StatusCode != http.StatusOK {
		t.Fatalf("interrupt: want 200, got %d", interrupted.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"reason":"interrupted"`) {
		t.Fatalf("run should end interrupted:\n%s", body)
	}
}

func TestEventLogReplaysAndFollows(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	resp := prompt(t, srv.URL, "s1", "hello")
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	replay, err := http.Get(srv.URL + "/sessions/s1/events?after=0")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(replay.Body)
	replay.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{"event: user.message", "event: assistant.message", `"seq":1`, `"seq":2`} {
		if !strings.Contains(text, want) {
			t.Fatalf("replay missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "event: text.delta") {
		t.Fatalf("replay must only contain durable events:\n%s", text)
	}

	// Tail from the last durable event, then follow a new turn live.
	follow, err := http.Get(srv.URL + "/sessions/s1/events?after=2&follow=true")
	if err != nil {
		t.Fatal(err)
	}
	defer follow.Body.Close()
	if follow.StatusCode != http.StatusOK {
		t.Fatalf("follow: want 200, got %d", follow.StatusCode)
	}

	second := prompt(t, srv.URL, "s1", "again")
	_, _ = io.ReadAll(second.Body)
	second.Body.Close()

	done := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(follow.Body)
		var b strings.Builder
		for scanner.Scan() {
			b.WriteString(scanner.Text())
			b.WriteString("\n")
			if strings.Contains(b.String(), "event: run.end") {
				break
			}
		}
		done <- b.String()
	}()
	select {
	case live := <-done:
		if !strings.Contains(live, "event: text.delta") {
			t.Fatalf("follow should deliver live deltas:\n%s", live)
		}
		if !strings.Contains(live, `"seq":3`) {
			t.Fatalf("follow should deliver the new durable events:\n%s", live)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follow stream did not deliver the turn")
	}
}

func TestCompactEndpoint(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	empty := post(t, srv.URL+"/sessions/s1/compact", "")
	if empty.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty compact: want 400, got %d", empty.StatusCode)
	}

	resp := prompt(t, srv.URL, "s1", "hello")
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if got := post(t, srv.URL+"/sessions/s1/compact", ""); got.StatusCode != http.StatusNoContent {
		t.Fatalf("compact: want 204, got %d", got.StatusCode)
	}

	var context struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	get(t, srv.URL+"/sessions/s1/context", &context)
	if len(context.Messages) != 1 || !strings.Contains(context.Messages[0].Content, "<conversation-checkpoint>") {
		t.Fatalf("history was not compacted: %+v", context.Messages)
	}
}

func TestCloseEndsFollowStreams(t *testing.T) {
	server := newTestServer(t)
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sessions/s1/events?follow=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(drained)
	}()

	server.Close()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the server should end open event streams")
	}
}

func TestShutdownEndpoint(t *testing.T) {
	server := newTestServer(t)
	called := make(chan struct{})
	server.OnShutdown(func() { close(called) })
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()

	resp := post(t, srv.URL+"/shutdown", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not called")
	}
}

func TestShutdownWithoutCallback(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	if got := post(t, srv.URL+"/shutdown", ""); got.StatusCode != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d", got.StatusCode)
	}
}

func get(t *testing.T, url string, target any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}
