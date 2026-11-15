package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSearchFiles(t *testing.T) {
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
	write("src/app.go", "package main")
	write("src/helper.go", "package main")
	write("README.md", "docs")
	write("node_modules/pkg/index.js", "ignored")

	srv, _ := newSessionServer(t, nil)
	srv.SetWorkdir(root)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/search?q=app.go", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected search status: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Files) != 1 || payload.Files[0].Path != "src/app.go" {
		t.Fatalf("unexpected search hits: %+v", payload.Files)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/search?q=index", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Files) != 0 {
		t.Fatalf("ignored directories should not match: %+v", payload.Files)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/search?q=.go&limit=1", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Files) != 1 || !payload.Truncated {
		t.Fatalf("limit should truncate: %+v", payload)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/search?q=", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty query should 400, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/search?q=x&limit=0", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit should 400, got %d", rec.Code)
	}
}
