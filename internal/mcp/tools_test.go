package mcp

import (
	"strings"
	"testing"

	"lingzhu/internal/model"
	"lingzhu/internal/tool"
)

func TestAttachResultMedia(t *testing.T) {
	var attachments []model.Attachment
	call := tool.Call{
		CanAttach: true,
		OnAttachment: func(attachment model.Attachment) {
			attachments = append(attachments, attachment)
		},
	}
	attachResultMedia(call, &CallResult{Content: []ToolContent{
		{Type: "image", MimeType: "image/png", Data: "aGVsbG8="},
		{Type: "audio", MimeType: "audio/wav", Data: "aGVsbG8="},
		{Type: "resource", Resource: &ResourceContent{URI: "memo://pdf", MimeType: "application/pdf", Blob: "aGVsbG8="}},
		{Type: "resource", Resource: &ResourceContent{URI: "memo://text", MimeType: "text/plain", Text: "body"}},
	}})
	if len(attachments) != 2 {
		t.Fatalf("unexpected attachments: %+v", attachments)
	}
	if attachments[0].Mime != "image/png" || attachments[1].Mime != "application/pdf" {
		t.Fatalf("unexpected attachment mimes: %+v", attachments)
	}
}

func TestAttachResultMediaNeedsAttachmentSupport(t *testing.T) {
	called := false
	call := tool.Call{OnAttachment: func(model.Attachment) { called = true }}
	attachResultMedia(call, &CallResult{Content: []ToolContent{{Type: "image", MimeType: "image/png", Data: "aGVsbG8="}}})
	if called {
		t.Fatal("media must not be attached when the model cannot accept attachments")
	}
}

func TestAttachResourceContents(t *testing.T) {
	var attachments []model.Attachment
	call := tool.Call{CanAttach: true, OnAttachment: func(attachment model.Attachment) {
		attachments = append(attachments, attachment)
	}}
	attachResourceContents(call, []ResourceContent{
		{URI: "memo://image", MimeType: "image/png", Blob: "aGVsbG8="},
		{URI: "memo://text", MimeType: "text/plain", Text: "body"},
	})
	if len(attachments) != 1 || attachments[0].Mime != "image/png" {
		t.Fatalf("unexpected resource attachments: %+v", attachments)
	}
}

func TestFormatCallResultKeepsTextAndStructured(t *testing.T) {
	out := formatCallResult(&CallResult{
		Content:           []ToolContent{{Type: "text", Text: "3"}},
		StructuredContent: []byte(`{"sum":3}`),
	})
	if !strings.Contains(out, "3") || !strings.Contains(out, `"sum": 3`) {
		t.Fatalf("text and structured content should both survive: %q", out)
	}
}

func TestFormatCallResultResourceContent(t *testing.T) {
	out := formatCallResult(&CallResult{Content: []ToolContent{
		{Type: "resource", Resource: &ResourceContent{URI: "memo://hello", MimeType: "text/plain", Text: "hello"}},
		{Type: "resource_link", URI: "memo://link", Name: "Link"},
	}})
	for _, want := range []string{"hello", "resource link", "memo://link"} {
		if !strings.Contains(out, want) {
			t.Fatalf("formatted result missing %q: %q", want, out)
		}
	}
}
