package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/event"
)

func TestSessionEventsFollow(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/sessions/s1/events?after=0&follow=true", nil)
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

	lines := make(chan string, 128)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	waitFor := func(marker string) {
		t.Helper()
		timeout := time.After(3 * time.Second)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("event stream closed before %q", marker)
				}
				if strings.HasPrefix(line, "data: ") && strings.Contains(line, marker) {
					return
				}
			case <-timeout:
				t.Fatalf("event stream did not report %q", marker)
			}
		}
	}

	waitFor("seed")

	live := event.Assistant("live answer")
	seq, err := store.Append("s1", live)
	if err != nil {
		t.Fatal(err)
	}
	live.Seq = seq
	srv.agent.Publish("s1", live)

	waitFor("live answer")
}
