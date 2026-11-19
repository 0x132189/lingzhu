package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"lingzhu/internal/rpc"
)

func TestToolInputRequiredFlow(t *testing.T) {
	round := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "no stream", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var message rpc.Message
		if err := json.Unmarshal(body, &message); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		reply := func(result any) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(rpcReply(message.ID, result))
		}
		switch message.Method {
		case "initialize":
			reply(map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "input"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			round++
			if round == 1 {
				reply(map[string]any{
					"resultType": "input_required",
					"inputRequests": map[string]any{
						"ask-name": map[string]any{
							"method": "elicitation/create",
							"params": map[string]any{
								"message": "Name?",
								"requestedSchema": map[string]any{
									"type":       "object",
									"properties": map[string]any{"name": map[string]any{"type": "string"}},
								},
							},
						},
					},
				})
				return
			}
			var params struct {
				InputResponses map[string]any `json:"inputResponses"`
			}
			_ = json.Unmarshal(message.Params, &params)
			if params.InputResponses["ask-name"] == nil {
				http.Error(w, "missing input response", http.StatusBadRequest)
				return
			}
			reply(map[string]any{"content": []map[string]any{{"type": "text", "text": "accepted"}}})
		default:
			reply(map[string]any{})
		}
	}))
	defer server.Close()

	client, err := Connect(context.Background(), "input", Server{
		Type:    "remote",
		URL:     server.URL,
		Timeout: Timeout{Startup: 5000, Catalog: 5000, Execution: 5000},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetElicitation(func(_ context.Context, params json.RawMessage) (any, error) {
		var request struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(params, &request); err != nil || request.Message != "Name?" {
			t.Fatalf("unexpected elicitation: %s", params)
		}
		return map[string]any{"action": "accept", "content": map[string]any{"name": "Ada"}}, nil
	})
	result, err := client.CallTool(context.Background(), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "accepted" {
		t.Fatalf("unexpected tool result: %+v", result)
	}
}
