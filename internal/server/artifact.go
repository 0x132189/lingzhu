package server

import (
	"io"
	"net/http"
	"strings"

	"lingzhu/internal/artifact"
)

// SetArtifacts attaches the artifact store; nil disables the endpoints.
func (s *Server) SetArtifacts(manager *artifact.Manager) {
	s.artifacts = manager
}

type artifactProvider struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
	Default      bool     `json:"default"`
}

type artifactInput struct {
	Title     string            `json:"title"`
	Files     map[string]string `json:"files"`
	Entry     string            `json:"entry"`
	Provider  string            `json:"provider"`
	SessionID string            `json:"sessionID"`
}

// withArtifacts answers 501 when the store is unavailable and hands the handler
// the manager otherwise, so no handler repeats the guard.
func (s *Server) withArtifacts(fn func(http.ResponseWriter, *http.Request, *artifact.Manager)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.artifacts == nil || !s.artifacts.Enabled() {
			writeError(w, http.StatusNotImplemented, errText("artifacts are not available"))
			return
		}
		fn(w, r, s.artifacts)
	}
}

func artifactProviders(w http.ResponseWriter, r *http.Request, manager *artifact.Manager) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": []artifactProvider{{
		ID:           "local",
		Name:         "Local",
		Capabilities: []string{"static"},
		Default:      true,
	}}})
}

func listArtifacts(w http.ResponseWriter, r *http.Request, manager *artifact.Manager) {
	items, err := manager.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if items == nil {
		items = []artifact.Info{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": items})
}

func createArtifact(w http.ResponseWriter, r *http.Request, manager *artifact.Manager) {
	var body artifactInput
	if err := jsonDecode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if provider := strings.TrimSpace(body.Provider); provider != "" && provider != "local" {
		writeError(w, http.StatusBadRequest, errText("unknown artifact provider"))
		return
	}
	info, err := manager.Create(body.Title, body.Files, body.Entry, body.SessionID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"artifact": info})
}

func updateArtifact(w http.ResponseWriter, r *http.Request, manager *artifact.Manager) {
	var body artifactInput
	if err := jsonDecode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	info, err := manager.Update(r.PathValue("id"), body.Title, body.Files, body.Entry, body.SessionID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifact": info})
}

func getArtifact(w http.ResponseWriter, r *http.Request, manager *artifact.Manager) {
	content, err := manager.Get(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifact": content})
}

func deleteArtifact(w http.ResponseWriter, r *http.Request, manager *artifact.Manager) {
	if err := manager.Delete(r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func serveArtifactFile(w http.ResponseWriter, r *http.Request, manager *artifact.Manager) {
	file, mediaType, err := manager.OpenFile(r.PathValue("id"), r.PathValue("path"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}
