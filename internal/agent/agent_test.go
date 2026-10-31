package agent

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
)

type fakeProvider struct {
	requests []model.Request
	chunks   [][]provider.Chunk
}

func (f *fakeProvider) Stream(_ context.Context, req model.Request) (provider.Stream, error) {
	f.requests = append(f.requests, req)
	index := len(f.requests) - 1
	if index >= len(f.chunks) {
		return &fakeStream{}, nil
	}
	return &fakeStream{chunks: f.chunks[index]}, nil
}

type fakeStream struct {
	chunks []provider.Chunk
	i      int
}

func (s *fakeStream) Recv() (provider.Chunk, error) {
	if s.i >= len(s.chunks) {
		return provider.Chunk{}, io.EOF
	}
	chunk := s.chunks[s.i]
	s.i++
	return chunk, nil
}

func (s *fakeStream) Close() error { return nil }

type echoTool struct{}

func (echoTool) Name() string            { return "echo" }
func (echoTool) Description() string     { return "echo text" }
func (echoTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (echoTool) Run(_ context.Context, in json.RawMessage) (string, error) {
	return "echo:" + string(in), nil
}

func newTestAgent(t *testing.T, fake *fakeProvider) (*Agent, *session.Store) {
	t.Helper()
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry()
	registry.Add(echoTool{})
	return New(store, fake, registry, "test-model", 4), store
}

func TestRunExecutesToolThenFinishes(t *testing.T) {
	fake := &fakeProvider{chunks: [][]provider.Chunk{
		{{ToolCalls: []provider.ToolCallDelta{{Index: 0, ID: "call_1", Name: "echo", Arguments: `{"text":"hi"}`}}}},
		{{Text: "done"}},
	}}
	a, store := newTestAgent(t, fake)

	var events []event.Event
	stream, err := a.Run(context.Background(), "s1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	for ev := range stream {
		events = append(events, ev)
	}

	last := events[len(events)-1]
	if last.Type != event.RunEnd || last.Data["reason"] != "done" {
		t.Fatalf("unexpected last event: %+v", last)
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Role{model.RoleUser, model.RoleAssistant, model.RoleTool, model.RoleAssistant}
	if len(messages) != len(want) {
		t.Fatalf("want %d messages, got %d: %+v", len(want), len(messages), messages)
	}
	if messages[2].Content != `echo:{"text":"hi"}` {
		t.Fatalf("tool result mismatch: %q", messages[2].Content)
	}
	if messages[3].Content != "done" {
		t.Fatalf("final assistant message mismatch: %q", messages[3].Content)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("want 2 provider calls, got %d", len(fake.requests))
	}
	if len(fake.requests[0].Tools) != 1 {
		t.Fatalf("tools not passed to provider: %+v", fake.requests[0].Tools)
	}
}

func TestRunStopsAtMaxSteps(t *testing.T) {
	call := provider.Chunk{ToolCalls: []provider.ToolCallDelta{
		{Index: 0, ID: "call_1", Name: "echo", Arguments: `{}`},
	}}
	fake := &fakeProvider{chunks: [][]provider.Chunk{{call}, {call}, {call}, {call}, {call}}}
	a, _ := newTestAgent(t, fake)

	var last event.Event
	stream, err := a.Run(context.Background(), "s1", "loop")
	if err != nil {
		t.Fatal(err)
	}
	for ev := range stream {
		last = ev
	}
	if last.Type != event.RunEnd || last.Data["reason"] != "max_steps" {
		t.Fatalf("want max_steps end, got %+v", last)
	}
}

func TestRunRequiresModel(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := New(store, &fakeProvider{}, tool.NewRegistry(), "", 4)
	if _, err := a.Run(context.Background(), "s1", "x"); err == nil {
		t.Fatal("want error when model is empty")
	}
}
