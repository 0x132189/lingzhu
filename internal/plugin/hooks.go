package plugin

import (
	"context"
	"encoding/json"
	"time"

	"lingzhu/internal/rpc"
)

const hookTimeout = 2 * time.Second

// ToolHookEvent is the payload sent to tool.execute.before and after.
type ToolHookEvent struct {
	SessionID string          `json:"sessionId"`
	Tool      string          `json:"tool"`
	CallID    string          `json:"callId"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Output    string          `json:"output,omitempty"`
	Failed    bool            `json:"failed,omitempty"`
}

// HookDecision is the before hook result. Arguments, when present, replaces the
// tool arguments before execution.
type HookDecision struct {
	Blocked   bool            `json:"blocked"`
	Reason    string          `json:"reason,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func (c *Client) BeforeTool(ctx context.Context, event ToolHookEvent) (HookDecision, error) {
	callCtx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	var decision HookDecision
	err := c.request(callCtx, "hooks/tool.execute.before", event, &decision)
	if err != nil && rpc.IsMethodNotFound(err) {
		return HookDecision{}, nil
	}
	return decision, err
}

// Fire sends a hook notification. Notifications have no reply, so the call only
// waits for the write, never for plugin work.
func (c *Client) Fire(method string, event any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": event})
}
