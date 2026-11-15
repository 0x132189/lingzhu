package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/agent"
	"lingzhu/internal/media"
	"lingzhu/internal/model"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
)

func pngAttachmentData(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
}

func newAttachmentServer(t *testing.T) (*Server, *session.Store) {
	t.Helper()
	store := testStore(t)
	catalog := provider.Static("test", "Test", staticProvider{}, []provider.ModelSpec{
		{ID: "m", Name: "m", Attachment: true},
	}, "m")
	return New(agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 1}), store), store
}

func largePNGAttachmentData(t *testing.T, width, height int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
}

func TestNormalizeAttachments(t *testing.T) {
	data := pngAttachmentData(t)
	got, err := normalizeAttachments([]model.Attachment{{Mime: "IMAGE/PNG", Data: data}}, media.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Mime != "image/png" || got[0].Name != "image.png" {
		t.Fatalf("unexpected attachment: %+v", got)
	}

	textData := base64.StdEncoding.EncodeToString([]byte("hello"))
	text, err := normalizeAttachments([]model.Attachment{{Mime: "text/markdown", Name: "notes.md", Data: textData}}, media.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(text) != 1 || text[0].Mime != "text/markdown" || text[0].Name != "notes.md" || text[0].Kind != "text" {
		t.Fatalf("unexpected text attachment: %+v", text)
	}

	ranged, err := normalizeAttachments([]model.Attachment{{
		Mime: "text/plain", Name: "notes.txt", Data: textData, Kind: "text", Start: 2, End: 4,
	}}, media.Default())
	if err != nil || len(ranged) != 1 || ranged[0].Start != 2 || ranged[0].End != 4 || ranged[0].Kind != "text" {
		t.Fatalf("unexpected ranged attachment: %+v %v", ranged, err)
	}

	directory, err := normalizeAttachments([]model.Attachment{{
		Mime: "text/plain", Name: "src/", Data: textData, Kind: "directory",
	}}, media.Default())
	if err != nil || len(directory) != 1 || directory[0].Kind != "directory" {
		t.Fatalf("unexpected directory attachment: %+v %v", directory, err)
	}

	large := largePNGAttachmentData(t, 300, 200)
	enabled := true
	resized, err := normalizeAttachments([]model.Attachment{{Mime: "image/png", Data: large}}, media.Config{Image: media.ImageConfig{
		AutoResize: &enabled, MaxWidth: 50, MaxHeight: 50, MaxBase64Bytes: 1 << 20,
	}})
	if err != nil || len(resized) != 1 {
		t.Fatalf("image resize failed: %+v %v", resized, err)
	}
	decoded, err := base64.StdEncoding.DecodeString(resized[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	picture, _, err := image.Decode(bytes.NewReader(decoded))
	if err != nil {
		t.Fatal(err)
	}
	if picture.Bounds().Dx() > 50 || picture.Bounds().Dy() > 50 {
		t.Fatalf("prompt image not resized: %v", picture.Bounds())
	}

	invalidUTF8 := base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe})
	if _, err := normalizeAttachments([]model.Attachment{{Mime: "text/plain", Data: invalidUTF8}}, media.Default()); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 text should be rejected, got %v", err)
	}

	invalid := []struct {
		name  string
		items []model.Attachment
		want  string
	}{
		{"unsupported", []model.Attachment{{Mime: "application/octet-stream", Data: data}}, "unsupported"},
		{"empty", []model.Attachment{{Mime: "image/png"}}, "empty"},
		{"base64", []model.Attachment{{Mime: "image/png", Data: "not base64!"}}, "base64"},
		{"range", []model.Attachment{{Mime: "text/plain", Data: textData, Start: 0, End: 2}}, "invalid line range"},
		{"media range", []model.Attachment{{Mime: "image/png", Data: data, Start: 1, End: 2}}, "cannot have a line range"},
		{"directory media", []model.Attachment{{Mime: "image/png", Data: data, Kind: "directory"}}, "must use a text type"},
		{"kind", []model.Attachment{{Mime: "text/plain", Data: textData, Kind: "unknown"}}, "unsupported attachment kind"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeAttachments(test.items, media.Default()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want %q error, got %v", test.want, err)
			}
		})
	}
}

func TestPromptStoresUserAttachments(t *testing.T) {
	srv, store := newAttachmentServer(t)
	seedSession(t, store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	data := pngAttachmentData(t)
	body, err := json.Marshal(map[string]any{
		"text": "look at this",
		"attachments": []model.Attachment{
			{Mime: "image/png", Name: "pixel.png", Data: data},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(httpSrv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected prompt status: %d", resp.StatusCode)
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, message := range messages {
		if message.Role != model.RoleUser || message.Content != "look at this" {
			continue
		}
		if len(message.Attachments) == 1 && message.Attachments[0].Name == "pixel.png" && message.Attachments[0].Data == data {
			found = true
		}
	}
	if !found {
		t.Fatalf("user attachment missing from history: %+v", messages)
	}
}

func TestPromptRejectsAttachmentsForTextOnlyModel(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	data := pngAttachmentData(t)
	body := `{"text":"look","attachments":[{"mime":"image/png","name":"pixel.png","data":"` + data + `"}]}`
	resp, err := http.Post(httpSrv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(payload), "does not accept attachments") {
		t.Fatalf("unexpected response: %d %s", resp.StatusCode, payload)
	}
}

func TestPublicMessagesStripsAttachmentData(t *testing.T) {
	source := []model.Message{{
		Role:    model.RoleUser,
		Content: "look",
		Attachments: []model.Attachment{
			{Mime: "image/png", Name: "pixel.png", Data: "AAA"},
		},
	}}
	stripped := publicMessages(source)
	if len(stripped) != 1 || len(stripped[0].Attachments) != 1 {
		t.Fatalf("unexpected messages: %+v", stripped)
	}
	if stripped[0].Attachments[0].Data != "" || stripped[0].Attachments[0].Name != "pixel.png" {
		t.Fatalf("attachment data should be stripped: %+v", stripped[0].Attachments[0])
	}
	if source[0].Attachments[0].Data != "AAA" {
		t.Fatalf("source messages must not be mutated")
	}
}

func TestPromptAcceptsTextAttachmentForTextOnlyModel(t *testing.T) {
	srv, store := newSessionServer(t, nil)
	seedSession(t, store, "s1")
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	data := base64.StdEncoding.EncodeToString([]byte("hello"))
	body := `{"text":"read","attachments":[{"mime":"text/plain","name":"notes.txt","data":"` + data + `"}]}`
	resp, err := http.Post(httpSrv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("text attachment should be accepted: %d %s", resp.StatusCode, payload)
	}
}
