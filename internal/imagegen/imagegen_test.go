package imagegen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/permission"
	"lingzhu/internal/tool"
)

var pngBytes = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x00, 0x00, 0x0d}

func allowAll() *permission.Manager {
	return permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})
}

func TestOpenAIB64(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"b64_json":       base64.StdEncoding.EncodeToString(pngBytes),
				"revised_prompt": "a revised prompt",
			}},
		})
	}))
	defer server.Close()

	manager := New(Config{APIKey: "key", Endpoint: server.URL})
	if !manager.Configured() {
		t.Fatal("manager should be configured")
	}
	result, err := manager.Generate(context.Background(), "a cat", "512x512")
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Data) != string(pngBytes) || result.MediaType != "image/png" || result.Extension != ".png" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.RevisedPrompt != "a revised prompt" {
		t.Fatalf("unexpected revised prompt: %q", result.RevisedPrompt)
	}
	if payload["prompt"] != "a cat" || payload["size"] != "512x512" || payload["model"] != defaultModel {
		t.Fatalf("unexpected request payload: %+v", payload)
	}
}

func TestImageURLDownload(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/image.png" {
			_, _ = w.Write(pngBytes)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"url": server.URL + "/image.png"}},
		})
	}))
	defer server.Close()

	manager := New(Config{Provider: "generic", Endpoint: server.URL})
	if !manager.Configured() {
		t.Fatal("generic provider needs no key")
	}
	result, err := manager.Generate(context.Background(), "logo", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Data) != string(pngBytes) || result.Extension != ".png" {
		t.Fatalf("unexpected download result: %+v", result)
	}
}

func TestUnconfiguredAndErrors(t *testing.T) {
	manager := New(Config{})
	if manager.Configured() {
		t.Fatal("openai without key should be unconfigured")
	}
	if _, err := manager.Generate(context.Background(), "x", ""); err == nil {
		t.Fatal("unconfigured generate should fail")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()
	manager = New(Config{APIKey: "key", Endpoint: server.URL})
	if _, err := manager.Generate(context.Background(), "x", ""); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want HTTP error, got %v", err)
	}
}

func TestStatusHidesKey(t *testing.T) {
	manager := New(Config{APIKey: "secret", Model: "custom", Size: "512x512"})
	status := manager.Status()
	if !status.Configured || status.Model != "custom" || status.Size != "512x512" {
		t.Fatalf("unexpected status: %+v", status)
	}
	data, _ := json.Marshal(status)
	if strings.Contains(string(data), "secret") {
		t.Fatal("status leaked the API key")
	}
}

func TestToolSavesImage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(pngBytes)}},
		})
	}))
	defer server.Close()

	workdir := t.TempDir()
	manager := New(Config{APIKey: "key", Endpoint: server.URL})
	item := Tool(manager, workdir)

	out, err := item.Run(context.Background(), tool.Call{
		Name:  "experimental_image_generate",
		Input: json.RawMessage(`{"prompt":"a cat"}`),
		Perm:  allowAll(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Generated image: images/") || !strings.Contains(out, "image/png") {
		t.Fatalf("unexpected output: %q", out)
	}
	matches, err := filepath.Glob(filepath.Join(workdir, "images", "image-*.png"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("image not saved: %v %v", matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil || string(data) != string(pngBytes) {
		t.Fatalf("unexpected image bytes: %v", err)
	}

	out, err = item.Run(context.Background(), tool.Call{
		Name:  "experimental_image_generate",
		Input: json.RawMessage(`{"prompt":"a dog","path":"out/pic.jpg"}`),
		Perm:  allowAll(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Generated image: out/pic.png") {
		t.Fatalf("custom path not used: %q", out)
	}
	if _, err := os.Stat(filepath.Join(workdir, "out", "pic.png")); err != nil {
		t.Fatalf("custom image missing: %v", err)
	}
}

func TestToolPermissionAndValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("endpoint should not be called without permission")
	}))
	defer server.Close()
	manager := New(Config{APIKey: "key", Endpoint: server.URL})
	item := Tool(manager, t.TempDir())

	deny := permission.New(permission.Rules{{Action: "experimental_image_generate", Resource: "*", Effect: permission.Deny}})
	if _, err := item.Run(context.Background(), tool.Call{
		Name:  "experimental_image_generate",
		Input: json.RawMessage(`{"prompt":"a cat"}`),
		Perm:  deny,
	}); err == nil {
		t.Fatal("denied call should fail")
	}
	if _, err := item.Run(context.Background(), tool.Call{
		Name:  "experimental_image_generate",
		Input: json.RawMessage(`{"prompt":"  "}`),
		Perm:  allowAll(),
	}); err == nil {
		t.Fatal("empty prompt should fail")
	}
}
