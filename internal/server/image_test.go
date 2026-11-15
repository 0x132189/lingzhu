package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/imagegen"
)

var testPNG = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x00, 0x00, 0x0d}

func TestGenerateImageEndpoint(t *testing.T) {
	var received string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = string(body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"b64_json":       base64.StdEncoding.EncodeToString(testPNG),
				"revised_prompt": "a revised cat",
			}},
		})
	}))
	defer upstream.Close()

	manager := imagegen.New(imagegen.Config{APIKey: "key", Endpoint: upstream.URL})
	srv, _ := newSessionServer(t, func(s *Server) { s.SetImage(manager) })

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/image",
		strings.NewReader(`{"prompt":"a cat","size":"512x512"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for image generation, got %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Image struct {
			Prompt        string `json:"prompt"`
			MediaType     string `json:"mediaType"`
			Extension     string `json:"extension"`
			Data          string `json:"data"`
			Size          int    `json:"size"`
			RevisedPrompt string `json:"revisedPrompt"`
		} `json:"image"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	image := payload.Image
	if image.Prompt != "a cat" || image.MediaType != "image/png" || image.Extension != ".png" {
		t.Fatalf("unexpected image metadata: %+v", image)
	}
	if image.Size != len(testPNG) || image.RevisedPrompt != "a revised cat" {
		t.Fatalf("unexpected image payload: %+v", image)
	}
	data, err := base64.StdEncoding.DecodeString(image.Data)
	if err != nil || string(data) != string(testPNG) {
		t.Fatalf("unexpected image bytes: %v", err)
	}
	if !strings.Contains(received, "a cat") || !strings.Contains(received, "512x512") {
		t.Fatalf("upstream request missing prompt/size: %s", received)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/image", strings.NewReader(`{"prompt":" "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty prompt should 400, got %d", rec.Code)
	}
}

func TestGenerateImageDisabled(t *testing.T) {
	srv, _ := newSessionServer(t, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/image", strings.NewReader(`{"prompt":"a cat"}`)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("missing manager should 501, got %d", rec.Code)
	}
}
