package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lingzhu/internal/rpc"
	"lingzhu/internal/version"
)

const (
	clientProtocolVersion = "2024-11-05"
	// maxCatalogPages bounds cursor loops so a broken server cannot make a
	// catalog request run forever.
	maxCatalogPages = 100
)

var errClosed = errors.New("mcp: server closed")

// transport delivers newline-delimited or HTTP JSON-RPC messages. Send blocks
// until the server has answered or the context expires; responses are passed to
// the message callback supplied when the transport is created.
type transport interface {
	Send(ctx context.Context, message []byte, expectID int64) error
	Close() error
}

// ToolInfo is one tool advertised by an MCP server.
type ToolInfo struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
}

// Resource is one resource or resource template advertised by a server.
type Resource struct {
	Name        string `json:"name"`
	URI         string `json:"uri,omitempty"`
	URITemplate string `json:"uriTemplate,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ResourceContent is one part returned by resources/read.
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// CallResult is the response to tools/call.
type CallResult struct {
	Content           []ToolContent   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// ToolContent is one content part in a tool result. Resource holds the nested
// object of a "resource" part; ResourceLink fields are kept flat.
type ToolContent struct {
	Type        string           `json:"type"`
	Text        string           `json:"text,omitempty"`
	Data        string           `json:"data,omitempty"`
	MimeType    string           `json:"mimeType,omitempty"`
	URI         string           `json:"uri,omitempty"`
	Name        string           `json:"name,omitempty"`
	Description string           `json:"description,omitempty"`
	Resource    *ResourceContent `json:"resource,omitempty"`
}

// Client is one connected MCP server.
type Client struct {
	name string
	cfg  Server
	tr   transport

	modern   bool
	protocol string
	// instructions is the server's own usage guidance from the handshake. It
	// is injected into the system prompt like the reference harness does.
	instructions string

	// stderr is only set for local servers and is used to explain handshake
	// failures.
	stderr *tailBuffer

	mu      sync.Mutex
	pending map[int64]chan rpc.Response
	seq     int64

	// refreshMu serializes catalog re-listing triggered by server notifications.
	refreshMu sync.Mutex
	// handlerMu protects the manager callbacks, which are set after the handshake.
	handlerMu sync.RWMutex
	onChange  func(kind string)
	onClose   func()

	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once
	closing   atomic.Bool
	timeout   Timeout
}

// Connect starts or dials an MCP server and completes the initialize
// handshake.
func Connect(ctx context.Context, name string, cfg Server, workdir string) (*Client, error) {
	return connect(ctx, name, cfg, workdir, nil)
}

// connect is Connect with an optional OAuth token resolver.
func connect(ctx context.Context, name string, cfg Server, workdir string, tokens TokenFunc) (*Client, error) {
	var token func(context.Context) (string, error)
	if cfg.OAuth.Name != "" && tokens != nil {
		endpoint := cfg.OAuth.Name
		token = func(c context.Context) (string, error) { return tokens(c, endpoint) }
	}
	c := &Client{
		name:    name,
		cfg:     cfg,
		pending: map[int64]chan rpc.Response{},
		done:    make(chan struct{}),
		timeout: cfg.timeouts(),
	}

	var tr transport
	var err error
	switch cfg.Kind() {
	case "local":
		var local *stdioTransport
		local, err = startStdio(name, cfg, workdir, c.receive, c.closed)
		if local != nil {
			c.stderr = local.stderr
		}
		tr = local
	case "remote", "http", "streamable", "streamable-http":
		tr, err = startHTTP(name, cfg, token, c.receive, c.closed)
	case "sse":
		tr, err = startSSE(ctx, name, cfg, token, c.receive, c.closed)
	default:
		return nil, fmt.Errorf("mcp: server %q uses unsupported transport %q", name, cfg.Kind())
	}
	if err != nil {
		return nil, err
	}
	c.tr = tr

	mode := cfg.ProtocolMode()
	initCtx, cancel := context.WithTimeout(ctx, millis(c.timeout.Startup))
	defer cancel()
	if mode != "legacy" {
		negotiated, err := c.discover(initCtx)
		if err != nil {
			if mode == "2026-07-28" {
				_ = tr.Close()
				c.closed()
				if detail := c.detail(); detail != "" {
					return nil, fmt.Errorf("mcp: negotiate %q: %w: %s", name, err, detail)
				}
				return nil, fmt.Errorf("mcp: negotiate %q: %w", name, err)
			}
			// Auto falls back to the legacy initialize handshake.
		}
		if negotiated {
			if starter, ok := tr.(interface{ StartStream(context.Context) }); ok {
				starter.StartStream(context.Background())
			}
			return c, nil
		}
		if mode == "2026-07-28" {
			_ = tr.Close()
			c.closed()
			return nil, fmt.Errorf("mcp: negotiate %q: server does not support %s", name, modernProtocolVersion)
		}
	}
	if err := c.initialize(initCtx); err != nil {
		_ = tr.Close()
		c.closed()
		if detail := c.detail(); detail != "" {
			return nil, fmt.Errorf("mcp: initialize %q: %w: %s", name, err, detail)
		}
		return nil, fmt.Errorf("mcp: initialize %q: %w", name, err)
	}
	if starter, ok := tr.(interface{ StartStream(context.Context) }); ok {
		starter.StartStream(context.Background())
	}
	return c, nil
}

func (c *Client) initialize(ctx context.Context) error {
	var result struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		Instructions    string         `json:"instructions"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": clientProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "lingzhu",
			"version": version.Version,
		},
	}, &result)
	if err != nil {
		return err
	}
	c.instructions = strings.TrimSpace(result.Instructions)
	if setter, ok := c.tr.(interface{ SetProtocolVersion(string) }); ok {
		protocol := result.ProtocolVersion
		if protocol == "" {
			protocol = clientProtocolVersion
		}
		setter.SetProtocolVersion(protocol)
	}
	return c.notify("notifications/initialized", nil)
}

// Name returns the configured server name.
func (c *Client) Name() string { return c.name }

// Instructions returns the server's usage guidance from the handshake.
func (c *Client) Instructions() string { return c.instructions }

// SetHandlers installs the manager callbacks used for catalog notifications and
// unexpected disconnects. An intentional Close does not invoke onClose.
func (c *Client) SetHandlers(onChange func(kind string), onClose func()) {
	c.handlerMu.Lock()
	c.onChange = onChange
	c.onClose = onClose
	c.handlerMu.Unlock()
}

func (c *Client) changed(kind string) {
	c.handlerMu.RLock()
	callback := c.onChange
	c.handlerMu.RUnlock()
	if callback != nil {
		callback(kind)
	}
}

func (c *Client) notifyClose() {
	c.handlerMu.RLock()
	callback := c.onClose
	c.handlerMu.RUnlock()
	if callback != nil {
		callback()
	}
}

// Closed reports whether the transport has already ended.
func (c *Client) Closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// ServerInstruction is one server's system-prompt guidance.
type ServerInstruction struct {
	Server       string
	Instructions string
}

// PromptArgument is one argument a prompt accepts.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// PromptInfo is one prompt advertised by an MCP server.
type PromptInfo struct {
	Server      string           `json:"-"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

const (
	modernProtocolVersion = "2026-07-28"
	protocolMetaKey       = "io.modelcontextprotocol/protocolVersion"
	clientInfoMetaKey     = "io.modelcontextprotocol/clientInfo"
	clientCapsMetaKey     = "io.modelcontextprotocol/clientCapabilities"
)

func modernEnvelope() map[string]any {
	return map[string]any{
		protocolMetaKey: modernProtocolVersion,
		clientInfoMetaKey: map[string]any{
			"name":    "lingzhu",
			"version": version.Version,
		},
		clientCapsMetaKey: map[string]any{},
	}
}

// discover probes for the 2026-07-28 revision with server/discover. A false
// result means the server did not offer the revision.
func (c *Client) discover(ctx context.Context) (bool, error) {
	var result struct {
		SupportedVersions []string       `json:"supportedVersions"`
		Capabilities      map[string]any `json:"capabilities"`
		Instructions      string         `json:"instructions"`
	}
	if err := c.request(ctx, "server/discover", map[string]any{"_meta": modernEnvelope()}, &result); err != nil {
		return false, err
	}
	found := false
	for _, item := range result.SupportedVersions {
		if item == modernProtocolVersion {
			found = true
			break
		}
	}
	if !found {
		return false, nil
	}
	c.modern = true
	c.protocol = modernProtocolVersion
	c.instructions = strings.TrimSpace(result.Instructions)
	if setter, ok := c.tr.(interface{ SetProtocolVersion(string) }); ok {
		setter.SetProtocolVersion(modernProtocolVersion)
	}
	return true, nil
}

// enrich adds the modern per-request _meta envelope; legacy requests are left
// untouched.
func (c *Client) enrich(params any) any {
	if !c.modern || c.protocol == "" {
		return params
	}
	base, ok := params.(map[string]any)
	if !ok {
		if params != nil {
			return params
		}
		base = map[string]any{}
	}
	merged := modernEnvelope()
	merged[protocolMetaKey] = c.protocol
	if meta, ok := base["_meta"].(map[string]any); ok {
		for key, value := range meta {
			merged[key] = value
		}
	}
	out := make(map[string]any, len(base)+1)
	for key, value := range base {
		out[key] = value
	}
	out["_meta"] = merged
	return out
}

// Tools reads every page of tools/list.
func (c *Client) Tools(ctx context.Context) ([]ToolInfo, error) {
	var tools []ToolInfo
	cursor := ""
	for pages := 1; ; pages++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Tools      []ToolInfo `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := c.requestWithTimeout(ctx, "tools/list", params, &page, millis(c.timeout.Catalog)); err != nil {
			return nil, err
		}
		tools = append(tools, page.Tools...)
		if page.NextCursor == "" {
			return tools, nil
		}
		if pages >= maxCatalogPages {
			return nil, fmt.Errorf("mcp: tools/list exceeded %d pages", maxCatalogPages)
		}
		cursor = page.NextCursor
	}
}

// Prompts reads prompts/list with cursor pagination. A server that does not
// implement prompts simply returns nothing.
func (c *Client) Prompts(ctx context.Context) ([]PromptInfo, error) {
	var out []PromptInfo
	cursor := ""
	for pages := 1; ; pages++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Prompts    []PromptInfo `json:"prompts"`
			NextCursor string       `json:"nextCursor"`
		}
		if err := c.requestWithTimeout(ctx, "prompts/list", params, &page, millis(c.timeout.Catalog)); err != nil {
			if rpc.IsMethodNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		for index := range page.Prompts {
			page.Prompts[index].Server = c.name
		}
		out = append(out, page.Prompts...)
		if page.NextCursor == "" {
			return out, nil
		}
		if pages >= maxCatalogPages {
			return nil, fmt.Errorf("mcp: prompts/list exceeded %d pages", maxCatalogPages)
		}
		cursor = page.NextCursor
	}
}

// Prompt renders one prompt to text. Message content may be a plain string or
// a typed content part; both are concatenated.
func (c *Client) Prompt(ctx context.Context, name string, args map[string]string) (string, error) {
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	var result struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := c.requestWithTimeout(ctx, "prompts/get", params, &result, millis(c.timeout.Execution)); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(result.Messages))
	for _, message := range result.Messages {
		var text string
		if err := json.Unmarshal(message.Content, &text); err == nil {
			parts = append(parts, text)
			continue
		}
		var part ToolContent
		if err := json.Unmarshal(message.Content, &part); err == nil && part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// Resources reads every page of resources/list and resources/templates/list.
// A server that does not implement resources simply returns nothing.
func (c *Client) Resources(ctx context.Context) ([]Resource, error) {
	var out []Resource
	cursor := ""
	for pages := 1; ; pages++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Resources  []Resource `json:"resources"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := c.requestWithTimeout(ctx, "resources/list", params, &page, millis(c.timeout.Catalog)); err != nil {
			if rpc.IsMethodNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		out = append(out, page.Resources...)
		if page.NextCursor == "" {
			break
		}
		if pages >= maxCatalogPages {
			return nil, fmt.Errorf("mcp: resources/list exceeded %d pages", maxCatalogPages)
		}
		cursor = page.NextCursor
	}

	cursor = ""
	for pages := 1; ; pages++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			ResourceTemplates []struct {
				Name        string `json:"name"`
				URITemplate string `json:"uriTemplate"`
				Description string `json:"description,omitempty"`
				MimeType    string `json:"mimeType,omitempty"`
			} `json:"resourceTemplates"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.requestWithTimeout(ctx, "resources/templates/list", params, &page, millis(c.timeout.Catalog)); err != nil {
			// Some servers declare resources without implementing templates.
			break
		}
		for _, item := range page.ResourceTemplates {
			out = append(out, Resource{
				Name:        item.Name,
				URITemplate: item.URITemplate,
				Description: item.Description,
				MimeType:    item.MimeType,
			})
		}
		if page.NextCursor == "" {
			break
		}
		if pages >= maxCatalogPages {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// ReadResource reads one resource by URI.
func (c *Client) ReadResource(ctx context.Context, uri string) ([]ResourceContent, error) {
	var result struct {
		Contents []ResourceContent `json:"contents"`
	}
	err := c.requestWithTimeout(ctx, "resources/read", map[string]any{"uri": uri}, &result, millis(c.timeout.Execution))
	if err != nil {
		return nil, err
	}
	return result.Contents, nil
}

// CallTool invokes one tool with JSON arguments.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	var result CallResult
	callCtx, cancel := context.WithTimeout(ctx, millis(c.timeout.Execution))
	defer cancel()
	if err := c.request(callCtx, "tools/call", map[string]any{"name": name, "arguments": args}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) request(ctx context.Context, method string, params any, out any) error {
	return c.requestWithTimeout(ctx, method, params, out, 0)
}

func (c *Client) requestWithTimeout(ctx context.Context, method string, params any, out any, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
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
	if params = c.enrich(params); params != nil {
		message["params"] = params
	}
	if err := c.send(ctx, message, id); err != nil {
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

func (c *Client) notify(method string, params any) error {
	message := map[string]any{"jsonrpc": "2.0", "method": method}
	if params = c.enrich(params); params != nil {
		message["params"] = params
	}
	ctx, cancel := context.WithTimeout(context.Background(), millis(c.timeout.Execution))
	defer cancel()
	return c.send(ctx, message, 0)
}

func (c *Client) send(ctx context.Context, message any, expectID int64) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return c.tr.Send(ctx, data, expectID)
}

// receive dispatches one decoded JSON-RPC message from the transport.
func (c *Client) receive(message rpc.Message) {
	if message.Method != "" {
		if len(message.ID) > 0 {
			// Server-initiated request. Only ping needs a reply; anything else
			// is not part of the client surface we expose.
			reply := map[string]any{"jsonrpc": "2.0", "id": message.ID}
			if message.Method == "ping" {
				reply["result"] = map[string]any{}
			} else {
				reply["error"] = rpc.Error{Code: -32601, Message: "method not supported"}
			}
			data, err := json.Marshal(reply)
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), millis(c.timeout.Execution))
				_ = c.tr.Send(ctx, data, 0)
				cancel()
			}
			return
		}
		// Servers announce catalog changes with a notification. The manager
		// re-lists the affected catalogs and refreshes the tool registry.
		switch message.Method {
		case "notifications/tools/list_changed":
			c.changed("tools")
		case "notifications/prompts/list_changed":
			c.changed("prompts")
		case "notifications/resources/list_changed", "notifications/resources/updated":
			c.changed("resources")
		}
		return
	}
	if len(message.ID) == 0 {
		return
	}
	id, err := rpc.ParseID(message.ID)
	if err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	c.mu.Unlock()
	if ch == nil {
		return
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

// closed is called by a transport when its stream or process ends. A close
// initiated by Close is expected and must not mark the server failed.
func (c *Client) closed() {
	c.doneOnce.Do(func() {
		close(c.done)
		if !c.closing.Load() {
			c.notifyClose()
		}
	})
	c.failAll(errClosed)
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[int64]chan rpc.Response{}
	c.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- rpc.Response{Err: err}:
		default:
		}
	}
}

func (c *Client) detail() string {
	if c.stderr == nil {
		return ""
	}
	return strings.TrimSpace(c.stderr.String())
}

// Close ends the server session.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.closing.Store(true)
		if c.tr != nil {
			_ = c.tr.Close()
		}
		c.closed()
	})
	return nil
}
