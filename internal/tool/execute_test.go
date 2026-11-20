package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

func TestExecuteSearchAcceptsString(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	registry := NewRegistry()
	registry.Add(Codemode(echoTool{}))
	execute := NewExecute(registry, ".")
	out, err := execute.Run(context.Background(), Call{
		Name:     "execute",
		Input:    json.RawMessage(`{"code":"const a = await search('echo'); const b = await search('tools.echo'); return a.items[0].path + '|' + b.items[0].path;"}`),
		Metadata: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "tools.echo|tools.echo" {
		t.Fatalf("search path = %q, want tools.echo|tools.echo", out)
	}
}

func TestExecuteSearchReturnsObject(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	registry := NewRegistry()
	registry.Add(Codemode(echoTool{}))
	execute := NewExecute(registry, ".")
	code := "const r = await search({ query: 'echo' }); return JSON.stringify({ array: Array.isArray(r.items), path: r.items[0].path, sig: typeof r.items[0].signature });"
	out, err := execute.Run(context.Background(), Call{
		Name:     "execute",
		Input:    json.RawMessage(`{"code":"` + code + `"}`),
		Metadata: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"array":true,"path":"tools.echo","sig":"string"}` {
		t.Fatalf("search result = %q", out)
	}
}

func TestExecuteSearchEmptyItems(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	registry := NewRegistry()
	registry.Add(Codemode(echoTool{}))
	execute := NewExecute(registry, ".")
	code := "const r = await search('no-such-tool'); return JSON.stringify({ array: Array.isArray(r.items), len: r.items.length, next: r.next });"
	out, err := execute.Run(context.Background(), Call{
		Name:     "execute",
		Input:    json.RawMessage(`{"code":"` + code + `"}`),
		Metadata: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"array":true,"len":0,"next":null}` {
		t.Fatalf("empty search result = %q", out)
	}
}

func TestExecuteFetchRejectsFileURL(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	execute := NewExecute(NewRegistry(), ".")
	code := "try { await fetch('file:///C:/'); return 'no-error'; } catch (e) { return String(e.message || e); }"
	out, err := execute.Run(context.Background(), Call{
		Name:     "execute",
		Input:    json.RawMessage(`{"code":"` + code + `"}`),
		Metadata: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "fetch cannot be used for filesystem access." {
		t.Fatalf("file fetch error = %q", out)
	}
}

func TestExecuteFileGlobals(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "c.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	execute := NewExecute(NewRegistry(), dir)
	execute.FullCodemode = true
	metadata := map[string]any{}
	out, err := execute.Run(context.Background(), Call{
		Name:     "execute",
		Input:    json.RawMessage(`{"code":"const glob = new Bun.Glob(\"*.txt\"); const files = []; for await (const file of glob.scan({ onlyFiles: true })) files.push(file); const text = await Bun.file(files[0]).text(); const sliced = await Bun.file(files[0]).slice(1, 4).text(); const bytes = await Bun.file(files[0]).bytes(); const stat = await Bun.file(files[0]).stat(); const written = await Bun.write(\"b.txt\", \"beta\"); const binaryWritten = await Bun.write(\"new/bytes.bin\", new Uint8Array([1,2,3])); const copied = await Bun.write(\"copy.txt\", Bun.file(files[0])); const copiedText = await Bun.file(\"copy.txt\").text(); const binary = await Bun.file(\"new/bytes.bin\").bytes(); return JSON.stringify({ files, text, sliced, written, copied, copiedText, statSize: stat.size, bytes: Array.from(bytes), binary: Array.from(binary), binaryWritten });"}`),
		Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Files         []string `json:"files"`
		Text          string   `json:"text"`
		Sliced        string   `json:"sliced"`
		Written       int      `json:"written"`
		Copied        int      `json:"copied"`
		CopiedText    string   `json:"copiedText"`
		StatSize      int64    `json:"statSize"`
		Bytes         []int    `json:"bytes"`
		Binary        []int    `json:"binary"`
		BinaryWritten int      `json:"binaryWritten"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unexpected output %q: %v", out, err)
	}
	if len(result.Files) != 1 || result.Files[0] != "a.txt" || result.Text != "alpha" || result.Sliced != "lph" || result.Written != 4 ||
		result.Copied != 5 || result.CopiedText != "alpha" || result.StatSize != 5 ||
		len(result.Bytes) != 5 || result.BinaryWritten != 3 || len(result.Binary) != 3 || result.Binary[2] != 3 {
		t.Fatalf("unexpected file globals result: %+v", result)
	}
	calls, ok := metadata["toolCalls"].([]map[string]any)
	if !ok {
		t.Fatalf("missing host call metadata: %#v", metadata["toolCalls"])
	}
	var names []string
	for _, item := range calls {
		names = append(names, fmt.Sprint(item["tool"]))
	}
	if !strings.Contains(strings.Join(names, ","), "Bun.Glob") || !strings.Contains(strings.Join(names, ","), "Bun.file") || !strings.Contains(strings.Join(names, ","), "Bun.write") {
		t.Fatalf("unexpected host calls: %v", names)
	}
}

func TestExecuteGlobListsDirectories(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	execute := NewExecute(NewRegistry(), dir)
	execute.FullCodemode = true
	out, err := execute.Run(context.Background(), Call{
		Name:  "execute",
		Input: json.RawMessage(`{"code":"const names = []; for await (const p of new Bun.Glob(\"*\").scan({ onlyFiles: false })) names.push(p); return JSON.stringify(names.sort());"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != `["a.txt","nested"]` {
		t.Fatalf("unexpected direct entries: %s", out)
	}
}

func TestExecuteFetch(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fixture"))
	}))
	defer server.Close()
	execute := NewExecute(NewRegistry(), t.TempDir())
	out, err := execute.Run(context.Background(), Call{
		Name:  "execute",
		Input: json.RawMessage(`{"code":"const response = await fetch(` + "`" + server.URL + "`" + `); return await response.text();"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "fixture" {
		t.Fatalf("unexpected fetch output: %q", out)
	}
}

func TestExecuteFetchFollowsRedirects(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("redirected"))
	}))
	defer server.Close()
	execute := NewExecute(NewRegistry(), t.TempDir())
	metadata := map[string]any{}
	out, err := execute.Run(context.Background(), Call{
		Name:     "execute",
		Input:    json.RawMessage(`{"code":"const response = await fetch(` + "`" + server.URL + `/start` + "`" + `); return await response.text();"}`),
		Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "redirected" {
		t.Fatalf("unexpected redirect output: %q", out)
	}
	calls, ok := metadata["toolCalls"].([]map[string]any)
	if !ok || len(calls) < 2 {
		t.Fatalf("redirect hops should be recorded: %#v", metadata["toolCalls"])
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

func TestExecuteStreamsRunningCalls(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	gate := gateTool{started: make(chan struct{}, 1), release: make(chan struct{})}
	registry := NewRegistry()
	registry.Add(Codemode(gate))
	execute := NewExecute(registry, ".")
	progress := make(chan map[string]any, 16)
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		_, err := execute.Run(ctx, Call{
			Name:  "execute",
			Input: json.RawMessage(`{"code":"await tool('gate', {})"}`),
			OnProgress: func(update map[string]any) {
				select {
				case progress <- update:
				default:
				}
			},
		})
		done <- err
	}()
	select {
	case <-gate.started:
	case <-ctx.Done():
		t.Fatal("nested call did not start")
	}
	var sawRunning bool
	for !sawRunning {
		select {
		case update := <-progress:
			calls, _ := update["toolCalls"].([]map[string]any)
			for _, call := range calls {
				if call["tool"] == "gate" && call["status"] == "running" {
					sawRunning = true
				}
			}
		case <-ctx.Done():
			t.Fatal("no running progress update")
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

func TestExecuteFullCodemodeCallsBuiltins(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	registry := NewRegistry()
	registry.Add(catalogTool{name: "shell", description: "Shell.", schema: `{"type":"object"}`})
	registry.Add(catalogTool{name: "glob", description: "Glob.", schema: `{"type":"object"}`})
	registry.SetFullCodemode(true)
	execute := NewExecute(registry, ".")
	execute.FullCodemode = true
	if _, err := execute.Run(context.Background(), Call{
		Name:  "execute",
		Input: json.RawMessage(`{"code":"return await tool('shell', {})"}`),
	}); err != nil {
		t.Fatalf("full Code Mode should call shell: %v", err)
	}
	_, err := execute.Run(context.Background(), Call{
		Name:  "execute",
		Input: json.RawMessage(`{"code":"return await tool('glob', {})"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "direct tool") {
		t.Fatalf("glob should stay direct-only: %v", err)
	}
}

func TestExecuteAcceptsRawInput(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	registry := NewRegistry()
	execute := NewExecute(registry, ".")
	registry.Add(execute)
	out, err := registry.Run(context.Background(), Call{
		Name:  "execute",
		Input: json.RawMessage(`"return 'raw'"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "raw" {
		t.Fatalf("unexpected raw input output: %q", out)
	}
	out, err = registry.Run(context.Background(), Call{
		Name:  "execute",
		Input: json.RawMessage(`return 'unquoted'`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "unquoted" {
		t.Fatalf("unexpected unquoted input output: %q", out)
	}
}

func TestExecuteToolsAreEnumerable(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	registry := NewRegistry()
	registry.Add(Codemode(echoTool{}))
	execute := NewExecute(registry, ".")
	out, err := execute.Run(context.Background(), Call{
		Name:  "execute",
		Input: json.RawMessage(`{"code":"return JSON.stringify(Object.keys(tools))"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != `["echo"]` {
		t.Fatalf("unexpected tools keys: %s", out)
	}
}

func TestExecuteFileGlobalsDisabledByDefault(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	execute := NewExecute(NewRegistry(), t.TempDir())
	out, err := execute.Run(context.Background(), Call{
		Name:  "execute",
		Input: json.RawMessage(`{"code":"return JSON.stringify({ bun: typeof Bun, path: typeof path })"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"bun":"undefined","path":"undefined"}` {
		t.Fatalf("file globals should be hidden by default: %s", out)
	}
}

func TestExecuteDescriptionMentionsNoTimers(t *testing.T) {
	description := NewExecute(NewRegistry(), ".").Description()
	for _, want := range []string{"setTimeout", "timers", "console.log", "direct tools", "direct filesystem access"} {
		if !strings.Contains(description, want) {
			t.Fatalf("execute description missing %q: %s", want, description)
		}
	}
	if strings.Contains(description, "Bun.file") {
		t.Fatalf("default execute description should not advertise Bun: %s", description)
	}

	full := NewExecute(NewRegistry(), ".")
	full.FullCodemode = true
	if text := full.Description(); !strings.Contains(text, "Bun.file") || !strings.Contains(text, "Bun.Glob") {
		t.Fatalf("full execute description missing Bun APIs: %s", text)
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

func TestExecuteOnlyLogsOmitsNull(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	execute := NewExecute(NewRegistry(), ".")
	out, err := execute.Run(context.Background(), Call{
		Name:  "execute",
		Input: json.RawMessage(`{"code":"console.log('hello')"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "Logs:\nhello" {
		t.Fatalf("unexpected log-only output: %q", out)
	}
}

func TestExecuteErrorsDropStack(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	execute := NewExecute(NewRegistry(), ".")
	_, err := execute.Run(context.Background(), Call{
		Name:  "execute",
		Input: json.RawMessage(`{"code":"throw new Error('boom')"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the script error: %v", err)
	}
	if strings.Contains(err.Error(), "\n    at ") {
		t.Fatalf("script error should not include a stack trace: %v", err)
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
