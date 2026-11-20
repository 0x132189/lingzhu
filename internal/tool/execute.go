package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Execute runs JavaScript in Code Mode. The script calls the other tools through
// tools.name(args); the host performs each call and returns its text result.
type Execute struct {
	Registry *Registry
	Dir      string
	// AllTools lets the CodemodeOnly switch reach every tool through execute,
	// because no direct tool remains.
	AllTools bool
	// FullCodemode enables the experimental path and Bun file globals. It is off
	// by default so execute cannot replace the direct file tools.
	FullCodemode bool
}

func NewExecute(registry *Registry, dir string) *Execute {
	return &Execute{Registry: registry, Dir: dir}
}

func (e *Execute) Name() string { return "execute" }

// AcceptsRawInput lets the registry pass freeform program text through without
// requiring the object schema used by providers that do not support freeform.
func (e *Execute) AcceptsRawInput() bool { return true }

func (e *Execute) Description() string {
	if e.FullCodemode {
		return "Run JavaScript in a confined Code Mode runtime to script tool calls and HTTP requests and compose their results. Call other tools through `tools.name(args)` only when they are listed in the Code Mode catalog; `shell`, `read`, `write`, `edit` and `patch` are Code Mode tools there, while `glob`, `grep` and `webfetch` are direct tools. `fetch` is available for HTTP requests, and Bun's file APIs (`Bun.file`, `Bun.write`, `Bun.Glob`) for files, in place of Node's `fs` (`Bun.Glob.scan` is async; `scanSync` is unavailable). Imports and timers are unavailable: there is no `setTimeout` or sleep. The same permission rules as the direct tools apply. Inside `execute`, only the tools listed in the Code Mode catalog or returned by `search` are callable; ignore tools shown outside the catalog. Use exact paths, await every call whose completion matters, and prefer an explicit return over a bare final expression. Log with `console.log` to see intermediate values."
	}
	return "Run JavaScript in a confined Code Mode runtime to script tool calls and HTTP requests and compose their results. Call other tools through `tools.name(args)` only when they are listed in the Code Mode catalog; shell, read, write, edit, glob, grep, patch and webfetch are direct tools and must be called as top-level tool calls. `fetch` is available for HTTP requests. Imports, direct filesystem access, and timers are unavailable: there is no `setTimeout` or sleep. The same permission rules as the direct tools apply. Inside `execute`, only the tools listed in the Code Mode catalog or returned by `search` are callable; ignore tools shown outside the catalog. Use exact paths, await every call whose completion matters, and prefer an explicit return over a bare final expression. Log with `console.log` to see intermediate values."
}

func (e *Execute) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"code": {"type": "string", "description": "JavaScript source. The runtime also accepts a raw source string."},
			"timeout": {"type": "integer", "description": "Maximum runtime in milliseconds (default: 120000)."}
		},
		"required": ["code"]
	}`)
}

// decodeExecuteInput accepts both the object schema providers use and the raw
// source string a freeform tool call carries.
func decodeExecuteInput(raw json.RawMessage) (string, *int, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	if raw[0] == '{' {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err == nil {
			if _, ok := probe["code"]; ok {
				var args struct {
					Code    string `json:"code"`
					Timeout *int   `json:"timeout"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", nil, err
				}
				return args.Code, args.Timeout, nil
			}
			if _, ok := probe["timeout"]; ok {
				var args struct {
					Code    string `json:"code"`
					Timeout *int   `json:"timeout"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", nil, err
				}
				return args.Code, args.Timeout, nil
			}
		}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil, nil
	}
	return string(raw), nil, nil
}

func (e *Execute) Run(ctx context.Context, call Call) (string, error) {
	code, timeoutArg, err := decodeExecuteInput(call.Input)
	if err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}
	code = strings.TrimSpace(code)
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
	if timeoutArg != nil {
		if *timeoutArg < 0 {
			return "", errors.New("execute: timeout must not be negative")
		}
		timeout = time.Duration(*timeoutArg) * time.Millisecond
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
	toolNames, err := json.Marshal(e.toolNames(call))
	if err != nil {
		return "", fmt.Errorf("execute: %w", err)
	}
	script := strings.Replace(executePrelude, "__CODE__", string(codeJSON), 1)
	script = strings.Replace(script, "__TOOLS__", string(toolNames), 1)
	full := "false"
	if e.FullCodemode {
		full = "true"
	}
	script = strings.Replace(script, "__FULL_CODEMODE__", full, 1)
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
	var progressMu sync.Mutex
	var lastProgress time.Time
	var pending sync.WaitGroup
	publishCalls := func(force bool) {
		if call.OnProgress == nil {
			return
		}
		progressMu.Lock()
		if !force && time.Since(lastProgress) < 100*time.Millisecond {
			progressMu.Unlock()
			return
		}
		lastProgress = time.Now()
		progressMu.Unlock()
		mu.Lock()
		snapshot := make([]map[string]any, len(calls))
		for i, item := range calls {
			copied := make(map[string]any, len(item))
			for key, value := range item {
				copied[key] = value
			}
			snapshot[i] = copied
		}
		mu.Unlock()
		call.OnProgress(map[string]any{"toolCalls": snapshot})
	}
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
			publishCalls(false)
			// Nested calls run concurrently. The script may use Promise.all;
			// the host must not serialize the tool work behind its scanner.
			pending.Add(1)
			go func(msg executeMessage, item map[string]any) {
				defer pending.Done()
				output, err := e.runNested(ctx, call, strconv.Itoa(msg.ID), msg.Name, msg.Args)
				mu.Lock()
				if err != nil {
					item["status"] = "error"
					item["error"] = err.Error()
				} else {
					item["status"] = "completed"
				}
				mu.Unlock()
				publishCalls(false)
				if err != nil {
					write(map[string]any{"type": "result", "id": msg.ID, "error": err.Error(), "tool": true})
					return
				}
				write(map[string]any{"type": "result", "id": msg.ID, "output": output})
			}(message, entry)
		case "host":
			name, input := hostDisplay(message.Op, message.Args)
			entry := map[string]any{
				"tool":   name,
				"status": "running",
				"input":  input,
			}
			mu.Lock()
			calls = append(calls, entry)
			mu.Unlock()
			publishCalls(false)
			pending.Add(1)
			go func(msg executeMessage, item map[string]any) {
				defer pending.Done()
				output, err := e.runHost(ctx, call, msg.Op, msg.Args)
				mu.Lock()
				if err != nil {
					item["status"] = "error"
					item["error"] = err.Error()
				} else {
					item["status"] = "completed"
				}
				mu.Unlock()
				publishCalls(false)
				if err != nil {
					write(map[string]any{"type": "result", "id": msg.ID, "error": err.Error()})
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

// toolNames lists the tools this execution exposes on the `tools` object. The
// list is materialized in the sandbox so Object.keys(tools) matches the catalog.
func (e *Execute) toolNames(call Call) []string {
	if e.Registry == nil {
		return nil
	}
	defs := e.Registry.CodemodeDefinitions()
	if e.AllTools {
		defs = e.Registry.AllDefinitions()
	}
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		if def.Name != "execute" && call.Allows(def.Name) {
			names = append(names, def.Name)
		}
	}
	sort.Strings(names)
	return names
}

// hostDisplay names one file or network host call for the execution's call
// list, using the same labels the Code Mode UI shows.
func hostDisplay(op string, raw json.RawMessage) (string, map[string]any) {
	var args struct {
		Path    string `json:"path"`
		URL     string `json:"url"`
		Method  string `json:"method"`
		Pattern string `json:"pattern"`
		Cwd     string `json:"cwd"`
	}
	_ = json.Unmarshal(raw, &args)
	switch op {
	case "webfetch":
		method := strings.ToUpper(strings.TrimSpace(args.Method))
		if method == "" {
			method = "GET"
		}
		input := map[string]any{"method": method}
		if args.URL != "" {
			input["url"] = args.URL
		}
		return "fetch", input
	case "writeFile":
		return "Bun.write", map[string]any{"path": args.Path}
	case "glob":
		input := map[string]any{}
		if args.Pattern != "" {
			input["pattern"] = args.Pattern
		}
		if args.Cwd != "" && args.Cwd != "." {
			input["cwd"] = args.Cwd
		}
		return "Bun.Glob", input
	default:
		return "Bun.file", map[string]any{"path": args.Path}
	}
}

// runHost backs the Code Mode file and network globals. Every operation asks
// the same permission as the matching direct tool before touching the host.
func (e *Execute) runHost(ctx context.Context, call Call, op string, raw json.RawMessage) (string, error) {
	if !e.FullCodemode && op != "webfetch" {
		return "", errors.New("direct filesystem access is unavailable in Code Mode; enable full_codemode to use Bun and path")
	}
	switch op {
	case "webfetch":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("fetch: %w", err)
		}
		target := strings.TrimSpace(args.URL)
		parsed, err := url.Parse(target)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return "", errors.New("fetch: URL must start with http:// or https://")
		}
		if parsed.User != nil {
			return "", errors.New("fetch: credentials in the URL are not supported")
		}
		if err := call.Allow(ctx, "webfetch", []string{target}, ""); err != nil {
			return "", err
		}
		return `{"ok":true}`, nil
	case "readFile":
		var args struct {
			Path     string `json:"path"`
			Encoding string `json:"encoding"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("Bun.file: %w", err)
		}
		path, err := call.Resolve(ctx, "read", e.Dir, args.Path)
		if err != nil {
			return "", err
		}
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
		if err != nil {
			return "", err
		}
		if len(data) > maxFileBytes {
			return "", fmt.Errorf("Bun.file: file is larger than %d bytes", maxFileBytes)
		}
		if args.Encoding == "base64" {
			return encodeHostValue(map[string]any{"content": base64.StdEncoding.EncodeToString(data), "size": len(data)})
		}
		return encodeHostValue(map[string]any{"content": string(data), "size": len(data)})
	case "statFile":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("Bun.file: %w", err)
		}
		path, err := call.Resolve(ctx, "read", e.Dir, args.Path)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return `{"exists":false}`, nil
			}
			return "", err
		}
		return encodeHostValue(map[string]any{
			"exists":    true,
			"size":      info.Size(),
			"directory": info.IsDir(),
			"modified":  info.ModTime().UnixMilli(),
		})
	case "writeFile":
		var args struct {
			Path       string `json:"path"`
			Content    string `json:"content"`
			Encoding   string `json:"encoding"`
			CreatePath bool   `json:"createPath"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("Bun.write: %w", err)
		}
		path, err := call.Resolve(ctx, "edit", e.Dir, args.Path)
		if err != nil {
			return "", err
		}
		data := []byte(args.Content)
		if args.Encoding == "base64" {
			data, err = base64.StdEncoding.DecodeString(args.Content)
			if err != nil {
				return "", fmt.Errorf("Bun.write: %w", err)
			}
		}
		if args.CreatePath {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return "", err
			}
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", err
		}
		return encodeHostValue(map[string]any{"written": len(data)})
	case "glob":
		var args struct {
			Pattern   string `json:"pattern"`
			Cwd       string `json:"cwd"`
			Dot       bool   `json:"dot"`
			OnlyFiles bool   `json:"onlyFiles"`
			Absolute  bool   `json:"absolute"`
			Limit     int    `json:"limit"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("Bun.Glob: %w", err)
		}
		pattern := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(args.Pattern)), "./")
		if pattern == "" {
			return "", errors.New("Bun.Glob: pattern is empty")
		}
		if args.Cwd == "" {
			args.Cwd = "."
		}
		root, err := call.Resolve(ctx, "glob", e.Dir, args.Cwd)
		if err != nil {
			return "", err
		}
		if args.Limit <= 0 {
			args.Limit = 1000
		}
		if args.Limit > 5000 {
			args.Limit = 5000
		}
		var hits []string
		err = walkGlob(root, args.Dot, func(path, rel string, info os.FileInfo) error {
			if !strings.Contains(pattern, "/") && strings.Contains(rel, "/") {
				return nil
			}
			if args.OnlyFiles && info.IsDir() {
				return nil
			}
			if matchPattern(pattern, rel) {
				if args.Absolute {
					hits = append(hits, filepath.ToSlash(path))
				} else {
					hits = append(hits, filepath.ToSlash(rel))
				}
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		sort.Strings(hits)
		if len(hits) > args.Limit {
			hits = hits[:args.Limit]
		}
		return encodeHostValue(hits)
	default:
		return "", fmt.Errorf("unknown Code Mode host operation %q", op)
	}
}

func encodeHostValue(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// walkGlob visits files and directories for Bun.Glob. Unlike the search walker,
// it does not skip common build directories, and it reports directories so
// onlyFiles:false can list them.
func walkGlob(root string, hidden bool, visit func(path, rel string, info os.FileInfo) error) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		if !hidden && strings.HasPrefix(info.Name(), ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		return visit(path, filepath.ToSlash(rel), info)
	})
}

// withLogs appends captured console output to the script result, matching the
// "Logs:" block the reference implementation returns.
func withLogs(result string, logs []string) string {
	if len(logs) == 0 {
		return result
	}
	if result == "null" {
		result = ""
	}
	var b strings.Builder
	b.WriteString(result)
	if result != "" {
		b.WriteString("\n\n")
	}
	b.WriteString("Logs:\n")
	b.WriteString(strings.Join(logs, "\n"))
	return b.String()
}

func (e *Execute) runNested(ctx context.Context, call Call, id, name string, raw json.RawMessage) (string, error) {
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
		ID:            call.ID + ":" + id,
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
		Rules:         call.Rules,
		ToolFilter:    call.ToolFilter,
		Unattended:    call.Unattended,
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
		var query string
		if err := json.Unmarshal(raw, &query); err == nil {
			input.Query = query
		} else if err := json.Unmarshal(raw, &input); err != nil {
			return "", errors.New("search: expected a query string or an object with query, namespace, limit and offset")
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
	query := strings.TrimPrefix(input.Query, "tools.")
	namespace := strings.TrimPrefix(input.Namespace, "tools.")
	if namespace == "tools" {
		namespace = ""
	}
	items, remaining, next := searchEntries(entries, query, namespace, input.Limit, input.Offset)
	if items == nil {
		items = []CatalogEntry{}
	}
	for index := range items {
		items[index].Path = "tools." + items[index].Path
	}
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
	Op     string          `json:"op"`
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
const nodePath = require("path");
const fullCodemode = __FULL_CODEMODE__;
const toolNames = __TOOLS__;
const nativeFetch = globalThis.fetch;
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
  waiters.set(id, { resolve, reject, host: false });
  send({ type: "call", id, name, args: args ?? {} });
});
const host = (op, args) => new Promise((resolve, reject) => {
  const id = nextID++;
  waiters.set(id, { resolve, reject, host: true });
  send({ type: "host", id, op, args: args ?? {} });
});
const guardedFetch = async (input, init) => {
  const raw = typeof input === "string" ? input : input && typeof input === "object" && "url" in input ? String(input.url) : "";
  if (!raw) throw new TypeError("fetch: URL is required");
  const parsed = new URL(raw);
  if (parsed.protocol === "file:") throw new Error("fetch cannot be used for filesystem access.");
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") throw new Error("fetch only supports http and https URLs");
  if (parsed.username || parsed.password) throw new TypeError("fetch cannot be used with credentials in the URL");
  if (typeof nativeFetch !== "function") throw new Error("fetch is unavailable in this Node.js runtime");
  const timeout = AbortSignal.timeout(30000);
  const signal = init && init.signal ? AbortSignal.any([init.signal, timeout]) : timeout;
  let target = parsed.href.split("#")[0];
  let request = { ...(init || {}) };
  let method = String(request.method || "GET").toUpperCase();
  await host("webfetch", { url: target, method });
  for (let redirects = 0; ; redirects++) {
    const response = await nativeFetch(target, { ...request, redirect: "manual", signal });
    const location = response.headers.get("location");
    if (![301, 302, 303, 307, 308].includes(response.status) || location === null) return response;
    if (response.body) await response.body.cancel();
    if (redirects >= 20) throw new Error("fetch: too many redirects (more than 20)");
    const next = new URL(location, target);
    if (next.protocol !== "http:" && next.protocol !== "https:") throw new Error("fetch cannot follow a redirect to " + next.protocol + " URLs");
    if (next.username || next.password) throw new TypeError("fetch cannot follow a redirect with credentials in the URL");
    next.hash = "";
    const toGet = response.status === 303 ? method !== "HEAD" : (response.status === 301 || response.status === 302) && method === "POST";
    const headers = new Headers(request.headers);
    if (toGet) ["content-type", "content-length", "content-encoding", "content-language", "content-location"].forEach((name) => headers.delete(name));
    if (new URL(target).origin !== next.origin) ["authorization", "cookie", "proxy-authorization"].forEach((name) => headers.delete(name));
    request = { ...request, method: toGet ? "GET" : request.method, headers: Array.from(headers), body: toGet ? undefined : request.body };
    method = toGet ? "GET" : method;
    target = next.href;
    await host("webfetch", { url: target, method });
  }
};
const bunFile = Symbol("Bun.file");
const hostJSON = async (op, args) => JSON.parse(await host(op, args));
const readFileBytes = async (path) => {
  const result = await hostJSON("readFile", { path: String(path), encoding: "base64" });
  return new Uint8Array(Buffer.from(result.content, "base64"));
};
const makeBunFile = (path, ranges = []) => {
  const name = String(path);
  const read = async () => {
    let bytes = await readFileBytes(name);
    for (const range of ranges) {
      const start = range.start < 0 ? Math.max(bytes.length + range.start, 0) : Math.min(range.start, bytes.length);
      const end = range.end === undefined ? bytes.length : range.end < 0 ? Math.max(bytes.length + range.end, 0) : Math.min(range.end, bytes.length);
      bytes = bytes.slice(start, Math.max(start, end));
    }
    return bytes;
  };
  const file = {
    [bunFile]: true,
    name,
    path: name,
    async text() {
      return Buffer.from(await read()).toString("utf8");
    },
    async json() {
      return JSON.parse(Buffer.from(await read()).toString("utf8"));
    },
    async bytes() {
      return read();
    },
    async arrayBuffer() {
      return read();
    },
    async exists() {
      return Boolean((await hostJSON("statFile", { path: name })).exists);
    },
    async stat() {
      const info = await hostJSON("statFile", { path: name });
      const size = info.exists ? (await read()).length : 0;
      return {
        size,
        mtimeMs: info.modified ?? 0,
        mtime: new Date(info.modified ?? 0),
        isFile: () => info.exists && !info.directory,
        isDirectory: () => Boolean(info.exists && info.directory),
        isSymbolicLink: () => false,
      };
    },
    slice(start = 0, end, type) {
      return makeBunFile(name, [...ranges, { start: typeof start === "number" ? start : 0, end: typeof end === "number" ? end : undefined }]);
    },
    async write(data) {
      return bun.write(name, data);
    },
  };
  return file;
};
const bun = {
  file: (path) => makeBunFile(path),
  write: async (path, data, options = {}) => {
    const target = path && path[bunFile] ? path.name : String(path);
    let content;
    let encoding = "utf8";
    if (typeof data === "string") {
      content = data;
    } else if (data && data[bunFile]) {
      content = Buffer.from(await data.bytes()).toString("base64");
      encoding = "base64";
    } else if (ArrayBuffer.isView(data)) {
      content = Buffer.from(data.buffer, data.byteOffset, data.byteLength).toString("base64");
      encoding = "base64";
    } else if (Object.prototype.toString.call(data) === "[object ArrayBuffer]") {
      content = Buffer.from(data).toString("base64");
      encoding = "base64";
    } else if (data && typeof data.arrayBuffer === "function") {
      content = Buffer.from(await data.arrayBuffer()).toString("base64");
      encoding = "base64";
    } else {
      throw new TypeError("Bun.write in Code Mode accepts a string, Uint8Array, BunFile or Response");
    }
    return (await hostJSON("writeFile", {
      path: target,
      content,
      encoding,
      createPath: options.createPath !== false,
    })).written;
  },
  Glob: class Glob {
    constructor(pattern) {
      if (typeof pattern !== "string" || !pattern) throw new TypeError("Bun.Glob expects a pattern string");
      this.pattern = pattern;
    }
    scan(input = {}) {
      const options = typeof input === "string" ? { cwd: input } : input;
      const cwd = options.cwd === undefined ? "." : String(options.cwd);
      const request = host("glob", {
        pattern: this.pattern,
        cwd,
        dot: options.dot === true,
        onlyFiles: options.onlyFiles === true,
        absolute: options.absolute === true,
        limit: options.limit,
      }).then((value) => JSON.parse(value));
      // Bun's scan returns an array: await resolves it and for await consumes
      // it. The permission check is asynchronous, so keep both interfaces.
      return {
        then: (resolve, reject) => request.then(resolve, reject),
        catch: (reject) => request.catch(reject),
        finally: (callback) => request.finally(callback),
        async *[Symbol.asyncIterator]() {
          yield* await request;
        },
      };
    }
    scanSync() {
      throw new Error("Bun.Glob.scanSync is unavailable in Code Mode; use await scan()");
    }
  },
};
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
  else waiter.resolve(waiter.host ? message.output : decorate(message.output));
});
(async () => {
  try {
    const globals = { __bridge: bridge, __host: host, console, fetch: guardedFetch, toolNames };
    if (fullCodemode) {
      globals.path = nodePath;
      globals.Bun = bun;
    }
    const context = vm.createContext(globals, { codeGeneration: { strings: false, wasm: false } });
    vm.runInContext(` + "`" + `
      const bridge = globalThis.__bridge;
      const host = globalThis.__host;
      delete globalThis.__bridge;
      delete globalThis.__host;
      const call = (name, args) => new Promise((resolve, reject) => bridge(name, args).then(resolve, reject));
      const tools = {};
      for (const name of toolNames) tools[name] = (args) => call(String(name), args);
      globalThis.tool = call;
      globalThis.tools = tools;
      globalThis.search = async (input) => JSON.parse(String(await call("__search", input ?? {})));
    ` + "`" + `, context);
    const value = await vm.runInContext("(async () => {\n" + code + "\n})()", context);
    let output;
    if (value === undefined) output = "null";
    else if (typeof value === "string" || isStringObject(value)) output = String(value);
    else {
      try { output = JSON.stringify(value) ?? String(value); } catch { output = String(value); }
    }
    send({ type: "done", output, logs });
  } catch (error) {
    const detail = String(error && error.message || error);
    send({ type: "done", error: detail, logs });
  } finally {
    input.close();
  }
})();
`
