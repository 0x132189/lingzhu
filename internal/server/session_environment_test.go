package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/agent"
	"lingzhu/internal/config"
	"lingzhu/internal/provider"
	"lingzhu/internal/tool"
)

type environmentPayload struct {
	ID       string   `json:"id"`
	Workdir  string   `json:"workdir"`
	Shell    []string `json:"shell"`
	Running  bool     `json:"running"`
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Agent    string   `json:"agent"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
}

func TestSessionEnvironment(t *testing.T) {
	workdir := t.TempDir()
	srv, store := newSessionServer(t, func(s *Server) {
		s.SetConfig(config.Config{Shell: config.DefaultShell(), Workdir: workdir})
	})
	seedSession(t, store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/sessions/s1/environment")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for environment, got %d", resp.StatusCode)
	}
	var env environmentPayload
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.ID != "s1" || env.Workdir != workdir || len(env.Shell) == 0 {
		t.Fatalf("unexpected environment: %+v", env)
	}
	if env.Running {
		t.Fatalf("idle session should not be running: %+v", env)
	}
	if env.Provider != "test" || env.Model != "m" {
		t.Fatalf("unexpected model environment: %+v", env)
	}
	if env.OS != runtime.GOOS || env.Arch != runtime.GOARCH {
		t.Fatalf("unexpected platform environment: %+v", env)
	}

	missing, err := http.Get(httpSrv.URL + "/sessions/missing/environment")
	if err != nil {
		t.Fatal(err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing session should 404, got %d", missing.StatusCode)
	}
}

func TestSessionEnvironmentReportsRunning(t *testing.T) {
	p := &gateProvider{started: make(chan struct{}, 1), release: make(chan struct{})}
	store := testStore(t)
	catalog := provider.Static("test", "Test", p, []provider.ModelSpec{{ID: "m", Name: "m"}}, "m")
	srv := httptest.NewServer(New(agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 4}), store).Handler())
	defer srv.Close()
	seedSession(t, store, "s1")

	resp, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(`{"text":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not start")
	}

	envResp, err := http.Get(srv.URL + "/sessions/s1/environment")
	if err != nil {
		t.Fatal(err)
	}
	var env environmentPayload
	if err := json.NewDecoder(envResp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	envResp.Body.Close()
	if !env.Running {
		t.Fatalf("running session should report running: %+v", env)
	}

	close(p.release)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
