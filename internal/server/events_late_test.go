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

// TestAllEventsSeesWorkspaceOpenedLater is the regression for a stream that
// only subscribed to the workspaces present when the client connected. A client
// opens its project after connecting, so a project added later has to join the
// same stream.
func TestAllEventsSeesWorkspaceOpenedLater(t *testing.T) {
	first := t.TempDir()
	late := t.TempDir()
	store := testStore(t)
	catalog := provider.Static("test", "Test", staticProvider{}, []provider.ModelSpec{{ID: "m", Name: "m"}}, "m")

	defaultAgent := agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 2})
	lateAgent := agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 2})

	srv := New(defaultAgent, store)
	set := location.NewSet()
	firstRef, err := location.Make(first)
	if err != nil {
		t.Fatal(err)
	}
	defaultServices := &location.Services{Ref: firstRef, Agent: defaultAgent, Agents: agents.New(agents.Config{}, "")}
	defaultServices.SetConfig(config.Config{Workdir: first})
	set.Put(defaultServices)
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

	// The stream is running before the second workspace exists, which is the
	// order a real client produces: connect, then open a project.
	time.Sleep(100 * time.Millisecond)
	lateRef, err := location.Make(late)
	if err != nil {
		t.Fatal(err)
	}
	lateServices := &location.Services{Ref: lateRef, Agent: lateAgent, Agents: agents.New(agents.Config{}, "")}
	lateServices.SetConfig(config.Config{Workdir: late})
	set.Put(lateServices)

	// The stream picks the new workspace up on its next tick.
	time.Sleep(900 * time.Millisecond)
	lateAgent.Publish("s-late", event.User("from a workspace opened after connecting"))

	seen := make(chan string, 4)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "s-late") {
				seen <- line
				return
			}
		}
	}()

	select {
	case line := <-seen:
		if !strings.Contains(line, `"directory"`) {
			t.Fatalf("the event must name a workspace, got %s", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a workspace opened after the stream started must still deliver events")
	}
}

// TestAllEventsTagsTheOwningWorkspace proves the frame carries the session's
// directory, so a client can route it without a second request per event.
func TestAllEventsTagsTheOwningWorkspace(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	store := testStore(t)
	catalog := provider.Static("test", "Test", staticProvider{}, []provider.ModelSpec{{ID: "m", Name: "m"}}, "m")
	loop := agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 2})

	srv := New(loop, store)
	set := location.NewSet()
	for _, dir := range []string{first, second} {
		ref, err := location.Make(dir)
		if err != nil {
			t.Fatal(err)
		}
		services := &location.Services{Ref: ref, Agent: loop, Agents: agents.New(agents.Config{}, "")}
		services.SetConfig(config.Config{Workdir: dir})
		set.Put(services)
	}
	srv.SetLocations(set)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	if _, err := store.Append("s1", event.User("seed")); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDirectory("s1", second); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Replay from position 0 so the seed event is re-sent with its directory.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/events?after=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// A live event is simpler to assert than the replay cursor: publish one and
	// check the frame it produces.
	loop.Publish("s1", event.User("live"))

	seen := make(chan string, 4)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "s1") && strings.Contains(line, `"directory"`) {
				seen <- line
				return
			}
		}
	}()

	select {
	case line := <-seen:
		if !strings.Contains(line, strings.ReplaceAll(second, `\`, `\\`)) {
			t.Fatalf("the frame must name the owning workspace, got %s", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a live frame must carry the session directory")
	}
}
