package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
)

// A turn that fails must leave a durable explanation, so a client that
// reconnects, or a reader after a restart, still sees why the reply stopped.
func TestFailedTurnRecordsDurableFailure(t *testing.T) {
	store := testStore(t)
	fake := &fakeProvider{
		chunks: [][]provider.Chunk{{{Text: "partial"}}},
		after:  []error{errors.New("provider exploded")},
	}
	runner := New(store, testCatalog(fake, "test-model", 128000), tool.NewRegistry(), nil, Options{Retries: 1, RetryDelay: 0})
	stream, err := runner.Run(context.Background(), "s1", "hello", provider.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}

	events, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	var failure string
	for _, ev := range events {
		if ev.Type == event.RunFailed {
			failure, _ = ev.Data["message"].(string)
		}
	}
	if !strings.Contains(failure, "provider exploded") {
		t.Fatalf("expected a durable failure naming the error, got %q", failure)
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Failure != nil {
			t.Fatalf("a failure row must not reach the model: %+v", message.Failure)
		}
	}

	records, err := store.MessageRecords("s1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range records {
		if record.Failure != nil && strings.Contains(record.Failure.Message, "provider exploded") {
			found = true
		}
	}
	if !found {
		t.Fatal("history should keep the failure row for the transcript")
	}
}

// A failure row is transcript-only: it must not be replayed as an assistant
// message on the next request.
func TestFailureRowIsTranscriptOnly(t *testing.T) {
	events := []event.Event{
		event.User("hello"),
		event.RunFailedEvent("boom"),
	}
	messages := session.BuildMessages(events)
	if len(messages) != 1 || messages[0].Role != model.RoleUser {
		t.Fatalf("expected only the user turn, got %+v", messages)
	}
}
