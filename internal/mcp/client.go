package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lingzhu/internal/rpc"
	"lingzhu/internal/version"
)

const (
	clientProtocolVersion = "2025-11-25"
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
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
	Annotations  map[string]any  `json:"annotations,omitempty"`
	Execution    map[string]any  `json:"execution,omitempty"`
	Meta         map[string]any  `json:"_meta,omitempty"`
}

// Resource is one resource or resource template advertised by a server.
type Resource struct {
	Name        string `json:"name"`
	URI         string `json:"uri,omitempty"`
	URITemplate string `json:"uriTemplate,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// Completion is one completion/complete answer.
type Completion struct {
	Values  []string `json:"values"`
	Total   int      `json:"total,omitempty"`
	HasMore bool     `json:"hasMore,omitempty"`
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
	Content           []ToolContent              `json:"content"`
	StructuredContent json.RawMessage            `json:"structuredContent"`
	IsError           bool                       `json:"isError"`
	ResultType        string                     `json:"resultType,omitempty"`
	InputRequests     map[string]json.RawMessage `json:"inputRequests,omitempty"`
	RequestState      string                     `json:"requestState,omitempty"`
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
	// title is the server's own display name from the handshake.
	title string
	// instructions is the server's own usage guidance from the handshake. It
	// is injected into the system prompt like the reference harness does.
	instructions string

	// stderr is only set for local servers and is used to explain handshake
	// failures.
	stderr *tailBuffer

	mu      sync.Mutex
	pending map[int64]chan rpc.Response
	seq     int64

	workdir             string
	shared              bool
	logger              func(string, ...any)
	resourcesCapability bool
	toolsCapability     bool
	eventsCapability    bool

	// refreshMu serializes catalog re-listing triggered by server notifications.
	refreshMu   sync.Mutex
	subscribeMu sync.Mutex
	subscribed  map[string]bool
	// handlerMu protects the manager callbacks, which are set after the handshake.
	handlerMu   sync.RWMutex
	listeners   map[int64]clientListener
	listenerSeq int64
	onSample    func(ctx context.Context, params json.RawMessage) (any, error)
	onElicit    func(ctx context.Context, params json.RawMessage) (any, error)

	sessionMu sync.Mutex
	sessions  []string

	streamMu sync.Mutex
	streams  map[string]*eventStream

	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once
	closing   atomic.Bool
	reinit    atomic.Bool
	timeout   Timeout
}

// Connect starts or dials an MCP server and completes the initialize
// handshake.
func Connect(ctx context.Context, name string, cfg Server, workdir string) (*Client, error) {
	return connect(ctx, name, cfg, workdir, nil)
}

// connect is Connect with an optional OAuth token resolver.
func connect(ctx context.Context, name string, cfg Server, workdir string, tokens TokenFunc) (*Client, error) {
	var token func(context.Context, bool) (string, error)
	if tokens != nil && cfg.Kind() != "local" && !cfg.OAuth.Disabled {
		endpoint := cfg.OAuth.Name
		if endpoint == "" {
			endpoint = "mcp:" + name
		}
		token = func(c context.Context, force bool) (string, error) { return tokens(c, endpoint, force) }
	}
	c := newClient(name, cfg, workdir)

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
			c.startSubscriptions()
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

func newClient(name string, cfg Server, workdir string) *Client {
	return &Client{
		name:       name,
		cfg:        cfg,
		workdir:    workdir,
		pending:    map[int64]chan rpc.Response{},
		subscribed: map[string]bool{},
		streams:    map[string]*eventStream{},
		done:       make(chan struct{}),
		timeout:    cfg.timeouts(),
	}
}

func (c *Client) initialize(ctx context.Context) error {
	var result struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		Instructions    string         `json:"instructions"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": clientProtocolVersion,
		"capabilities":    c.capabilities(),
		"clientInfo": map[string]any{
			"name":    "lingzhu",
			"version": version.Version,
		},
	}, &result)
	if err != nil {
		return err
	}
	c.instructions = strings.TrimSpace(result.Instructions)
	c.title = strings.TrimSpace(result.ServerInfo.Title)
	if c.title == "" {
		c.title = strings.TrimSpace(result.ServerInfo.Name)
	}
	c.resourcesCapability = result.Capabilities["resources"] != nil
	c.toolsCapability = result.Capabilities["tools"] != nil
	c.eventsCapability = hasExtension(result.Capabilities, eventsExtension)
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

// Title returns the server's own display name from the handshake.
func (c *Client) Title() string { return c.title }

// SetLogger installs the destination for server log notifications.
func (c *Client) SetLogger(logger func(string, ...any)) {
	c.handlerMu.Lock()
	c.logger = logger
	c.handlerMu.Unlock()
}

type clientListener struct {
	onChange func(kind string)
	onClose  func()
}

// AddHandlers adds one observer and returns a function that removes it. A
// shared client can have several managers watching it at once.
func (c *Client) AddHandlers(onChange func(kind string), onClose func()) func() {
	c.handlerMu.Lock()
	if c.listeners == nil {
		c.listeners = map[int64]clientListener{}
	}
	id := c.listenerSeq
	c.listenerSeq++
	c.listeners[id] = clientListener{onChange: onChange, onClose: onClose}
	c.handlerMu.Unlock()
	return func() {
		c.handlerMu.Lock()
		delete(c.listeners, id)
		c.handlerMu.Unlock()
	}
}

// SetSampling installs the handler for server-initiated sampling requests.
func (c *Client) SetSampling(handler func(ctx context.Context, params json.RawMessage) (any, error)) {
	c.handlerMu.Lock()
	c.onSample = handler
	c.handlerMu.Unlock()
}

// SetElicitation installs the handler for server-initiated user input.
func (c *Client) SetElicitation(handler func(ctx context.Context, params json.RawMessage) (any, error)) {
	c.handlerMu.Lock()
	c.onElicit = handler
	c.handlerMu.Unlock()
}

func (c *Client) elicitCall(ctx context.Context, params json.RawMessage) (any, error) {
	c.handlerMu.RLock()
	handler := c.onElicit
	c.handlerMu.RUnlock()
	if handler == nil {
		return map[string]any{"action": "decline"}, nil
	}
	return handler(ctx, params)
}

func (c *Client) elicitRequest(id json.RawMessage, params json.RawMessage) {
	reply := map[string]any{"jsonrpc": "2.0", "id": id}
	ctx, cancel := context.WithTimeout(context.Background(), millis(c.timeout.Execution))
	result, err := c.elicitCall(ctx, params)
	cancel()
	if err != nil {
		reply["result"] = map[string]any{"action": "decline"}
	} else {
		reply["result"] = result
	}
	data, err := json.Marshal(reply)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), millis(c.timeout.Execution))
		_ = c.tr.Send(ctx, data, 0)
		cancel()
	}
}

// ActiveSession is the session owning the most recent tool call on this
// connection. Elicitation and sampling use it to attach UI and context.
func (c *Client) ActiveSession() string {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	if len(c.sessions) == 0 {
		return ""
	}
	return c.sessions[len(c.sessions)-1]
}

// CallToolSession is CallTool with the owning session recorded for requests
// the server starts while the tool call is running.
func (c *Client) CallToolSession(ctx context.Context, name string, args map[string]any, sessionID string) (*CallResult, error) {
	if sessionID != "" {
		c.sessionMu.Lock()
		c.sessions = append(c.sessions, sessionID)
		c.sessionMu.Unlock()
		defer func() {
			c.sessionMu.Lock()
			for index := len(c.sessions) - 1; index >= 0; index-- {
				if c.sessions[index] == sessionID {
					c.sessions = append(c.sessions[:index], c.sessions[index+1:]...)
					break
				}
			}
			c.sessionMu.Unlock()
		}()
	}
	return c.callTool(ctx, name, args, sessionID)
}

// sample runs the installed sampling handler.
func (c *Client) sample(ctx context.Context, params json.RawMessage) (any, error) {
	c.handlerMu.RLock()
	handler := c.onSample
	c.handlerMu.RUnlock()
	if handler == nil {
		return nil, errors.New("sampling is not enabled")
	}
	return handler(ctx, params)
}

func (c *Client) sampleRequest(id json.RawMessage, params json.RawMessage) {
	reply := map[string]any{"jsonrpc": "2.0", "id": id}
	ctx, cancel := context.WithTimeout(context.Background(), millis(c.timeout.Execution))
	result, err := c.sample(ctx, params)
	cancel()
	if err != nil {
		reply["error"] = rpc.Error{Code: -32603, Message: err.Error()}
	} else {
		reply["result"] = result
	}
	data, err := json.Marshal(reply)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), millis(c.timeout.Execution))
		_ = c.tr.Send(ctx, data, 0)
		cancel()
	}
}

func (c *Client) changed(kind string) {
	c.handlerMu.RLock()
	callbacks := make([]func(string), 0, len(c.listeners))
	for _, listener := range c.listeners {
		if listener.onChange != nil {
			callbacks = append(callbacks, listener.onChange)
		}
	}
	c.handlerMu.RUnlock()
	for _, callback := range callbacks {
		callback(kind)
	}
}

func (c *Client) notifyClose() {
	c.handlerMu.RLock()
	callbacks := make([]func(), 0, len(c.listeners))
	for _, listener := range c.listeners {
		if listener.onClose != nil {
			callbacks = append(callbacks, listener.onClose)
		}
	}
	c.handlerMu.RUnlock()
	for _, callback := range callbacks {
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

func (c *Client) modernEnvelope() map[string]any {
	return map[string]any{
		protocolMetaKey: modernProtocolVersion,
		clientInfoMetaKey: map[string]any{
			"name":    "lingzhu",
			"version": version.Version,
		},
		clientCapsMetaKey: c.capabilities(),
	}
}

func (c *Client) capabilities() map[string]any {
	capabilities := map[string]any{
		"roots": map[string]any{"listChanged": false},
		"extensions": map[string]any{
			eventsExtension: map[string]any{},
		},
	}
	if c.cfg.Sampling {
		capabilities["sampling"] = map[string]any{}
	}
	if c.cfg.Elicitation {
		capabilities["elicitation"] = map[string]any{
			"form": map[string]any{},
			"url":  map[string]any{},
		}
	}
	return capabilities
}

// discover probes for the 2026-07-28 revision with server/discover. A false
// result means the server did not offer the revision.
func (c *Client) discover(ctx context.Context) (bool, error) {
	var result struct {
		SupportedVersions []string       `json:"supportedVersions"`
		Capabilities      map[string]any `json:"capabilities"`
		Instructions      string         `json:"instructions"`
		ServerInfo        struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := c.request(ctx, "server/discover", map[string]any{"_meta": c.modernEnvelope()}, &result); err != nil {
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
	c.title = strings.TrimSpace(result.ServerInfo.Title)
	if c.title == "" {
		c.title = strings.TrimSpace(result.ServerInfo.Name)
	}
	c.resourcesCapability = result.Capabilities["resources"] != nil
	c.toolsCapability = result.Capabilities["tools"] != nil
	c.eventsCapability = hasExtension(result.Capabilities, eventsExtension)
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
	merged := c.modernEnvelope()
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
	if !c.toolsCapability {
		return nil, nil
	}
	var tools []ToolInfo
	cursor := ""
	for pages := 1; ; pages++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := c.requestCatalog(ctx, "tools/list", params, &page, millis(c.timeout.Catalog)); err != nil {
			return nil, err
		}
		for _, raw := range page.Tools {
			info, ok := parseToolInfo(raw)
			if !ok {
				c.logf("mcp[%s] skipping invalid tool", c.name)
				continue
			}
			tools = append(tools, info)
		}
		if page.NextCursor == "" {
			return tools, nil
		}
		if pages >= maxCatalogPages {
			return nil, fmt.Errorf("mcp: tools/list exceeded %d pages", maxCatalogPages)
		}
		cursor = page.NextCursor
	}
}

// parseToolInfo validates one listed tool before it reaches a provider. A
// malformed entry is skipped instead of failing the whole catalog.
func parseToolInfo(raw json.RawMessage) (ToolInfo, bool) {
	var item struct {
		Name         string          `json:"name"`
		Title        string          `json:"title"`
		Description  string          `json:"description"`
		InputSchema  json.RawMessage `json:"inputSchema"`
		OutputSchema json.RawMessage `json:"outputSchema"`
		Annotations  json.RawMessage `json:"annotations"`
		Execution    json.RawMessage `json:"execution"`
		Meta         json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return ToolInfo{}, false
	}
	name := strings.TrimSpace(item.Name)
	if name == "" {
		return ToolInfo{}, false
	}
	schema, ok := normalizeToolSchema(item.InputSchema)
	if !ok {
		return ToolInfo{}, false
	}
	return ToolInfo{
		Name:         name,
		Title:        item.Title,
		Description:  item.Description,
		InputSchema:  schema,
		OutputSchema: objectJSON(item.OutputSchema),
		Annotations:  objectMap(item.Annotations),
		Execution:    objectMap(item.Execution),
		Meta:         objectMap(item.Meta),
	}, true
}

// normalizeToolSchema keeps the top level valid for provider checks while
// leaving nested property schemas exactly as the server sent them.
func normalizeToolSchema(raw json.RawMessage) (json.RawMessage, bool) {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return nil, false
	}
	if kind, ok := schema["type"]; ok && kind != "object" {
		return nil, false
	}
	schema["type"] = "object"
	if _, ok := schema["properties"].(map[string]any); !ok {
		delete(schema, "properties")
	}
	if required, ok := schema["required"].([]any); ok {
		for _, value := range required {
			if _, ok := value.(string); !ok {
				delete(schema, "required")
				break
			}
		}
	} else if _, ok := schema["required"]; ok {
		delete(schema, "required")
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, false
	}
	return data, true
}

func objectMap(raw json.RawMessage) map[string]any {
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}

func objectJSON(raw json.RawMessage) json.RawMessage {
	if objectMap(raw) == nil {
		return nil
	}
	return raw
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
		if err := c.requestCatalog(ctx, "prompts/list", params, &page, millis(c.timeout.Catalog)); err != nil {
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
	if err := c.requestCatalog(ctx, "prompts/get", params, &result, millis(c.timeout.Execution)); err != nil {
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
		if err := c.requestCatalog(ctx, "resources/list", params, &page, millis(c.timeout.Catalog)); err != nil {
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
		if err := c.requestCatalog(ctx, "resources/templates/list", params, &page, millis(c.timeout.Catalog)); err != nil {
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
	c.subscribeResources(ctx, out)
	return out, nil
}

// subscribeResources asks a server that advertises resources to push changes
// for every listed URI. Failures are ignored because a server may advertise
// the capability without implementing subscriptions.
func (c *Client) subscribeResources(ctx context.Context, resources []Resource) {
	if !c.resourcesCapability {
		return
	}
	for _, item := range resources {
		uri := strings.TrimSpace(item.URI)
		if uri == "" {
			continue
		}
		c.subscribeMu.Lock()
		if c.subscribed[uri] {
			c.subscribeMu.Unlock()
			continue
		}
		c.subscribeMu.Unlock()
		if err := c.requestWithTimeout(ctx, "resources/subscribe", map[string]any{"uri": uri}, nil, millis(c.timeout.Execution)); err != nil {
			c.logf("mcp[%s] subscribe %s: %v", c.name, uri, err)
			continue
		}
		c.subscribeMu.Lock()
		c.subscribed[uri] = true
		c.subscribeMu.Unlock()
	}
}

func (c *Client) logf(format string, args ...any) {
	c.handlerMu.RLock()
	logger := c.logger
	c.handlerMu.RUnlock()
	if logger != nil {
		logger(format, args...)
	}
}

// Complete asks a server for prompt or resource argument completions.
func (c *Client) Complete(ctx context.Context, ref map[string]any, argument, value string) (Completion, error) {
	var result struct {
		Completion Completion `json:"completion"`
	}
	params := map[string]any{
		"ref":      ref,
		"argument": map[string]string{"name": argument, "value": value},
	}
	err := c.requestCatalog(ctx, "completion/complete", params, &result, millis(c.timeout.Execution))
	return result.Completion, err
}

// ReadResource reads one resource by URI.
func (c *Client) ReadResource(ctx context.Context, uri string) ([]ResourceContent, error) {
	if !c.resourcesCapability {
		return nil, nil
	}
	var result struct {
		Contents []ResourceContent `json:"contents"`
	}
	err := c.requestCatalog(ctx, "resources/read", map[string]any{"uri": uri}, &result, millis(c.timeout.Execution))
	if err != nil {
		return nil, err
	}
	return result.Contents, nil
}

// CallTool invokes one tool with JSON arguments.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallResult, error) {
	return c.callTool(ctx, name, args, "")
}

func (c *Client) callTool(ctx context.Context, name string, args map[string]any, sessionID string) (*CallResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	params := map[string]any{
		"name":      name,
		"arguments": args,
	}
	if sessionID != "" {
		params["_meta"] = map[string]any{"lingzhu/sessionID": sessionID}
	}
	callCtx, cancel := context.WithTimeout(ctx, millis(c.timeout.Execution))
	defer cancel()
	for round := 0; round < 10; round++ {
		var result CallResult
		if err := c.request(callCtx, "tools/call", params, &result); err != nil {
			return nil, err
		}
		if result.ResultType != "input_required" {
			return &result, nil
		}
		if len(result.InputRequests) == 0 {
			return nil, fmt.Errorf("mcp: tool %q requires input but sent no requests", name)
		}
		responses := make(map[string]any, len(result.InputRequests))
		for key, raw := range result.InputRequests {
			var request struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(raw, &request); err != nil || request.Method == "" {
				return nil, fmt.Errorf("mcp: tool %q sent an invalid input request", name)
			}
			switch request.Method {
			case "elicitation/create":
				value, err := c.elicitCall(callCtx, request.Params)
				if err != nil {
					return nil, err
				}
				responses[key] = value
			case "sampling/createMessage":
				value, err := c.sample(callCtx, request.Params)
				if err != nil {
					return nil, err
				}
				responses[key] = value
			case "roots/list":
				responses[key] = c.rootsResult()
			default:
				return nil, fmt.Errorf("mcp: tool %q requested unsupported input %q", name, request.Method)
			}
		}
		params = map[string]any{
			"name":           name,
			"arguments":      args,
			"inputResponses": responses,
		}
		if result.RequestState != "" {
			params["requestState"] = result.RequestState
		}
		if sessionID != "" {
			params["_meta"] = map[string]any{"lingzhu/sessionID": sessionID}
		}
	}
	return nil, fmt.Errorf("mcp: tool %q exceeded the input round limit", name)
}

func (c *Client) request(ctx context.Context, method string, params any, out any) error {
	return c.requestWithTimeout(ctx, method, params, out, 0)
}

// requestCatalog retries transient catalog/read failures twice, matching the
// reference client's short backoff. Tool calls are excluded because a retry can
// duplicate a side effect.
func (c *Client) requestCatalog(ctx context.Context, method string, params any, out any, timeout time.Duration) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = c.requestWithTimeout(ctx, method, params, out, timeout)
		if err == nil || !retryableError(err) {
			return err
		}
		if attempt == 2 {
			return err
		}
		timer := time.NewTimer(time.Duration(250*(1<<attempt)) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-c.done:
			timer.Stop()
			return errClosed
		case <-timer.C:
		}
	}
	return err
}

func retryableError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"http 408", "http 429", "http 500", "http 502", "http 503", "http 504",
		"connection reset", "broken pipe", "unexpected eof", "temporarily unavailable",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func (c *Client) requestWithTimeout(ctx context.Context, method string, params any, out any, timeout time.Duration) error {
	for attempt := 0; ; attempt++ {
		err := c.requestOnce(ctx, method, params, out, timeout)
		if !errors.Is(err, errSessionExpired) || attempt > 0 || method == "initialize" {
			return err
		}
		if !c.reinit.CompareAndSwap(false, true) {
			return err
		}
		reinitErr := c.initialize(ctx)
		c.reinit.Store(false)
		if reinitErr != nil {
			return err
		}
	}
}

func (c *Client) requestOnce(ctx context.Context, method string, params any, out any, timeout time.Duration) error {
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

// startSubscriptions opens the modern long-lived catalog-change channel. The
// server may keep the request open until shutdown, so it runs in the
// background; list_changed notifications feed the existing refresh path.
func (c *Client) startSubscriptions() {
	if !c.modern {
		return
	}
	go func() {
		params := map[string]any{
			"notifications": map[string]any{
				"toolsListChanged":     true,
				"promptsListChanged":   true,
				"resourcesListChanged": true,
			},
		}
		_ = c.request(context.Background(), "subscriptions/listen", params, nil)
	}()
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
			switch message.Method {
			case "sampling/createMessage":
				go c.sampleRequest(message.ID, message.Params)
				return
			case "elicitation/create":
				go c.elicitRequest(message.ID, message.Params)
				return
			}
			reply := map[string]any{"jsonrpc": "2.0", "id": message.ID}
			switch message.Method {
			case "ping":
				reply["result"] = map[string]any{}
			case "roots/list":
				reply["result"] = c.rootsResult()
			default:
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
		case "notifications/message":
			c.logNotification(message.Params)
		case "notifications/progress":
			c.logProgress(message.Params)
		case "notifications/tools/list_changed":
			c.changed("tools")
		case "notifications/prompts/list_changed":
			c.changed("prompts")
		case "notifications/resources/list_changed", "notifications/resources/updated":
			c.changed("resources")
		case "notifications/elicitation/complete":
			c.logf("mcp[%s] elicitation completed", c.name)
		default:
			if strings.HasPrefix(message.Method, "notifications/events/") {
				c.routeEventNotification(message.Method, message.Params)
			}
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

func (c *Client) rootsResult() map[string]any {
	if c.shared || strings.TrimSpace(c.workdir) == "" {
		return map[string]any{"roots": []any{}}
	}
	root := map[string]string{
		"uri":  fileURL(c.workdir),
		"name": filepath.Base(c.workdir),
	}
	return map[string]any{"roots": []any{root}}
}

func fileURL(path string) string {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed}).String()
}

func (c *Client) logNotification(params json.RawMessage) {
	c.handlerMu.RLock()
	logger := c.logger
	c.handlerMu.RUnlock()
	if logger == nil {
		return
	}
	var payload struct {
		Level  string `json:"level"`
		Logger string `json:"logger"`
		Data   any    `json:"data"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return
	}
	label := strings.TrimSpace(payload.Logger)
	if label == "" {
		label = "server"
	}
	logger("mcp[%s] %s %s: %v", c.name, payload.Level, label, payload.Data)
}

func (c *Client) logProgress(params json.RawMessage) {
	c.handlerMu.RLock()
	logger := c.logger
	c.handlerMu.RUnlock()
	if logger == nil {
		return
	}
	var payload struct {
		Progress float64 `json:"progress"`
		Total    float64 `json:"total"`
		Message  string  `json:"message"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return
	}
	if payload.Message != "" {
		logger("mcp[%s] progress %v/%v: %s", c.name, payload.Progress, payload.Total, payload.Message)
	}
}

// closed is called by a transport when its stream or process ends. A close
// initiated by Close is expected and must not mark the server failed.
func (c *Client) closed() {
	c.doneOnce.Do(func() {
		close(c.done)
		c.closeEventStreams()
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
