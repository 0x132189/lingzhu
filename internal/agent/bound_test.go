package agent

import (
	"strings"
	"testing"

	"lingzhu/internal/model"
)

func TestBoundImagesDropsOldest(t *testing.T) {
	image := strings.Repeat("a", 9*1024*1024)
	messages := []model.Message{
		{Role: model.RoleUser, Content: "first", Attachments: []model.Attachment{{Mime: "image/png", Data: image}}},
		{Role: model.RoleUser, Attachments: []model.Attachment{{Mime: "image/png", Data: image}}},
		{Role: model.RoleUser, Content: "third", Attachments: []model.Attachment{{Mime: "image/png", Data: image}}},
		{Role: model.RoleUser, Content: "pdf", Attachments: []model.Attachment{{Mime: "application/pdf", Data: image}}},
	}
	out := boundImages(messages)
	if len(out[0].Attachments) != 0 || !strings.Contains(out[0].Content, "removed") {
		t.Fatalf("oldest image should be replaced by a note: %+v", out[0])
	}
	if len(out[1].Attachments) != 0 || out[1].Content != imageRemoved {
		t.Fatalf("second image should be replaced by only the note: %+v", out[1])
	}
	if len(out[2].Attachments) != 1 {
		t.Fatalf("newest image should stay: %+v", out[2])
	}
	if len(out[3].Attachments) != 1 {
		t.Fatalf("PDFs do not count toward the image budget: %+v", out[3])
	}
	if len(messages[0].Attachments) != 1 {
		t.Fatal("the stored history must not be mutated")
	}
}

func TestBoundImagesLeavesSmallRequestsAlone(t *testing.T) {
	messages := []model.Message{{
		Role:        model.RoleUser,
		Attachments: []model.Attachment{{Mime: "image/png", Data: "small"}},
	}}
	out := boundImages(messages)
	if len(out[0].Attachments) != 1 || out[0].Content != "" {
		t.Fatalf("small requests should be untouched: %+v", out[0])
	}
}
