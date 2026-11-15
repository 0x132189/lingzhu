package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/commands"
	"lingzhu/internal/model"
)

func TestCustomCommandsAppearAndRun(t *testing.T) {
	srv, store := newSessionServer(t, func(s *Server) {
		s.SetCommands(commands.New(nil, map[string]commands.Definition{
			"review": {Description: "Review code", Template: "Review $ARGUMENTS."},
		}))
	})
	seedSession(t, store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/commands")
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Commands []struct {
			Name   string `json:"name"`
			Plugin string `json:"plugin"`
		} `json:"commands"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(listed.Commands) != 1 || listed.Commands[0].Name != "review" || listed.Commands[0].Plugin != "custom" {
		t.Fatalf("unexpected commands: %+v", listed.Commands)
	}

	run, err := http.Post(httpSrv.URL+"/sessions/s1/command", "application/json", strings.NewReader(`{"name":"review","arguments":"src/main.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(run.Body)
	run.Body.Close()
	if run.StatusCode != http.StatusNoContent {
		t.Fatalf("unexpected run status: %d %s", run.StatusCode, payload)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		messages, err := store.Messages("s1")
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range messages {
			if message.Role == model.RoleUser && message.Content == "Review src/main.go." {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("custom command prompt missing from history: %+v", messages)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPromptExpandsCustomCommand(t *testing.T) {
	srv, store := newSessionServer(t, func(s *Server) {
		s.SetCommands(commands.New(nil, map[string]commands.Definition{
			"review": {Template: "Review $ARGUMENTS."},
		}))
	})
	seedSession(t, store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	resp, err := http.Post(httpSrv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(`{"text":"/review src/main.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected prompt status: %d", resp.StatusCode)
	}
	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Role == model.RoleUser && message.Content == "Review src/main.go." {
			return
		}
	}
	t.Fatalf("expanded command missing from history: %+v", messages)
}
