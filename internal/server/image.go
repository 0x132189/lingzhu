package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"lingzhu/internal/imagegen"
)

// SetImage attaches the image generator; nil or unconfigured disables it.
func (s *Server) SetImage(manager *imagegen.Manager) {
	s.image = manager
}

func (s *Server) imageStatus(w http.ResponseWriter, r *http.Request) {
	status := imagegen.Status{}
	if s.image != nil {
		status = s.image.Status()
	}
	writeJSON(w, http.StatusOK, status)
}

// generateImage creates one image and returns it inline as base64.
func (s *Server) generateImage(w http.ResponseWriter, r *http.Request) {
	if s.image == nil || !s.image.Configured() {
		writeError(w, http.StatusNotImplemented, errText("image generation is not configured"))
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
		Size   string `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		writeError(w, http.StatusBadRequest, errText("prompt is empty"))
		return
	}
	result, err := s.image.Generate(r.Context(), prompt, strings.TrimSpace(body.Size))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"image": map[string]any{
			"prompt":        prompt,
			"mediaType":     result.MediaType,
			"extension":     result.Extension,
			"data":          base64.StdEncoding.EncodeToString(result.Data),
			"size":          len(result.Data),
			"revisedPrompt": result.RevisedPrompt,
		},
	})
}
