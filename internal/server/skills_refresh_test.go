package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/location"
	"lingzhu/internal/skill"
)

func TestSkillsRefreshEndpoint(t *testing.T) {
	root := t.TempDir()
	ref, err := location.Make(root)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	source := ""
	services := &location.Services{
		Ref:    ref,
		Skills: skill.Discover(nil, nil),
		RefreshSkills: func(context.Context) error {
			called = true
			return nil
		},
		RefreshSkillSource: func(_ context.Context, url string) error {
			source = url
			return nil
		},
	}
	set := location.NewSet()
	set.Put(services)
	srv, _ := newSessionServer(t, func(s *Server) { s.SetLocations(set) })

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/skills/refresh", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected refresh status: %d %s", rec.Code, rec.Body.String())
	}
	if !called {
		t.Fatal("refresh callback was not invoked")
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/skills/refresh", strings.NewReader(`{"url":"https://example.com/skills"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected source refresh status: %d %s", rec.Code, rec.Body.String())
	}
	if source != "https://example.com/skills" {
		t.Fatalf("source refresh callback got %q", source)
	}
}
