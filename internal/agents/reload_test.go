package agents

import (
	"sync"
	"testing"
)

func TestReloadSwapsProfilesAndDefault(t *testing.T) {
	registry := New(Config{Agents: map[string]Definition{
		"custom": {Name: "Custom", Mode: ModePrimary},
	}}, "custom")
	if got := registry.Default().ID; got != "custom" {
		t.Fatalf("default = %q, want custom", got)
	}
	registry.Reload(Config{Agents: map[string]Definition{
		"next": {Name: "Next", Mode: ModePrimary},
	}}, nil, "next")
	if got := registry.Default().ID; got != "next" {
		t.Fatalf("default after reload = %q, want next", got)
	}
	if _, ok := registry.Get("custom"); ok {
		t.Fatal("removed profile survived reload")
	}
	if profile, ok := registry.Get("next"); !ok || profile.Name != "Next" {
		t.Fatalf("reloaded profile = %+v, %v", profile, ok)
	}
}

func TestReloadIsConcurrentWithReads(t *testing.T) {
	registry := New(Config{Agents: map[string]Definition{
		"one": {Name: "One", Mode: ModePrimary},
	}}, "one")
	var group sync.WaitGroup
	stop := make(chan struct{})
	for index := 0; index < 4; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = registry.Default()
					_ = registry.Primary()
					_, _ = registry.Get("one")
					_ = registry.Resolve("one")
				}
			}
		}()
	}
	for index := 0; index < 50; index++ {
		registry.Reload(Config{Agents: map[string]Definition{
			"two": {Name: "Two", Mode: ModePrimary},
		}}, nil, "two")
		registry.Reload(Config{Agents: map[string]Definition{
			"one": {Name: "One", Mode: ModePrimary},
		}}, nil, "one")
	}
	close(stop)
	group.Wait()
}
