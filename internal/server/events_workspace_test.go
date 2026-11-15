package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/agent"
	"lingzhu/internal/agents"
	"lingzhu/internal/config"
	"lingzhu/internal/event"
	"lingzhu/internal/location"
	"lingzhu/internal/provider"
	"lingzhu/internal/tool"
)

// TestAllEventsIncludesEveryWorkspace proves the all-session stream is not
// limited to the default workspace. Each workspace has its own hub, so a single
// subscription would drop every event from the other projects.
func TestAllEventsIncludesEveryWorkspace(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	store := testStore(t)
	catalog := provider.Static("test", "Test", staticProvider{}, []provider.ModelSpec{{ID: "m", Name: "m"}}, "m")

	defaultAgent := agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 2})
	secondAgent := agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 2})

	srv := New(defaultAgent, store)
	set := location.NewSet()
	for index, dir := range []string{first, second} {
		ref, err := location.Make(dir)
		if err != nil {
			t.Fatal(err)
		}
		services := &location.Services{Ref: ref, Agents: agents.New(agents.Config{}, "")}
		services.SetConfig(config.Config{Workdir: dir})
		if index == 0 {
			services.Agent = defaultAgent
		} else {
			services.Agent = secondAgent
		}
		set.Put(services)
	}
	srv.SetLocations(set)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Give the handler a moment to subscribe before publishing, so the event is
	// not dropped between the request and the subscription.
	time.Sleep(100 * time.Millisecond)
	secondAgent.Publish("s2", event.User("from the second workspace"))

	seen := make(chan string, 4)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "s2") {
				seen <- line
				return
			}
		}
	}()

	select {
	case <-seen:
	case <-time.After(3 * time.Second):
		t.Fatal("the all-session stream must include events from every workspace")
	}
}
