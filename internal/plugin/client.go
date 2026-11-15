package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lingzhu/internal/rpc"
	"lingzhu/internal/version"
)

const protocolVersion = "1"

var errClosed = errors.New("plugin: process closed")

// Capabilities is what a plugin advertises during initialize.
type Capabilities struct {
	Tools       bool `json:"tools"`
	Commands    bool `json:"commands"`
	Permissions bool `json:"permissions"`
	Hooks       bool `json:"hooks"`
	Panels      bool `json:"panels"`
}

// ToolInfo is one tool advertised by a plugin.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// CommandInfo is one slash command advertised by a plugin.
type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Template    string `json:"template"`
}

// PanelInfo is one HTML panel advertised by a plugin.
type PanelInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	HTML  string `json:"html"`
}

// CallResult is the response to tools/call.
type CallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Output  string `json:"output"`
	IsError bool   `json:"isError"`
}

func (r *CallResult) Text() string {
	if strings.TrimSpace(r.Output) != "" {
		return r.Output
	}
	parts := make([]string, 0, len(r.Content))
	for _, item := range r.Content {
		if item.Text != "" {
			parts = append(parts, item.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type Client struct {
	name string
	cfg  Server
	cmd  *exec.Cmd

	stdin   io.WriteCloser
	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[int64]chan rpc.Response
	seq     int64

	caps      Capabilities
	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once
	waitOnce  sync.Once
	stderr    *tailBuffer
	timeout   Timeout
}

func connect(ctx context.Context, name string, cfg Server, workdir string) (*Client, error) {
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("plugin: %q has no command", name)
	}
	cwd := workdir
	if cfg.Cwd != "" {
		if filepath.IsAbs(cfg.Cwd) {
			cwd = cfg.Cwd
		} else {
			cwd = filepath.Join(workdir, cfg.Cwd)
		}
	}
	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Dir = cwd
	env := os.Environ()
	for key, value := range cfg.Environment {
		env = append(env, key+"="+value)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("plugin: %w", err)
	}
	stderr := &tailBuffer{limit: 4096}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("plugin: start %q: %w", name, err)
	}

	c := &Client{
		name:    name,
		cfg:     cfg,
		cmd:     cmd,
		stdin:   stdin,
		pending: map[int64]chan rpc.Response{},
		done:    make(chan struct{}),
		stderr:  stderr,
		timeout: cfg.timeouts(),
	}
	go c.readLoop(stdout)

	startup, cancel := context.WithTimeout(ctx, millis(c.timeout.Startup))
	defer cancel()
	caps, err := c.initialize(startup, workdir)
	if err != nil {
		c.Close()
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("plugin: initialize %q: %w: %s", name, err, detail)
		}
		return nil, fmt.Errorf("plugin: initialize %q: %w", name, err)
	}
	c.caps = caps
	return c, nil
}

func (c *Client) initialize(ctx context.Context, workdir string) (Capabilities, error) {
	var result struct {
		Name         string       `json:"name"`
		Version      string       `json:"version"`
		Capabilities Capabilities `json:"capabilities"`
	}
	err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"workdir":         workdir,
		"client": map[string]any{
			"name":    "lingzhu",
			"version": version.Version,
		},
	}, &result)
	if err != nil {
		return Capabilities{}, err
	}
	return result.Capabilities, nil
}

// Name returns the configured plugin name.
func (c *Client) Name() string { return c.name }

// Capabilities reports what the plugin advertised during initialize.
func (c *Client) Capabilities() Capabilities { return c.caps }

// Tools discovers every tool the plugin exposes.
func (c *Client) Tools(ctx context.Context) ([]ToolInfo, error) {
	var result struct {
		Tools []ToolInfo `json:"tools"`
	}
	err := c.request(ctx, "tools/list", map[string]any{}, &result)
	if err != nil {
		if rpc.IsMethodNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return result.Tools, nil
}

// Commands discovers every slash command the plugin exposes.
func (c *Client) Commands(ctx context.Context) ([]CommandInfo, error) {
	var result struct {
		Commands []CommandInfo `json:"commands"`
	}
	err := c.request(ctx, "commands/list", map[string]any{}, &result)
	if err != nil {
		if rpc.IsMethodNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return result.Commands, nil
}

// Panels discovers HTML panels the plugin contributes to the app.
func (c *Client) Panels(ctx context.Context) ([]PanelInfo, error) {
	var result struct {
		Panels []PanelInfo `json:"panels"`
	}
	err := c.request(ctx, "panels/list", map[string]any{}, &result)
	if err != nil {
		if rpc.IsMethodNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return result.Panels, nil
}

// CallTool invokes one tool with raw JSON arguments.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallResult, error) {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	var result CallResult
	callCtx, cancel := context.WithTimeout(ctx, millis(c.timeout.Execution))
	defer cancel()
	if err := c.request(callCtx, "tools/call", map[string]any{"name": name, "arguments": args}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Evaluate asks the plugin for a permission effect. A missing method, timeout,
// or malformed answer means the plugin defers.
func (c *Client) Evaluate(ctx context.Context, action string, resources []string) (string, string) {
	callCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var result struct {
		Effect  string `json:"effect"`
		Message string `json:"message"`
	}
	params := map[string]any{
		"action":    action,
		"resources": resources,
	}
	err := c.request(callCtx, "hooks/permission.evaluate", params, &result)
	if err != nil && rpc.IsMethodNotFound(err) {
		err = c.request(callCtx, "permissions/evaluate", params, &result)
	}
	if err != nil {
		return "", ""
	}
	effect := strings.ToLower(strings.TrimSpace(result.Effect))
	switch effect {
	case "allow", "ask", "deny":
		return effect, strings.TrimSpace(result.Message)
	}
	return "", ""
}

func (c *Client) request(ctx context.Context, method string, params any, out any) error {
	id := atomic.AddInt64(&c.seq, 1)
	ch := make(chan rpc.Response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	message := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		message["params"] = params
	}
	if err := c.write(message); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errClosed
	case response := <-ch:
		if response.Err != nil {
			return response.Err
		}
		if out == nil || len(response.Result) == 0 {
			return nil
		}
		return json.Unmarshal(response.Result, out)
	}
}

func (c *Client) write(message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(data); err != nil {
		return fmt.Errorf("plugin: write: %w", err)
	}
	return nil
}

func (c *Client) readLoop(stdout io.Reader) {
	defer c.finish()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var message rpc.Message
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			continue
		}
		if message.Method != "" {
			if len(message.ID) > 0 {
				_ = c.write(map[string]any{
					"jsonrpc": "2.0",
					"id":      message.ID,
					"error":   rpc.Error{Code: -32601, Message: "method not supported"},
				})
			}
			continue
		}
		if len(message.ID) == 0 {
			continue
		}
		id, err := rpc.ParseID(message.ID)
		if err != nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[id]
		c.mu.Unlock()
		if ch == nil {
			continue
		}
		response := rpc.Response{Result: message.Result}
		if message.Error != nil {
			response.Err = message.Error
		}
		select {
		case ch <- response:
		default:
		}
	}
}

func (c *Client) finish() {
	c.doneOnce.Do(func() { close(c.done) })
	c.mu.Lock()
	pending := c.pending
	c.pending = map[int64]chan rpc.Response{}
	c.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- rpc.Response{Err: errClosed}:
		default:
		}
	}
	_ = c.stdin.Close()
	c.wait()
}

func (c *Client) wait() {
	c.waitOnce.Do(func() { _ = c.cmd.Wait() })
}

// Close stops the plugin process.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		killProcess(c.cmd)
		c.doneOnce.Do(func() { close(c.done) })
		go c.wait()
	})
	return nil
}

func killProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		if err := exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run(); err == nil {
			return
		}
	}
	_ = cmd.Process.Kill()
}

// tailBuffer keeps the last bytes written to stderr for diagnostics.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if b.limit > 0 && len(b.data) > b.limit {
		b.data = b.data[len(b.data)-b.limit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
