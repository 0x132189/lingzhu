package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/config"
)

func TestProviderAndModelEndpoints(t *testing.T) {
	server := newTestServer(t)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/providers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected providers status: %d %s", rec.Code, rec.Body.String())
	}
	var providers struct {
		Providers []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Models []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &providers); err != nil {
		t.Fatal(err)
	}
	if len(providers.Providers) != 1 || providers.Providers[0].ID != "test" || providers.Providers[0].Name != "Test" {
		t.Fatalf("unexpected providers: %s", rec.Body.String())
	}
	if len(providers.Providers[0].Models) != 1 || providers.Providers[0].Models[0].ID != "test-model" {
		t.Fatalf("unexpected provider models: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/providers/test", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "test-model") {
		t.Fatalf("unexpected provider: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/providers/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing provider should 404: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/models", nil))
	var models struct {
		Models []struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerId"`
			Default    bool   `json:"default"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	if len(models.Models) != 1 || models.Models[0].ID != "test-model" || models.Models[0].ProviderID != "test" || !models.Models[0].Default {
		t.Fatalf("unexpected models: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/models/default", nil))
	var defaultModel struct {
		Model *struct {
			ID string `json:"id"`
		} `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &defaultModel); err != nil {
		t.Fatal(err)
	}
	if defaultModel.Model == nil || defaultModel.Model.ID != "test-model" {
		t.Fatalf("unexpected default model: %s", rec.Body.String())
	}
}

func TestConfigReadAndPatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"provider":"old","model":"old-model","max_steps":2,"providers":{"fake":{"base_url":"https://example.com","api_key":"secret-key","models":[{"id":"m"}]}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t)
	server.SetConfig(cfg)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected config status: %d %s", rec.Code, rec.Body.String())
	}
	var view struct {
		Config ConfigView `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Config.Provider != "old" || view.Config.Model != "old-model" || view.Config.MaxSteps != 2 || view.Config.Path != path {
		t.Fatalf("unexpected config view: %+v", view.Config)
	}
	if strings.Contains(rec.Body.String(), "secret-key") {
		t.Fatalf("config view leaked a provider key: %s", rec.Body.String())
	}

	patch := `{"provider":"fake","model":"new-model","max_steps":5,"default_agent":"plan","shell":["bash","-lc"],"permission":[{"action":"shell","resource":"*","effect":"deny"}]}`
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/config", strings.NewReader(patch)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected patch status: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Config.Provider != "fake" || view.Config.Model != "new-model" || view.Config.MaxSteps != 5 || view.Config.DefaultAgent != "plan" {
		t.Fatalf("patch did not update the view: %+v", view.Config)
	}
	if len(view.Config.Permission) != 1 || view.Config.Permission[0].Action != "shell" {
		t.Fatalf("patch did not update permissions: %+v", view.Config.Permission)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["provider"] != "fake" || stored["model"] != "new-model" || stored["max_steps"].(float64) != 5 {
		t.Fatalf("config file not patched: %s", data)
	}
	providers, ok := stored["providers"].(map[string]any)
	if !ok || providers["fake"] == nil {
		t.Fatalf("patch dropped providers: %s", data)
	}
	fake := providers["fake"].(map[string]any)
	if fake["api_key"] != "secret-key" {
		t.Fatalf("patch dropped the provider key: %s", data)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/config", strings.NewReader(`{"max_steps":-1}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative max_steps should 400: %d %s", rec.Code, rec.Body.String())
	}
}

func TestConfigEmptyPathUsesDefault(t *testing.T) {
	server := newTestServer(t)
	server.SetConfig(config.Config{Model: "m"})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model":"m"`) {
		t.Fatalf("unexpected config: %d %s", rec.Code, rec.Body.String())
	}
}
