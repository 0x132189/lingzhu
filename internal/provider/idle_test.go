package provider

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/model"
)

// TestStreamIdleTimeoutFailsAStalledResponse proves a stream that stops
// sending is bounded: without this, a hung provider holds its turn forever.
func TestStreamIdleTimeoutFailsAStalledResponse(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Send one chunk, then stall until the test releases the handler.
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
	}))
	defer server.Close()
	defer close(release)

	client := NewOpenAI(server.URL, "key")
	client.Idle = 150 * time.Millisecond
	stream, err := client.Stream(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	chunk, err := stream.Recv()
	if err != nil {
		t.Fatalf("the first chunk should arrive: %v", err)
	}
	if chunk.Text != "hi" {
		t.Fatalf("unexpected chunk: %+v", chunk)
	}

	_, err = stream.Recv()
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("a stalled stream must report an idle timeout, got %v", err)
	}
	if !IsRetryable(err) {
		t.Fatal("an idle timeout is retryable")
	}
	if IsTimeout(err) {
		t.Fatal("an idle timeout must not spend the hung-endpoint budget")
	}
}

// TestStreamPassesDataThroughUnderTheIdleLimit keeps the watchdog from
// cutting a healthy response.
func TestStreamPassesDataThroughUnderTheIdleLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"b\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	defer server.Close()

	client := NewOpenAI(server.URL, "key")
	client.Idle = 2 * time.Second
	stream, err := client.Stream(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var text strings.Builder
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		text.WriteString(chunk.Text)
	}
	if text.String() != "ab" {
		t.Fatalf("unexpected text: %q", text.String())
	}
}

func TestTransportSetsHeaderTimeout(t *testing.T) {
	transport := NewTransport(Timeouts{Header: 3 * time.Second})
	if transport.ResponseHeaderTimeout != 3*time.Second {
		t.Fatalf("unexpected header timeout: %s", transport.ResponseHeaderTimeout)
	}
	fallback := NewTransport(Timeouts{})
	if fallback.ResponseHeaderTimeout != DefaultHeaderTimeout {
		t.Fatalf("unexpected fallback: %s", fallback.ResponseHeaderTimeout)
	}
}

// testRequest is the minimal request the adapters accept.
func testRequest() model.Request {
	return model.Request{
		Model:    "test-model",
		Messages: []model.Message{{Role: model.RoleUser, Content: "go"}},
	}
}
