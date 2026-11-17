package tool

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/permission"
)

type echoTool struct{}

func (echoTool) Name() string            { return "echo" }
func (echoTool) Description() string     { return "Echo a value" }
func (echoTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (echoTool) Run(_ context.Context, call Call) (string, error) {
	var args struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", err
	}
	return args.Value, nil
}

func TestExecuteRunsNestedTools(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	registry := NewRegistry()
	registry.Add(Codemode(echoTool{}))
	execute := NewExecute(registry, ".")
	metadata := map[string]any{}
	out, err := execute.Run(context.Background(), Call{
		Name:     "execute",
		Input:    json.RawMessage(`{"code":"const value = await tool('echo', { value: 'hello' }); return value.toUpperCase();"}`),
		Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "HELLO" {
		t.Fatalf("unexpected execute output: %q", out)
	}
	calls, ok := metadata["toolCalls"].([]map[string]any)
	if !ok || len(calls) != 1 || calls[0]["tool"] != "echo" || calls[0]["status"] != "completed" {
		t.Fatalf("unexpected nested call metadata: %#v", metadata["toolCalls"])
	}
}

type gateTool struct {
	started chan struct{}
	release chan struct{}
}

func (g gateTool) Name() string            { return "gate" }
func (g gateTool) Description() string     { return "Wait for release" }
func (g gateTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (g gateTool) Run(ctx context.Context, _ Call) (string, error) {
	g.started <- struct{}{}
	select {
	case <-g.release:
		return "ok", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestExecuteRunsNestedCallsConcurrently(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	gate := gateTool{started: make(chan struct{}, 2), release: make(chan struct{})}
	registry := NewRegistry()
	registry.Add(Codemode(gate))
	execute := NewExecute(registry, ".")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := execute.Run(ctx, Call{
			Name:     "execute",
			Input:    json.RawMessage(`{"code":"const [a, b] = await Promise.all([tool('gate', {}), tool('gate', {})]); return a + b"}`),
			Metadata: map[string]any{},
		})
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-gate.started:
		case <-ctx.Done():
			t.Fatal("nested calls did not start concurrently")
		}
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExecuteRejectsDirectOnlyTools(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	registry := NewRegistry()
	registry.Add(Direct(echoTool{}))
	execute := NewExecute(registry, ".")
	_, err := execute.Run(context.Background(), Call{
		Name:     "execute",
		Input:    json.RawMessage(`{"code":"return await tool('echo', { value: 'hello' })"}`),
		Metadata: map[string]any{},
	})
	if err == nil || !strings.Contains(err.Error(), "is a direct tool") {
		t.Fatalf("direct-only tool should be rejected: %v", err)
	}
	if strings.Contains(err.Error(), "at ") {
		t.Fatalf("direct-tool error should not include a stack trace: %v", err)
	}
}

func TestExecuteRejectsUnknownToolsWithoutStack(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	registry := NewRegistry()
	execute := NewExecute(registry, ".")
	_, err := execute.Run(context.Background(), Call{
		Name:     "execute",
		Input:    json.RawMessage(`{"code":"return await tool('missing_tool', {})"}`),
		Metadata: map[string]any{},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown Code Mode tool") {
		t.Fatalf("unknown tool should be rejected: %v", err)
	}
	if strings.Contains(err.Error(), "at ") {
		t.Fatalf("unknown-tool error should not include a stack trace: %v", err)
	}
}

func TestExecuteDescriptionMentionsNoTimers(t *testing.T) {
	description := NewExecute(NewRegistry(), ".").Description()
	for _, want := range []string{"setTimeout", "timers", "console.log", "direct tools"} {
		if !strings.Contains(description, want) {
			t.Fatalf("execute description missing %q: %s", want, description)
		}
	}
}

func TestExecuteCapturesConsoleOutput(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	registry := NewRegistry()
	registry.Add(Codemode(echoTool{}))
	execute := NewExecute(registry, ".")
	out, err := execute.Run(context.Background(), Call{
		Name:     "execute",
		Input:    json.RawMessage(`{"code":"console.log('first'); console.error('bad'); return 'done';"}`),
		Metadata: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "done\n\nLogs:\nfirst\n[error] bad"
	if out != want {
		t.Fatalf("unexpected execute output:\n%q\nwant:\n%q", out, want)
	}
}

func TestExecuteReportsNullForEmptyScript(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	execute := NewExecute(NewRegistry(), ".")
	out, err := execute.Run(context.Background(), Call{
		Name:     "execute",
		Input:    json.RawMessage(`{"code":"const value = 1;"}`),
		Metadata: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "null" {
		t.Fatalf("empty script output = %q, want null", out)
	}
}

func TestExecuteNestedResultBehavesLikeString(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	registry := NewRegistry()
	registry.Add(Codemode(echoTool{}))
	execute := NewExecute(registry, ".")
	code := "const r = await tool('echo', { value: 'hello' }); return [typeof r, r.length, r.content, r.output, r.toUpperCase()].join('|');"
	input, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		t.Fatal(err)
	}
	out, err := execute.Run(context.Background(), Call{
		Name:     "execute",
		Input:    input,
		Metadata: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "object|5|hello|hello|HELLO" {
		t.Fatalf("unexpected nested result: %q", out)
	}
}

type callbackTool struct{}

func (callbackTool) Name() string            { return "callback" }
func (callbackTool) Description() string     { return "Emit an event and an instruction" }
func (callbackTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (callbackTool) Run(_ context.Context, call Call) (string, error) {
	if call.Emit != nil {
		_ = call.Emit(event.New(event.ToolOutput, map[string]any{"output": "chunk"}))
	}
	if call.OnInstruction != nil {
		call.OnInstruction(Instruction{Text: "instructions", Description: "Loaded AGENTS.md"})
	}
	return "ok", nil
}

func TestExecutePassesCallbacksToNestedTools(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	registry := NewRegistry()
	registry.Add(Codemode(callbackTool{}))
	execute := NewExecute(registry, ".")
	var emitted int
	var loaded int
	_, err := execute.Run(context.Background(), Call{
		Name:     "execute",
		Input:    json.RawMessage(`{"code":"return await tool('callback', {});"}`),
		Metadata: map[string]any{},
		Emit: func(event.Event) error {
			emitted++
			return nil
		},
		OnInstruction: func(Instruction) {
			loaded++
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if emitted != 1 || loaded != 1 {
		t.Fatalf("nested callbacks not forwarded: emitted=%d loaded=%d", emitted, loaded)
	}
}

type askTool struct{}

func (askTool) Name() string            { return "ask" }
func (askTool) Description() string     { return "Ask through the permission channel" }
func (askTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (askTool) Run(_ context.Context, call Call) (string, error) {
	if call.OnAsk != nil {
		call.OnAsk(permission.Request{ID: "q1", SessionID: call.SessionID, Action: "question", Resources: []string{"answer"}})
	}
	return "asked", nil
}

func TestExecuteForwardsAskToNestedTools(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	registry := NewRegistry()
	registry.Add(Codemode(askTool{}))
	execute := NewExecute(registry, ".")
	var asked int
	out, err := execute.Run(context.Background(), Call{
		SessionID: "s1",
		Name:      "execute",
		Input:     json.RawMessage(`{"code":"return await tool('ask', {});"}`),
		Metadata:  map[string]any{},
		OnAsk: func(permission.Request) {
			asked++
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "asked" || asked != 1 {
		t.Fatalf("question callback not forwarded: out=%q asked=%d", out, asked)
	}
}
