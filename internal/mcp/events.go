package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lingzhu/internal/rpc"
)

// MCP Events is a draft extension. It is implemented as an additive
// capability: servers that do not declare it keep working unchanged.
const (
	eventsExtension        = "io.modelcontextprotocol/events"
	eventsSubscriptionMeta = "io.modelcontextprotocol/subscriptionId"
	maxEventListPages      = 100
)

// eventStreamIdle is how long a stream may stay silent. Servers send a
// heartbeat at least every 30 seconds, so twice that means the stream is dead.
var eventStreamIdle = 60 * time.Second

// EventType is one event a server advertises through events/list.
type EventType struct {
	Name          string          `json:"name"`
	Description   string          `json:"description,omitempty"`
	Delivery      []string        `json:"delivery,omitempty"`
	InputSchema   json.RawMessage `json:"inputSchema,omitempty"`
	PayloadSchema json.RawMessage `json:"payloadSchema,omitempty"`
}

// EventError is a server-reported stream error.
type EventError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// EventMessage is one message on an events/stream subscription.
type EventMessage struct {
	Type      string          `json:"type"`
	Cursor    *string         `json:"cursor,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	EventID   string          `json:"eventId,omitempty"`
	Name      string          `json:"name,omitempty"`
	Timestamp string          `json:"timestamp,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     *EventError     `json:"error,omitempty"`
}

type eventStream struct {
	id     int64
	client *Client
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	ch       chan EventMessage
	activity chan struct{}
	once     sync.Once
}

func (s *eventStream) send(message EventMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- message:
	default:
	}
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

func (s *eventStream) fail(message string) {
	s.send(EventMessage{Type: "error", Error: &EventError{Code: -32603, Message: message}})
	s.finish()
}

func (s *eventStream) finish() {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.ch)
		s.mu.Unlock()
		s.cancel()
		s.client.removeEventStream(s.id)
	})
}

// EventsSupported reports whether the server declared the events extension
// during the handshake.
func (c *Client) EventsSupported() bool { return c.eventsCapability }

// EventTypes reads every page of events/list. Servers without the extension
// return an explicit error instead of an empty catalog.
func (c *Client) EventTypes(ctx context.Context) ([]EventType, error) {
	if !c.eventsCapability {
		return nil, fmt.Errorf("mcp: server %q does not offer events", c.name)
	}
	var out []EventType
	cursor := ""
	for pages := 1; ; pages++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Events     []EventType `json:"events"`
			NextCursor string      `json:"nextCursor"`
		}
		if err := c.requestWithTimeout(ctx, "events/list", params, &page, millis(c.timeout.Catalog)); err != nil {
			return nil, err
		}
		out = append(out, page.Events...)
		if page.NextCursor == "" {
			return out, nil
		}
		if pages >= maxEventListPages {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// StreamEvents subscribes to one event type. The returned channel closes when
// the server ends the stream or the caller cancels it. The stop function
// unsubscribes and closes the channel.
func (c *Client) StreamEvents(ctx context.Context, name string, args map[string]any, cursor *string, maxAgeMs int) (<-chan EventMessage, func(), error) {
	if !c.eventsCapability {
		return nil, nil, fmt.Errorf("mcp: server %q does not offer events", c.name)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil, errors.New("mcp: event name is required")
	}
	streamCtx, cancel := context.WithCancel(ctx)
	id := atomic.AddInt64(&c.seq, 1)
	stream := &eventStream{
		id:       id,
		client:   c,
		cancel:   cancel,
		ch:       make(chan EventMessage, 64),
		activity: make(chan struct{}, 1),
	}
	c.streamMu.Lock()
	c.streams[strconv.FormatInt(id, 10)] = stream
	c.streamMu.Unlock()

	pending := make(chan rpc.Response, 1)
	c.mu.Lock()
	c.pending[id] = pending
	c.mu.Unlock()

	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	if cursor != nil {
		params["cursor"] = *cursor
	}
	if maxAgeMs > 0 {
		params["maxAgeMs"] = maxAgeMs
	}
	message := map[string]any{"jsonrpc": "2.0", "id": id, "method": "events/stream"}
	if enriched := c.enrich(params); enriched != nil {
		message["params"] = enriched
	}

	go func() {
		idle := time.NewTimer(eventStreamIdle)
		defer idle.Stop()
		for {
			select {
			case <-stream.activity:
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(eventStreamIdle)
			case <-idle.C:
				stream.fail(fmt.Sprintf("MCP server %q sent nothing on the event stream for %s", c.name, eventStreamIdle))
				return
			case <-streamCtx.Done():
				return
			case <-c.done:
				return
			}
		}
	}()

	go func() {
		sendErr := c.send(streamCtx, message, id)
		if sendErr != nil {
			if streamCtx.Err() == nil && !errors.Is(sendErr, errClosed) {
				stream.fail(sendErr.Error())
			} else {
				stream.finish()
			}
			c.clearPending(id)
			return
		}
		// Transports either wait for the response themselves (stdio and
		// HTTP) or return after writing (legacy SSE); waiting on the pending
		// channel covers both.
		select {
		case response := <-pending:
			if response.Err != nil {
				stream.fail(response.Err.Error())
			} else {
				stream.finish()
			}
		case <-streamCtx.Done():
			stream.finish()
		case <-c.done:
			stream.finish()
		}
		c.clearPending(id)
	}()

	stop := func() {
		stream.cancel()
		notification := map[string]any{
			"jsonrpc": "2.0",
			"method":  "notifications/cancelled",
			"params":  map[string]any{"requestId": id, "reason": "client stopped the stream"},
		}
		if data, err := json.Marshal(notification); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), millis(c.timeout.Execution))
			_ = c.tr.Send(ctx, data, 0)
			cancel()
		}
		stream.finish()
	}
	return stream.ch, stop, nil
}

func (c *Client) clearPending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) removeEventStream(id int64) {
	c.streamMu.Lock()
	delete(c.streams, strconv.FormatInt(id, 10))
	c.streamMu.Unlock()
}

func (c *Client) eventStreamFor(key string) *eventStream {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	return c.streams[key]
}

func (c *Client) closeEventStreams() {
	c.streamMu.Lock()
	streams := make([]*eventStream, 0, len(c.streams))
	for _, stream := range c.streams {
		streams = append(streams, stream)
	}
	c.streams = map[string]*eventStream{}
	c.streamMu.Unlock()
	for _, stream := range streams {
		stream.finish()
	}
}

// hasExtension reports whether a server capability map declares an extension.
func hasExtension(capabilities map[string]any, name string) bool {
	extensions, ok := capabilities["extensions"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = extensions[name]
	return ok
}

func eventNotificationKey(params json.RawMessage) string {
	var payload struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return ""
	}
	raw := strings.TrimSpace(string(payload.Meta[eventsSubscriptionMeta]))
	if raw == "" || raw == "null" {
		return ""
	}
	if unquoted, err := strconv.Unquote(raw); err == nil {
		return unquoted
	}
	return raw
}

func eventCursor(raw json.RawMessage) *string {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return &value
}

func (c *Client) routeEventNotification(method string, params json.RawMessage) {
	key := eventNotificationKey(params)
	if key == "" {
		return
	}
	stream := c.eventStreamFor(key)
	if stream == nil {
		return
	}
	switch method {
	case "notifications/events/active":
		var payload struct {
			Cursor    json.RawMessage `json:"cursor"`
			Truncated bool            `json:"truncated"`
		}
		if err := json.Unmarshal(params, &payload); err != nil {
			return
		}
		stream.send(EventMessage{Type: "active", Cursor: eventCursor(payload.Cursor), Truncated: payload.Truncated})
	case "notifications/events/event":
		var payload struct {
			EventID   string          `json:"eventId"`
			Name      string          `json:"name"`
			Timestamp string          `json:"timestamp"`
			Data      json.RawMessage `json:"data"`
			Cursor    json.RawMessage `json:"cursor"`
		}
		if err := json.Unmarshal(params, &payload); err != nil {
			return
		}
		stream.send(EventMessage{
			Type:      "event",
			EventID:   payload.EventID,
			Name:      payload.Name,
			Timestamp: payload.Timestamp,
			Data:      payload.Data,
			Cursor:    eventCursor(payload.Cursor),
		})
	case "notifications/events/heartbeat":
		var payload struct {
			Cursor json.RawMessage `json:"cursor"`
		}
		if err := json.Unmarshal(params, &payload); err != nil {
			return
		}
		stream.send(EventMessage{Type: "heartbeat", Cursor: eventCursor(payload.Cursor)})
	case "notifications/events/error":
		var payload struct {
			Error EventError `json:"error"`
		}
		if err := json.Unmarshal(params, &payload); err != nil {
			return
		}
		stream.send(EventMessage{Type: "error", Error: &payload.Error})
	case "notifications/events/terminated":
		var payload struct {
			Error EventError `json:"error"`
		}
		if err := json.Unmarshal(params, &payload); err != nil {
			stream.fail("event stream terminated")
			return
		}
		stream.fail(payload.Error.Message)
	}
}
