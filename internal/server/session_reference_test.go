package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type referencePayload struct {
	Query      string `json:"query"`
	Path       string `json:"path"`
	References []struct {
		Path   string `json:"path"`
		Line   int    `json:"line"`
		Column int    `json:"column"`
		Text   string `json:"text"`
	} `json:"references"`
	Truncated bool `json:"truncated"`
}

func newReferenceServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"sub", "node_modules", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"a.go":                 "package a\nfunc Target() {}\n",
		"sub/b.go":             "// Target here\nvar x = Target\n",
		"node_modules/skip.go": "Target\n",
		".hidden/hidden.go":    "Target\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv, store := newSessionServer(t, func(s *Server) { s.SetWorkdir(root) })
	seedSession(t, store, "s1")
	return srv, root
}

func getReference(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestSessionReference(t *testing.T) {
	srv, _ := newReferenceServer(t)

	rec := getReference(t, srv, "/sessions/s1/reference?q=Target")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for reference, got %d: %s", rec.Code, rec.Body.String())
	}
	var payload referencePayload
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Query != "Target" || payload.Path != "" || payload.Truncated {
		t.Fatalf("unexpected reference metadata: %+v", payload)
	}
	if len(payload.References) != 3 {
		t.Fatalf("want 3 references, got %+v", payload.References)
	}
	paths := map[string]bool{}
	for _, ref := range payload.References {
		paths[ref.Path] = true
		if strings.Contains(ref.Path, "node_modules") || strings.Contains(ref.Path, ".hidden") {
			t.Fatalf("skipped directory leaked into results: %+v", ref)
		}
	}
	if !paths["a.go"] || !paths["sub/b.go"] {
		t.Fatalf("unexpected reference paths: %+v", payload.References)
	}
	if payload.References[0].Path == "a.go" {
		if payload.References[0].Line != 2 || payload.References[0].Column != 6 {
			t.Fatalf("unexpected a.go position: %+v", payload.References[0])
		}
		if payload.References[0].Text != "func Target() {}" {
			t.Fatalf("unexpected a.go text: %+v", payload.References[0])
		}
	}

	rec = getReference(t, srv, "/sessions/s1/reference?q=target")
	var insensitive referencePayload
	if err := json.NewDecoder(rec.Body).Decode(&insensitive); err != nil {
		t.Fatal(err)
	}
	if len(insensitive.References) != 3 {
		t.Fatalf("search should be case-insensitive by default: %+v", insensitive.References)
	}

	rec = getReference(t, srv, "/sessions/s1/reference?q=target&case_sensitive=true")
	var sensitive referencePayload
	if err := json.NewDecoder(rec.Body).Decode(&sensitive); err != nil {
		t.Fatal(err)
	}
	if len(sensitive.References) != 0 {
		t.Fatalf("case-sensitive search should not match: %+v", sensitive.References)
	}

	rec = getReference(t, srv, "/sessions/s1/reference?q=Target&path=sub")
	var scoped referencePayload
	if err := json.NewDecoder(rec.Body).Decode(&scoped); err != nil {
		t.Fatal(err)
	}
	if scoped.Path != "sub" || len(scoped.References) != 2 {
		t.Fatalf("unexpected scoped references: %+v", scoped)
	}
	for _, ref := range scoped.References {
		if ref.Path != "sub/b.go" {
			t.Fatalf("scoped search leaked: %+v", ref)
		}
	}

	rec = getReference(t, srv, "/sessions/s1/reference?q=Target&limit=1")
	var limited referencePayload
	if err := json.NewDecoder(rec.Body).Decode(&limited); err != nil {
		t.Fatal(err)
	}
	if len(limited.References) != 1 || !limited.Truncated {
		t.Fatalf("limit should truncate: %+v", limited)
	}
}

func TestSessionReferenceErrors(t *testing.T) {
	srv, _ := newReferenceServer(t)

	cases := []struct {
		target string
		status int
	}{
		{"/sessions/s1/reference", http.StatusBadRequest},
		{"/sessions/s1/reference?q=", http.StatusBadRequest},
		{"/sessions/missing/reference?q=Target", http.StatusNotFound},
		{"/sessions/s1/reference?q=Target&path=..%2F", http.StatusBadRequest},
		{"/sessions/s1/reference?q=Target&path=a.go", http.StatusBadRequest},
		{"/sessions/s1/reference?q=Target&path=missing", http.StatusNotFound},
	}
	for _, item := range cases {
		rec := getReference(t, srv, item.target)
		if rec.Code != item.status {
			t.Fatalf("want %d for %s, got %d", item.status, item.target, rec.Code)
		}
	}
}
