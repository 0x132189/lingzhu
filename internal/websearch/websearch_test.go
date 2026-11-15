package websearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/permission"
	"lingzhu/internal/tool"
)

func TestBraveSearch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subscription-Token") != "token" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("q") != "golang" || r.URL.Query().Get("count") != "2" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"web": map[string]any{"results": []map[string]any{
				{"title": "Go", "url": "https://go.dev", "description": "The Go language", "age": "1 day"},
				{"title": "Docs", "url": "https://go.dev/doc", "description": "Documentation"},
			}},
		})
	}))
	defer server.Close()

	manager := New(Config{Provider: "brave", Endpoint: server.URL, APIKey: "token", MaxResults: 2})
	if !manager.Configured() {
		t.Fatal("manager should be configured")
	}
	results, err := manager.Search(context.Background(), "golang")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Title != "Go" || results[0].URL != "https://go.dev" || results[0].Published != "1 day" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestExaAndGeneric(t *testing.T) {
	exa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("x-api-key") != "key" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["query"] != "news" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{"title": "News", "url": "https://example.com/news", "text": "latest", "publishedDate": "2026-01-01"},
		}})
	}))
	defer exa.Close()
	manager := New(Config{Provider: "exa", Endpoint: exa.URL, APIKey: "key"})
	results, err := manager.Search(context.Background(), "news")
	if err != nil || len(results) != 1 || results[0].Content != "latest" {
		t.Fatalf("unexpected exa results: %+v %v", results, err)
	}

	generic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{"title": "One", "url": "https://example.com/1", "content": "body"},
		}})
	}))
	defer generic.Close()
	manager = New(Config{Provider: "generic", Endpoint: generic.URL})
	if !manager.Configured() {
		t.Fatal("generic endpoint should configure without a key")
	}
	results, err = manager.Search(context.Background(), "anything")
	if err != nil || len(results) != 1 || results[0].Title != "One" {
		t.Fatalf("unexpected generic results: %+v %v", results, err)
	}
}

func TestMissingKeyIsUnconfigured(t *testing.T) {
	manager := New(Config{Provider: "brave"})
	if manager.Configured() {
		t.Fatal("brave without a key should be unconfigured")
	}
	if _, err := manager.Search(context.Background(), "x"); err == nil {
		t.Fatal("unconfigured search should fail")
	}
}

func TestSearchTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{"title": "Result", "url": "https://example.com", "content": "Snippet"},
		}})
	}))
	defer server.Close()
	manager := New(Config{Provider: "generic", Endpoint: server.URL})
	item := Tool(manager)
	out, err := item.Run(context.Background(), tool.Call{
		Name:  "websearch",
		Input: json.RawMessage(`{"query":"hello"}`),
		Perm:  permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[Result](https://example.com)") || !strings.Contains(out, "Snippet") {
		t.Fatalf("unexpected tool output: %q", out)
	}
}

func TestStatusHidesKey(t *testing.T) {
	manager := New(Config{Provider: "brave", APIKey: "secret", MaxResults: 50})
	status := manager.Status()
	if status.Provider != "brave" || !status.Configured || status.MaxResults != maxResultsLimit {
		t.Fatalf("unexpected status: %+v", status)
	}
	data, _ := json.Marshal(status)
	if strings.Contains(string(data), "secret") {
		t.Fatal("status leaked the API key")
	}
}
