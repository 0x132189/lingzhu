package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func testManagerForClient(client *Client, config Server) *Manager {
	return &Manager{
		states:  map[string]*serverState{"demo": {config: config}},
		clients: map[string]*Client{"demo": client},
	}
}

func TestSamplingRequiresOptIn(t *testing.T) {
	client := &Client{name: "demo"}
	manager := testManagerForClient(client, Server{})
	if _, err := manager.sample(client, json.RawMessage(`{"messages":[]}`)); err == nil ||
		!strings.Contains(err.Error(), "disabled") {
		t.Fatalf("want disabled sampling error, got %v", err)
	}

	manager.states["demo"].config.Sampling = true
	if _, err := manager.sample(client, json.RawMessage(`{"messages":[]}`)); err == nil ||
		!strings.Contains(err.Error(), "not configured") {
		t.Fatalf("want unconfigured sampling error, got %v", err)
	}

	var seen SamplingRequest
	manager.SetSampling(func(_ context.Context, server string, request SamplingRequest) (SamplingResult, error) {
		if server != "demo" {
			t.Fatalf("unexpected server: %q", server)
		}
		seen = request
		return SamplingResult{Role: "assistant", Content: map[string]any{"type": "text", "text": "ok"}}, nil
	})
	result, err := manager.sample(client, json.RawMessage(`{
		"systemPrompt": "be brief",
		"maxTokens": 32,
		"messages": [{"role": "user", "content": {"type": "text", "text": "hello"}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if seen.SystemPrompt != "be brief" || seen.MaxTokens != 32 || len(seen.Messages) != 1 {
		t.Fatalf("unexpected sampling request: %+v", seen)
	}
	payload, ok := result.(SamplingResult)
	if !ok || payload.Content["text"] != "ok" {
		t.Fatalf("unexpected sampling result: %#v", result)
	}
}

func TestElicitationNeedsAnOwningSession(t *testing.T) {
	client := &Client{name: "demo"}
	manager := testManagerForClient(client, Server{})
	params := json.RawMessage(`{"message":"Name?","requestedSchema":{"type":"object","properties":{"name":{"type":"string"}}}}`)

	result, err := manager.elicit(client, params)
	if err != nil {
		t.Fatal(err)
	}
	if action, _ := result.(map[string]any)["action"].(string); action != "decline" {
		t.Fatalf("disabled elicitation should decline: %#v", result)
	}

	manager.states["demo"].config.Elicitation = true
	result, err = manager.elicit(client, params)
	if err != nil {
		t.Fatal(err)
	}
	if action, _ := result.(map[string]any)["action"].(string); action != "decline" {
		t.Fatalf("elicitation without a session should decline: %#v", result)
	}

	client.sessions = []string{"ses_1"}
	called := 0
	manager.SetElicitation(func(_ context.Context, server, session string, got json.RawMessage) (any, error) {
		called++
		if server != "demo" || session != "ses_1" {
			t.Fatalf("unexpected elicitation target: %q %q", server, session)
		}
		if string(got) != string(params) {
			t.Fatalf("unexpected params: %s", got)
		}
		return map[string]any{"action": "accept", "content": map[string]any{"name": "Ada"}}, nil
	})
	result, err = manager.elicit(client, params)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := result.(map[string]any)
	if called != 1 || payload["action"] != "accept" {
		t.Fatalf("unexpected elicitation result: %#v", result)
	}
	content, _ := payload["content"].(map[string]any)
	if content["name"] != "Ada" {
		t.Fatalf("unexpected elicited content: %#v", payload)
	}
}
