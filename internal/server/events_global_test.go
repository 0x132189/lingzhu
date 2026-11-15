package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/event"
)

// A client that reconnects with its last cross-session position must receive
// the events it missed, in order, without any event arriving twice.
func TestAllEventsResumesFromPosition(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	if _, err := store.Append("s2", event.User("second")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("s1", event.Assistant("third")); err != nil {
		t.Fatal(err)
	}

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/events?after=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	frames := make(chan map[string]any, 16)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var frame map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame) == nil {
				frames <- frame
			}
		}
		close(frames)
	}()

	next := func() map[string]any {
		t.Helper()
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("stream closed before the replayed events arrived")
			}
			return frame
		case <-time.After(3 * time.Second):
			t.Fatal("replayed events did not arrive")
			return nil
		}
	}

	first := next()
	if first["sessionId"] != "s2" || first["type"] != string(event.UserMessage) {
		t.Fatalf("unexpected first replayed frame: %v", first)
	}
	if first["global"] != float64(2) {
		t.Fatalf("frame should carry its cross-session position: %v", first)
	}
	second := next()
	if second["sessionId"] != "s1" || second["type"] != string(event.AssistantMessage) {
		t.Fatalf("unexpected second replayed frame: %v", second)
	}
	if second["global"] != float64(3) {
		t.Fatalf("frame should carry its cross-session position: %v", second)
	}

	// A live event continues the same numbering, so the client never has to
	// guess which position to resume from next.
	stored, err := store.AppendEvent("s1", event.Assistant("live answer"))
	if err != nil {
		t.Fatal(err)
	}
	srv.agent.Publish("s1", stored)

	third := next()
	if third["global"] != float64(4) || !strings.Contains(toString(third["data"]), "live answer") {
		t.Fatalf("unexpected live frame: %v", third)
	}
}

// Positions are unique and increasing across sessions, so a client can resume
// from any of them.
func TestStoreGlobalPositionsAreUniqueAcrossSessions(t *testing.T) {
	_, store := newSessionServer(t, nil)
	for _, session := range []string{"s1", "s2", "s1", "s2"} {
		if _, err := store.Append(session, event.User("message")); err != nil {
			t.Fatal(err)
		}
	}
	list, err := store.GlobalAfter(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("expected 4 events, got %d", len(list))
	}
	for index, item := range list {
		if item.Event.Global != int64(index+1) {
			t.Fatalf("event %d has position %d", index, item.Event.Global)
		}
	}
	if list[0].SessionID != "s1" || list[1].SessionID != "s2" {
		t.Fatalf("events should stay in write order: %+v", list)
	}
	if again, err := store.GlobalAfter(2, 100); err != nil || len(again) != 2 {
		t.Fatalf("expected 2 events after position 2, got %d (%v)", len(again), err)
	}
}

func toString(value any) string {
	text, _ := value.(map[string]any)
	if text == nil {
		return ""
	}
	message, _ := text["text"].(string)
	return message
}
