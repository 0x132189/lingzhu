package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lingzhu/internal/config"
	"lingzhu/internal/event"
	"lingzhu/internal/provider"
)

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
