package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/agent"
	"lingzhu/internal/model"
	"lingzhu/internal/provider"
	"lingzhu/internal/tool"
)

func TestSessionMessageLogAndSynthetic(t *testing.T) {
	store := testStore(t)
	catalog := provider.Static("test", "Test", staticProvider{}, []provider.ModelSpec{{ID: "test-model", Name: "test-model"}}, "test-model")
	srv := httptest.NewServer(New(agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 2}), store).Handler())
	defer srv.Close()

	seed, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, seed.Body); err != nil {
		t.Fatal(err)
	}
	seed.Body.Close()

	resp, err := http.Get(srv.URL + "/sessions/s1/message")
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(listed.Messages) != 2 || listed.Messages[0].Content != "hello" || listed.Messages[1].Content != "hi!" {
		t.Fatalf("unexpected message listing: %+v", listed.Messages)
	}

	resp, err = http.Get(srv.URL + "/sessions/s1/log")
	if err != nil {
		t.Fatal(err)
	}
	var log struct {
		Events []struct {
			Type string `json:"type"`
			Seq  int64  `json:"seq"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&log); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(log.Events) == 0 || log.Events[0].Type != "user.message" || log.Events[0].Seq == 0 {
		t.Fatalf("unexpected session log: %+v", log.Events)
	}

	resp, err = http.Post(srv.URL+"/sessions/s1/synthetic", "application/json", strings.NewReader(`{"text":"external note"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for synthetic, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	var messages []model.Message
	deadline := time.Now().Add(2 * time.Second)
	for {
		messages, err = store.Messages("s1")
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) >= 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(messages) != 4 {
		t.Fatalf("unexpected synthetic turn history: %+v", messages)
	}
	if !messages[2].Synthetic || messages[2].Content != "external note" || messages[2].Role != model.RoleUser {
		t.Fatalf("synthetic text should be a synthetic user message: %+v", messages[2])
	}
	if messages[3].Content != "hi!" {
		t.Fatalf("synthetic turn should get an assistant reply: %+v", messages[3])
	}

	empty, err := http.Post(srv.URL+"/sessions/s1/synthetic", "application/json", strings.NewReader(`{"text":"  "}`))
	if err != nil {
		t.Fatal(err)
	}
	empty.Body.Close()
	if empty.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty synthetic text should 400, got %d", empty.StatusCode)
	}

	missing, err := http.Post(srv.URL+"/sessions/missing/synthetic", "application/json", strings.NewReader(`{"text":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing session should 404, got %d", missing.StatusCode)
	}
}

func TestWaitSession(t *testing.T) {
	p := &gateProvider{started: make(chan struct{}, 1), release: make(chan struct{})}
	store := testStore(t)
	catalog := provider.Static("test", "Test", p, []provider.ModelSpec{{ID: "m", Name: "m"}}, "m")
	srv := httptest.NewServer(New(agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 4}), store).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(`{"text":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not start")
	}

	waitResp, err := http.Post(srv.URL+"/sessions/s1/wait", "application/json", strings.NewReader(`{"timeout_ms":25}`))
	if err != nil {
		t.Fatal(err)
	}
	waitResp.Body.Close()
	if waitResp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("wait on a running session should time out with 408, got %d", waitResp.StatusCode)
	}

	close(p.release)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	waitResp, err = http.Post(srv.URL+"/sessions/s1/wait", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	waitResp.Body.Close()
	if waitResp.StatusCode != http.StatusOK {
		t.Fatalf("wait on an idle session should return 200, got %d", waitResp.StatusCode)
	}

	missing, err := http.Post(srv.URL+"/sessions/missing/wait", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing session should 404, got %d", missing.StatusCode)
	}
}
