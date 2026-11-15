package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		} `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Skills) != 1 || payload.Skills[0].ID != "demo" || payload.Skills[0].Description != "demo skill" {
		t.Fatalf("unexpected skill catalog: %+v", payload.Skills)
	}
	if strings.Contains(rec.Body.String(), "DEMO_BODY") {
		t.Fatalf("skill list leaked contents: %s", rec.Body.String())
	}

	srv, _ = newSessionServer(t, nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/skills", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"skills":[]`) {
		t.Fatalf("missing skill manager should return an empty list: %d %s", rec.Code, rec.Body.String())
	}
}
