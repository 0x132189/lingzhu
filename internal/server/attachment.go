package server

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"lingzhu/internal/media"
	"lingzhu/internal/model"
)

const (
	maxPromptAttachments     = 8
	maxPromptAttachmentBytes = 20 << 20
	maxPromptAttachmentTotal = 20 << 20
)

// normalizeAttachments validates inline prompt media and returns canonical
// values. Image attachments are resized before the size limits are applied.
func normalizeAttachments(items []model.Attachment, imageConfig media.Config) ([]model.Attachment, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if len(items) > maxPromptAttachments {
		return nil, fmt.Errorf("at most %d attachments are allowed", maxPromptAttachments)
	}
	normalized := make([]model.Attachment, 0, len(items))
	total := 0
	for _, item := range items {
		mime := strings.ToLower(strings.TrimSpace(item.Mime))
		if !isPromptAttachmentMime(mime) {
			return nil, fmt.Errorf("unsupported attachment type %q", item.Mime)
		}
		kind := strings.ToLower(strings.TrimSpace(item.Kind))
		switch kind {
		case "", "file":
			if isTextAttachmentMime(mime) {
				kind = "text"
			} else {
				kind = "file"
			}
		case "text":
			if !isTextAttachmentMime(mime) {
				return nil, fmt.Errorf("attachment %q has a text kind with non-text type %q", item.Name, mime)
			}
		case "directory":
			if !isTextAttachmentMime(mime) {
				return nil, fmt.Errorf("directory attachment %q must use a text type", item.Name)
			}
			if item.Start != 0 || item.End != 0 {
				return nil, fmt.Errorf("directory attachment %q cannot have a line range", item.Name)
			}
		default:
			return nil, fmt.Errorf("unsupported attachment kind %q", item.Kind)
		}
		if item.Start != 0 || item.End != 0 {
			if !isTextAttachmentMime(mime) || kind == "directory" {
				return nil, fmt.Errorf("attachment %q cannot have a line range", item.Name)
			}
			if item.Start < 1 || item.End < item.Start {
				return nil, fmt.Errorf("attachment %q has an invalid line range", item.Name)
			}
			kind = "text"
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = defaultAttachmentName(mime)
		}
		if runes := []rune(name); len(runes) > 200 {
			name = string(runes[:200])
		}
		data := strings.TrimSpace(item.Data)
		if data == "" {
			return nil, fmt.Errorf("attachment %q is empty", name)
		}
		raw, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("attachment %q is not valid base64", name)
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("attachment %q is empty", name)
		}
		if len(raw) > maxPromptAttachmentBytes {
			return nil, fmt.Errorf("attachment %q is larger than %d MB", name, maxPromptAttachmentBytes>>20)
		}
		if strings.HasPrefix(mime, "image/") {
			processed, nextMime, err := media.Process(raw, mime, imageConfig)
			if err != nil {
				return nil, fmt.Errorf("attachment %q: %w", name, err)
			}
			raw = processed
			mime = nextMime
			data = base64.StdEncoding.EncodeToString(raw)
		}
		if isTextAttachmentMime(mime) {
			if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
				return nil, fmt.Errorf("attachment %q is not UTF-8 text", name)
			}
		}
		total += len(raw)
		if total > maxPromptAttachmentTotal {
			return nil, fmt.Errorf("attachments are larger than %d MB in total", maxPromptAttachmentTotal>>20)
		}
		normalized = append(normalized, model.Attachment{
			Mime:  mime,
			Name:  name,
			Data:  data,
			Kind:  kind,
			Start: item.Start,
			End:   item.End,
		})
	}
	return normalized, nil
}

func isPromptAttachmentMime(mime string) bool {
	return strings.HasPrefix(mime, "image/") || mime == "application/pdf" || isTextAttachmentMime(mime)
}

func hasMediaAttachment(items []model.Attachment) bool {
	for _, item := range items {
		mime := strings.ToLower(strings.TrimSpace(item.Mime))
		if strings.HasPrefix(mime, "image/") || mime == "application/pdf" {
			return true
		}
	}
	return false
}

// isTextAttachmentMime reports whether bytes can be inlined into the prompt as
// text rather than provider media.
func isTextAttachmentMime(mime string) bool {
	return model.IsTextMime(mime)
}

func defaultAttachmentName(mime string) string {
	switch mime {
	case "application/pdf":
		return "document.pdf"
	case "image/png":
		return "image.png"
	case "image/jpeg":
		return "image.jpg"
	case "image/gif":
		return "image.gif"
	case "image/webp":
		return "image.webp"
	case "application/json":
		return "attachment.json"
	case "application/xml":
		return "attachment.xml"
	case "application/yaml", "application/x-yaml":
		return "attachment.yaml"
	default:
		return "attachment.txt"
	}
}

var errAttachmentsUnsupported = errors.New("the selected model does not accept attachments")

// publicMessages drops inline media bytes from API responses. Clients only
// need mime and name; the bytes stay in the event log for the next request.
func publicMessages(messages []model.Message) []model.Message {
	out := make([]model.Message, len(messages))
	copy(out, messages)
	for index := range out {
		if len(out[index].Attachments) == 0 {
			continue
		}
		attachments := make([]model.Attachment, len(out[index].Attachments))
		for position, attachment := range out[index].Attachments {
			attachments[position] = model.Attachment{
				Mime:  attachment.Mime,
				Name:  attachment.Name,
				Kind:  attachment.Kind,
				Start: attachment.Start,
				End:   attachment.End,
			}
		}
		out[index].Attachments = attachments
	}
	return out
}
