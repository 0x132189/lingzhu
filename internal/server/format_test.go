package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/format"
)

func TestHelperFormatServer(t *testing.T) {
	if os.Getenv("LINGZHU_FORMAT_SERVER_HELPER") != "1" {
		return
	}
	file := os.Args[len(os.Args)-1]
	data, err := os.ReadFile(file)
	if err != nil {
		os.Exit(1)
	}
	if err := os.WriteFile(file, []byte(strings.ReplaceAll(string(data), "bad", "good")), 0o644); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestFormatterEndpoints(t *testing.T) {
	workdir := t.TempDir()
	manager := format.New(workdir, format.Config{Formatters: map[string]format.Definition{
		"fixer": {
			Command:    []string{os.Args[0], "-test.run=TestHelperFormatServer", "$FILE"},
			Extensions: []string{".txt"},
			Environment: map[string]string{
				"LINGZHU_FORMAT_SERVER_HELPER": "1",
			},
		},
	}}, t.Logf)
	server := newTestServer(t)
	server.SetFormatters(manager)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/formatters", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"fixer"`) {
		t.Fatalf("unexpected formatters response: %d %s", rec.Code, rec.Body.String())
	}

	path := filepath.Join(workdir, "note.txt")
	if err := os.WriteFile(path, []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/format", strings.NewReader(`{"path":"note.txt"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected format status: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Format format.Result `json:"format"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Format.Name != "fixer" || !body.Format.Changed {
		t.Fatalf("unexpected format result: %+v", body.Format)
	}
	if data, _ := os.ReadFile(path); string(data) != "good" {
		t.Fatalf("formatter did not run: %q", data)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/format", strings.NewReader(`{"path":"../outside.txt"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("outside path should be rejected: %d %s", rec.Code, rec.Body.String())
	}
}
