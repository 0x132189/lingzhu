package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/artifact"
)

func TestArtifactEndpoints(t *testing.T) {
	server := newTestServer(t)
	manager, err := artifact.New(t.TempDir(), artifact.Config{})
	if err != nil {
		t.Fatal(err)
	}
	server.SetArtifacts(manager)

	created, err := manager.Create("Demo", map[string]string{
		"index.html": "<h1>artifact</h1>",
		"app.js":     "console.log(1)",
	}, "", "s1")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/artifacts", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("unexpected list: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/artifacts/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected get status: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Artifact artifact.Content `json:"artifact"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Artifact.Title != "Demo" || payload.Artifact.Files["index.html"] != "<h1>artifact</h1>" {
		t.Fatalf("unexpected artifact content: %+v", payload.Artifact)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/artifacts/"+created.ID+"/raw/index.html", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "<h1>artifact</h1>" {
		t.Fatalf("unexpected raw file: %d %s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("unexpected content type: %q", contentType)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/artifacts/"+created.ID+"/raw/..%2Fartifact.json", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("traversal should 404: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/artifacts/"+created.ID, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected delete status: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/artifacts/"+created.ID, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted artifact should 404: %d", rec.Code)
	}
}

func TestArtifactHTTPWrite(t *testing.T) {
	server := newTestServer(t)
	manager, err := artifact.New(t.TempDir(), artifact.Config{})
	if err != nil {
		t.Fatal(err)
	}
	server.SetArtifacts(manager)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/artifacts/providers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected providers status: %d %s", rec.Code, rec.Body.String())
	}
	var providers struct {
		Providers []struct {
			ID      string `json:"id"`
			Default bool   `json:"default"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &providers); err != nil {
		t.Fatal(err)
	}
	if len(providers.Providers) != 1 || providers.Providers[0].ID != "local" || !providers.Providers[0].Default {
		t.Fatalf("unexpected providers: %s", rec.Body.String())
	}

	body := `{"title":"HTTP Demo","files":{"index.html":"<h1>HTTP_MARKER</h1>","app.js":"console.log(1)"}}`
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/artifacts", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Artifact artifact.Info `json:"artifact"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Artifact.ID == "" || created.Artifact.Entry != "index.html" || len(created.Artifact.Files) != 2 {
		t.Fatalf("unexpected artifact: %+v", created.Artifact)
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/artifacts/"+created.Artifact.ID+"/raw/index.html", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "HTTP_MARKER") {
		t.Fatalf("unexpected raw file: %d %s", rec.Code, rec.Body.String())
	}

	update := `{"title":"HTTP Demo v2","files":{"index.html":"<h1>HTTP_V2</h1>"},"entry":"index.html"}`
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/artifacts/"+created.Artifact.ID, strings.NewReader(update)))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected update status: %d %s", rec.Code, rec.Body.String())
	}
	var updated struct {
		Artifact artifact.Info `json:"artifact"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Artifact.Title != "HTTP Demo v2" || len(updated.Artifact.Files) != 1 {
		t.Fatalf("unexpected updated artifact: %+v", updated.Artifact)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/artifacts/"+created.Artifact.ID, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "HTTP_V2") {
		t.Fatalf("unexpected updated content: %d %s", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct {
		method string
		path   string
		body   string
		want   int
	}{
		{http.MethodPost, "/artifacts", `{"title":"x","files":{"index.html":"x"},"provider":"remote"}`, http.StatusBadRequest},
		{http.MethodPost, "/artifacts", `{"title":"x","files":{"../escape":"x"}}`, http.StatusBadRequest},
		{http.MethodPost, "/artifacts", `{"title":"","files":{"index.html":"x"}}`, http.StatusBadRequest},
		{http.MethodPatch, "/artifacts/art_0000000000000000", `{"files":{"index.html":"x"}}`, http.StatusNotFound},
		{http.MethodPatch, "/artifacts/" + created.Artifact.ID, `{"files":{}}`, http.StatusBadRequest},
	} {
		rec = httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Fatalf("%s %s: want %d, got %d %s", tc.method, tc.path, tc.want, rec.Code, rec.Body.String())
		}
	}
}

func TestArtifactDisabledEndpoints(t *testing.T) {
	server := newTestServer(t)
	for _, item := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/artifacts", ""},
		{http.MethodGet, "/artifacts/providers", ""},
		{http.MethodPost, "/artifacts", `{"title":"x","files":{"index.html":"x"}}`},
	} {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(item.method, item.path, strings.NewReader(item.body)))
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s should be 501, got %d %s", item.method, item.path, rec.Code, rec.Body.String())
		}
	}
}
