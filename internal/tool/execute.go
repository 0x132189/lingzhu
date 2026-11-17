package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Execute runs JavaScript in Code Mode. The script can call the other tools
// through tool(name, args) or tools.name(args); the host performs each call and
// returns its text result to the script.
type Execute struct {
	Registry *Registry
	Dir      string
	// AllTools lets the CodemodeOnly switch reach every tool through execute,
	// because no direct tool remains.
	AllTools bool
}

func NewExecute(registry *Registry, dir string) *Execute {
	return &Execute{Registry: registry, Dir: dir}
}

func (e *Execute) Name() string { return "execute" }

func (e *Execute) Description() string {
	return "Run JavaScript in Code Mode. Call other tools with tool(name, args) or tools.name(args). Only the Code Mode tools listed in the system prompt can be called here; shell, read, write, edit, glob, grep, patch and webfetch are direct tools and must be called as top-level tool calls. Return a string or JSON value, or log with console.log. The script has no filesystem, network, imports or timers: there is no setTimeout or sleep, and only the registered tools are available."
}

func (e *Execute) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"code": {"type": "string", "description": "JavaScript to run. Use tool(name, args) or tools.name(args) to call other tools."},
			"timeout": {"type": "integer", "description": "Maximum runtime in milliseconds (default: 120000)."}
		},
		"required": ["code"]
	}`)
}

func (e *Execute) Run(ctx context.Context, call Call) (string, error) {
	var args struct {
		Code    string `json:"code"`
		Timeout *int   `json:"timeout"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}
	code := strings.TrimSpace(args.Code)
	if code == "" {
		return "", errors.New("execute: code is empty")
	}
	if err := call.Allow(ctx, "execute", []string{"code"}, ""); err != nil {
		return "", err
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return "", errors.New("execute: Node.js is required for Code Mode")
	}

	timeout := 120 * time.Second
	if args.Timeout != nil {
		if *args.Timeout < 0 {
			return "", errors.New("execute: timeout must not be negative")
		}
		timeout = time.Duration(*args.Timeout) * time.Millisecond
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	codeJSON, err := json.Marshal(code)
	if err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}
	script := strings.Replace(executePrelude, "__CODE__", string(codeJSON), 1)
	file, err := os.CreateTemp("", "lingzhu-execute-*.js")
	if err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err := file.WriteString(script); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("execute: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}

	cmd := exec.CommandContext(ctx, node, path)
	cmd.Dir = e.Dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}
	defer func() {
		_ = stdin.Close()
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var calls []map[string]any
	var result string
	var logs []string
	var scriptErr error
	var mu sync.Mutex
	var writeMu sync.Mutex
	var pending sync.WaitGroup
	write := func(value any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = writeExecuteMessage(stdin, value)
	}

	for scanner.Scan() {
		var message executeMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		switch message.Type {
		case "call":
			entry := map[string]any{
				"tool":   message.Name,
				"status": "running",
				"input":  decodeObject(message.Args),
			}
			mu.Lock()
			calls = append(calls, entry)
			mu.Unlock()
			// Nested calls run concurrently. The script may use Promise.all;
			// the host must not serialize the tool work behind its scanner.
			pending.Add(1)
			go func(msg executeMessage, item map[string]any) {
				defer pending.Done()
				output, err := e.runNested(ctx, call, msg.Name, msg.Args)
				mu.Lock()
				if err != nil {
					item["status"] = "error"
					item["error"] = err.Error()
				} else {
					item["status"] = "completed"
				}
				mu.Unlock()
				if err != nil {
					write(map[string]any{"type": "result", "id": msg.ID, "error": err.Error(), "tool": true})
					return
				}
				write(map[string]any{"type": "result", "id": msg.ID, "output": output})
			}(message, entry)
		case "done":
			result = message.Output
			logs = message.Logs
			if message.Error != "" {
				scriptErr = errors.New(message.Error)
			}
		}
	}
	pending.Wait()

	// Console output is part of the answer: scripts routinely gather results
	// with console.log instead of returning them, exactly as they do in the
	// reference implementation.
	output := withLogs(result, logs)
	waitErr := cmd.Wait()
	if scriptErr != nil {
		call.SetMetadata("code", code)
		call.SetMetadata("toolCalls", calls)
		call.SetMetadata("error", true)
		call.SetMetadata("errorMessage", scriptErr.Error())
		return truncate(output), scriptErr
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		call.SetMetadata("code", code)
		call.SetMetadata("toolCalls", calls)
		call.SetMetadata("error", true)
		call.SetMetadata("errorMessage", "Execute timed out")
		return truncate(output), fmt.Errorf("execute: timeout after %s", timeout)
	}
	if waitErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = waitErr.Error()
		}
		call.SetMetadata("code", code)
		call.SetMetadata("toolCalls", calls)
		call.SetMetadata("error", true)
		call.SetMetadata("errorMessage", message)
		return truncate(output), fmt.Errorf("execute: %s", message)
	}
	call.SetMetadata("code", code)
	call.SetMetadata("toolCalls", calls)
	call.SetMetadata("output", truncate(output))
	return truncate(output), nil
}

// withLogs appends captured console output to the script result, matching the
// "Logs:" block the reference implementation returns.
func withLogs(result string, logs []string) string {
	if len(logs) == 0 {
		return result
	}
	var b strings.Builder
	b.WriteString(result)
	b.WriteString("\n\nLogs:\n")
	b.WriteString(strings.Join(logs, "\n"))
	return b.String()
}

func (e *Execute) runNested(ctx context.Context, call Call, name string, raw json.RawMessage) (string, error) {
	if name == "execute" {
		return "", errors.New("execute cannot call itself")
	}
	if name == "__search" {
		return e.searchCatalog(raw, call)
	}
	if e.Registry == nil {
		return "", errors.New("execute: tool registry is unavailable")
	}
	if !call.Allows(name) {
		return "", fmt.Errorf("execute: tool %q is disabled for this agent", name)
	}
	data := raw
	if len(data) == 0 || string(data) == "null" {
		data = json.RawMessage(`{}`)
	}
	if name == "execute" {
		return "", errors.New("execute cannot call itself")
	}
	t, ok := e.Registry.CodemodeGet(name)
	if e.AllTools {
		t, ok = e.Registry.Get(name)
	}
	if !ok {
		if !e.AllTools {
			if _, direct := e.Registry.Get(name); direct {
				return "", fmt.Errorf("%s is a direct tool; call it as a top-level tool call, not inside execute", name)
			}
		}
		return "", fmt.Errorf("unknown Code Mode tool %q; use search({ query }) to discover tools", name)
	}
	if err := Validate(name, t.Schema(), data); err != nil {
		return "", err
	}
	metadata := map[string]any{}
	return t.Run(ctx, Call{
		SessionID:     call.SessionID,
		Name:          name,
		Input:         data,
		Perm:          call.Perm,
		CanAttach:     call.CanAttach,
		Metadata:      metadata,
		OnAsk:         call.OnAsk,
		OnDiff:        call.OnDiff,
		OnAttachment:  call.OnAttachment,
		OnInstruction: call.OnInstruction,
		OnFormat:      call.OnFormat,
		Emit:          call.Emit,
	})
}

// searchCatalog backs the `search` helper inside execute. It pages through the
// Code Mode catalog so a script can discover tools the prompt did not list.
func (e *Execute) searchCatalog(raw json.RawMessage, call Call) (string, error) {
	var input struct {
		Query     string `json:"query"`
		Namespace string `json:"namespace"`
		Limit     int    `json:"limit"`
		Offset    int    `json:"offset"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &input); err != nil {
			return "", fmt.Errorf("search: %w", err)
		}
	}
	var entries []CatalogEntry
	if e.AllTools {
		entries = AllCatalog(e.Registry)
	} else {
		entries = Catalog(e.Registry)
	}
	if call.ToolFilter != nil {
		allowed := entries[:0]
		for _, entry := range entries {
			if call.Allows(entry.Path) {
				allowed = append(allowed, entry)
			}
		}
		entries = allowed
	}
	items, remaining, next := searchEntries(entries, input.Query, input.Namespace, input.Limit, input.Offset)
	result := map[string]any{
		"items":     items,
		"remaining": remaining,
		"next":      nil,
	}
	if next >= 0 {
		result["next"] = map[string]any{"offset": next}
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type executeMessage struct {
	Type   string          `json:"type"`
	ID     int             `json:"id"`
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args"`
	Output string          `json:"output"`
	Error  string          `json:"error"`
	Tool   bool            `json:"tool"`
	Logs   []string        `json:"logs"`
}

func decodeObject(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return map[string]any{}
	}
	return value
}

func writeExecuteMessage(writer interface{ Write([]byte) (int, error) }, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = writer.Write(data)
	return err
}

// executePrelude runs user code inside a separate V8 context. The bridge is
// hidden before user code runs, console output is captured and returned with
// the result, and nested tool results keep the fields scripts written for the
// reference implementation rely on while still behaving like strings.
const executePrelude = `const vm = require("vm");
const readline = require("readline");
const code = __CODE__;
const waiters = new Map();
let nextID = 1;
const logLimit = 32768;
const send = (message) => process.stdout.write(JSON.stringify(message) + "\n");
const isStringObject = (value) => Object.prototype.toString.call(value) === "[object String]";
const text = (value) => {
  if (typeof value === "string") return value;
  if (isStringObject(value)) return String(value);
  if (value instanceof Error) return value.stack || value.message;
  try {
    const json = JSON.stringify(value);
    if (json !== undefined) return json;
  } catch {}
  try { return String(value); } catch { return "[unprintable]"; }
};
const decorate = (output) => {
  if (typeof output !== "string") return output;
  const wrapped = new String(output);
  wrapped.output = output;
  wrapped.content = output;
  wrapped.text = output;
  return wrapped;
};
const bridge = (name, args) => new Promise((resolve, reject) => {
  const id = nextID++;
  waiters.set(id, { resolve, reject });
  send({ type: "call", id, name, args: args ?? {} });
});
const logs = [];
let logBytes = 0;
const capture = (level, args) => {
  if (logBytes >= logLimit) return;
  const body = args.map(text).join(" ");
  const line = level === "log" ? body : "[" + level + "] " + body;
  logs.push(line);
  logBytes += line.length + 1;
  if (logBytes >= logLimit) logs.push("(logs truncated)");
};
const console = {
  log: (...args) => capture("log", args),
  info: (...args) => capture("info", args),
  warn: (...args) => capture("warn", args),
  error: (...args) => capture("error", args),
  debug: (...args) => capture("debug", args),
  dir: (...args) => capture("log", args),
  table: (...args) => capture("log", args),
};
const input = readline.createInterface({ input: process.stdin, terminal: false });
input.on("line", (line) => {
  let message;
  try { message = JSON.parse(line); } catch { return; }
  if (message.type !== "result") return;
  const waiter = waiters.get(message.id);
  if (!waiter) return;
  waiters.delete(message.id);
  if (message.error) {
    const failure = new Error(message.error);
    failure.tool = message.tool === true;
    waiter.reject(failure);
  }
  else waiter.resolve(decorate(message.output));
});
(async () => {
  try {
    const context = vm.createContext({ __bridge: bridge, console }, { codeGeneration: { strings: false, wasm: false } });
    vm.runInContext(` + "`" + `
      const bridge = globalThis.__bridge;
      delete globalThis.__bridge;
      const call = (name, args) => new Promise((resolve, reject) => bridge(name, args).then(resolve, reject));
      const tools = new Proxy({}, { get: (_target, name) => (args) => call(String(name), args) });
      globalThis.tool = call;
      globalThis.tools = tools;
      globalThis.search = (input) => call("__search", input ?? {});
    ` + "`" + `, context);
    const value = await vm.runInContext("(async () => {\n" + code + "\n})()", context);
    let output;
    if (value === undefined) output = "null";
    else if (typeof value === "string" || isStringObject(value)) output = String(value);
    else {
      try { output = JSON.stringify(value, null, 2) ?? String(value); } catch { output = String(value); }
    }
    send({ type: "done", output, logs });
  } catch (error) {
    const detail = error && error.tool ? error.message : String(error && error.stack || error);
    send({ type: "done", error: detail, logs });
  } finally {
    input.close();
  }
})();
`
