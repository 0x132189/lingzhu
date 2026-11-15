package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lingzhu/internal/config"
	"lingzhu/internal/event"
	"lingzhu/internal/mcp"
	"lingzhu/internal/plugin"
	"lingzhu/internal/policy"
	"lingzhu/internal/provider"
	"lingzhu/internal/share"
)

func TestBuildCatalogWithNoProviders(t *testing.T) {
	cfg := config.Config{ProvidersSet: true, Providers: map[string]config.Provider{}}
	catalog, err := buildCatalog(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resolve(provider.Selection{}); err == nil {
		t.Fatal("want an error when no provider is configured")
	}
}

func TestRecoverResumesClaimAndQueuedInput(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer model.Close()

	application, err := New(config.Config{
		DataDir:  t.TempDir(),
		Workdir:  t.TempDir(),
		Model:    "test-model",
		BaseURL:  model.URL,
		APIKey:   "test-key",
		MaxSteps: 2,
		Shell:    []string{"bash", "-lc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Store.Close() })
	if _, err := application.Store.Append("s1", event.TurnStartedEvent("turn_1")); err != nil {
		t.Fatal(err)
	}
	if _, err := application.Store.QueueInput("s1", "inp_1", "queued while away"); err != nil {
		t.Fatal(err)
	}
	if err := application.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		recoveries, err := application.Store.Recoveries()
		if err != nil {
			t.Fatal(err)
		}
		if len(recoveries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovery did not finish: %+v", recoveries)
		}
		time.Sleep(10 * time.Millisecond)
	}

	messages, err := application.Store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Content != "queued while away" || messages[1].Content != "recovered" {
		t.Fatalf("unexpected recovered history: %+v", messages)
	}
}

func TestShutdownInterruptsRunningTurn(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	// The handler accepts the model request and stalls until the test releases
	// it; the client still cancels its side when the run is interrupted.
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
	}))
	defer func() {
		close(release)
		model.Close()
	}()

	application, err := New(config.Config{
		DataDir:  t.TempDir(),
		Workdir:  t.TempDir(),
		Model:    "test-model",
		BaseURL:  model.URL,
		APIKey:   "test-key",
		MaxSteps: 2,
		Shell:    []string{"bash", "-lc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Store.Close() })

	events, err := application.Agent.Run(context.Background(), "s1", "hi", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not reach the provider")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := application.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	var last event.Event
	for ev := range events {
		last = ev
	}
	if last.Type != event.RunEnd || last.Data["reason"] != "interrupted" {
		t.Fatalf("want interrupted end, got %+v", last)
	}
	if application.Agent.Running("s1") {
		t.Fatal("turn should be stopped after shutdown")
	}
}

func TestAutoShareAfterTurn(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"auto shared answer\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer model.Close()

	application, err := New(config.Config{
		DataDir:  t.TempDir(),
		Workdir:  t.TempDir(),
		Model:    "test-model",
		BaseURL:  model.URL,
		APIKey:   "test-key",
		MaxSteps: 2,
		Shell:    []string{"bash", "-lc"},
		Share:    shareConfigForTest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = application.Shutdown(context.Background())
		_ = application.Store.Close()
	})
	stream, err := application.Agent.Run(context.Background(), "s1", "hello", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		record, ok, err := application.Shares.BySession("s1")
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if record.Messages != 2 {
				t.Fatalf("unexpected auto share: %+v", record)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("auto share was not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManualShareDoesNotAutoCreate(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"manual\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer model.Close()

	application, err := New(config.Config{
		DataDir:  t.TempDir(),
		Workdir:  t.TempDir(),
		Model:    "test-model",
		BaseURL:  model.URL,
		APIKey:   "test-key",
		MaxSteps: 2,
		Shell:    []string{"bash", "-lc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = application.Shutdown(context.Background())
		_ = application.Store.Close()
	})
	stream, err := application.Agent.Run(context.Background(), "s1", "hello", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	time.Sleep(100 * time.Millisecond)
	if _, ok, _ := application.Shares.BySession("s1"); ok {
		t.Fatal("manual mode must not create a share automatically")
	}
}

func shareConfigForTest() share.Config {
	return share.Config{Mode: "auto"}
}

func TestPolicyFiltersMCPAndPlugins(t *testing.T) {
	rules := policy.Normalize([]policy.Statement{
		{Action: "mcp.use", Resource: "blocked", Effect: policy.Deny},
		{Action: "plugin.use", Resource: "blocked", Effect: policy.Deny},
	})
	mcpConfig := filterMCPConfig(mcp.Config{Servers: map[string]mcp.Server{
		"keep": {}, "blocked": {},
	}}, rules, t.Logf)
	if _, ok := mcpConfig.Servers["blocked"]; ok || len(mcpConfig.Servers) != 1 {
		t.Fatalf("unexpected mcp config: %+v", mcpConfig.Servers)
	}
	pluginConfig := filterPluginConfig(plugin.Config{Plugins: map[string]plugin.Server{
		"keep": {}, "blocked": {},
	}}, rules, t.Logf)
	if _, ok := pluginConfig.Plugins["blocked"]; ok || len(pluginConfig.Plugins) != 1 {
		t.Fatalf("unexpected plugin config: %+v", pluginConfig.Plugins)
	}
}
