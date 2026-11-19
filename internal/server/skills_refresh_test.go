package server

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	services := &location.Services{
		Ref:    ref,
		Skills: skill.Discover(nil, nil),
		RefreshSkills: func(context.Context) error {
			called = true
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
}
