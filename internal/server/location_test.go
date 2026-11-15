package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/agent"
	"lingzhu/internal/agents"
	"lingzhu/internal/config"
	"lingzhu/internal/event"
	"lingzhu/internal/location"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/skill"
	"lingzhu/internal/tool"
)

// newMultiServer builds a server that serves two workspaces, each with its own
// agent profile and skill catalog, so routing mistakes are observable.
func newMultiServer(t *testing.T, first, second string) (*Server, *session.Store) {
	t.Helper()
	store := testStore(t)
	catalog := provider.Static("test", "Test", staticProvider{}, []provider.ModelSpec{{ID: "m", Name: "m"}}, "m")
	defaultAgent := agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 2})
	srv := New(defaultAgent, store)

	set := location.NewSet()
	for _, dir := range []string{first, second} {
		ref, err := location.Make(dir)
		if err != nil {
			t.Fatal(err)
		}
		services := &location.Services{
			Ref:      ref,
			Agent:    defaultAgent,
			Agents:   agents.New(agents.Config{}, ""),
			Skills:   skill.Discover(skill.Roots(nil, "", dir), nil),
			Commands: nil,
		}
		services.SetConfig(config.Config{Workdir: dir})
		set.Put(services)
	}
	srv.SetLocations(set)
	return srv, store
}

func TestProjectReportsRequestedWorkspace(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	srv, _ := newMultiServer(t, first, second)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/project?directory=" + first)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload struct {
		Directory string `json:"directory"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Directory != first {
		t.Fatalf("want the requested workspace, got %q", payload.Directory)
	}

	// No directory means the default workspace answers, which is what a client
	// written against a single-workspace core expects.
	fallback, err := http.Get(httpSrv.URL + "/project")
	if err != nil {
		t.Fatal(err)
	}
	defer fallback.Body.Close()
	if err := json.NewDecoder(fallback.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Directory != first {
		t.Fatalf("want the default workspace, got %q", payload.Directory)
	}
}

func TestListLocationsReportsEveryWorkspace(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	srv, _ := newMultiServer(t, first, second)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/locations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload struct {
		Locations []struct {
			Directory string `json:"directory"`
			Default   bool   `json:"default"`
			Available bool   `json:"available"`
		} `json:"locations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Locations) != 2 {
		t.Fatalf("want two workspaces, got %+v", payload.Locations)
	}
	if payload.Locations[0].Directory != first || !payload.Locations[0].Default {
		t.Fatalf("first workspace must be the default: %+v", payload.Locations[0])
	}
	if !payload.Locations[1].Available {
		t.Fatalf("an existing directory must be available: %+v", payload.Locations[1])
	}
}

func TestSkillsFollowTheSessionWorkspace(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	for dir, name := range map[string]string{first: "alpha", second: "beta"} {
		if err := os.MkdirAll(filepath.Join(dir, ".lingzhu", "skills", name), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: " + name + " skill\n---\nbody\n"
		if err := os.WriteFile(filepath.Join(dir, ".lingzhu", "skills", name, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv, _ := newMultiServer(t, first, second)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	for dir, want := range map[string]string{first: "alpha", second: "beta"} {
		resp, err := http.Get(httpSrv.URL + "/skills?directory=" + dir)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Skills []struct {
				ID string `json:"id"`
			} `json:"skills"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		if len(payload.Skills) != 1 || payload.Skills[0].ID != want {
			t.Fatalf("workspace %s must only see %s, got %+v", dir, want, payload.Skills)
		}
	}
}

func TestMoveSessionUpdatesPlacementInsideOneCore(t *testing.T) {
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
	body := strings.NewReader(`{"directory":"` + strings.ReplaceAll(second, `\`, `\\`) + `"}`)
	resp, err := http.Post(httpSrv.URL+"/sessions/s1/move", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for a local move, got %d", resp.StatusCode)
	}
	meta, err := store.Meta("s1")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Directory != second {
		t.Fatalf("want the destination directory, got %q", meta.Directory)
	}
	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == event.SessionMoved {
			found = true
		}
	}
	if !found {
		t.Fatal("a local move must record a durable session.moved event")
	}
}

func TestMoveSessionRejectsUnknownDestination(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	srv, store := newMultiServer(t, first, second)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	if _, err := store.Append("s1", event.User("seed")); err != nil {
		t.Fatal(err)
	}
	body := strings.NewReader(`{"directory":"` + strings.ReplaceAll(t.TempDir(), `\`, `\\`) + `"}`)
	resp, err := http.Post(httpSrv.URL+"/sessions/s1/move", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for a workspace that is not open, got %d", resp.StatusCode)
	}
}
