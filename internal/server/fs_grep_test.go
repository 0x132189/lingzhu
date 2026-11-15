package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestGrepFiles(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/app.go", "package main\n// QUARTZ marker\nfunc main() {}\n")
	write("src/other.go", "package main\n")
	write("node_modules/pkg/index.js", "QUARTZ ignored\n")
	write("binary.bin", "QUARTZ\x00binary")

	srv, _ := newSessionServer(t, nil)
	srv.SetWorkdir(root)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/grep?q=quartz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected grep status: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Hits []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"hits"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Hits) != 1 || payload.Hits[0].Path != "src/app.go" || payload.Hits[0].Line != 2 {
		t.Fatalf("unexpected grep hits: %+v", payload.Hits)
	}
	if payload.Truncated {
		t.Fatal("unexpected truncation")
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/grep?q=package&limit=1", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Hits) != 1 || !payload.Truncated {
		t.Fatalf("limit should truncate: %+v", payload)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/grep?q=", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty query should 400, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/grep?q=x&limit=0", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit should 400, got %d", rec.Code)
	}
}
