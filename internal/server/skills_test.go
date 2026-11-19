package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/skill"
)

func TestSkillListEndpoint(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: Demo\ndescription: demo skill\n---\nDEMO_BODY\n"
	if err := os.WriteFile(filepath.Join(root, "demo", "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := newSessionServer(t, func(s *Server) {
		s.SetSkills(skill.Discover([]string{root}, nil))
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/skills", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected skills status: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Skills []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Path        string `json:"path"`
			Autoinvoke  bool   `json:"autoinvoke"`
			Source      string `json:"source"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Skills) != 1 || payload.Skills[0].ID != "demo" || payload.Skills[0].Description != "demo skill" {
		t.Fatalf("unexpected skill catalog: %+v", payload.Skills)
	}
	if !payload.Skills[0].Autoinvoke || !strings.HasSuffix(payload.Skills[0].Path, "SKILL.md") {
		t.Fatalf("skill list should expose its path and autoinvoke: %+v", payload.Skills[0])
	}
	if payload.Skills[0].Source == "" {
		t.Fatalf("skill list should label its source: %+v", payload.Skills[0])
	}
	if strings.Contains(rec.Body.String(), "DEMO_BODY") {
		t.Fatalf("skill list leaked contents: %s", rec.Body.String())
	}

	skillPath := filepath.Join(root, "demo", "SKILL.md")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/file?path="+url.QueryEscape(skillPath), nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "DEMO_BODY") {
		t.Fatalf("skill file should be readable: %d %s", rec.Code, rec.Body.String())
	}

	srv, _ = newSessionServer(t, nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/skills", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"skills":[]`) {
		t.Fatalf("missing skill manager should return an empty list: %d %s", rec.Code, rec.Body.String())
	}
}
