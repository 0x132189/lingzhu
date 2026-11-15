package agent

import (
	"strings"

	"lingzhu/internal/model"
)

const (
	// imageBytesTrigger starts dropping the oldest images at this inline size.
	imageBytesTrigger = 25 * 1024 * 1024
	// imageBytesTarget is the size the request is trimmed back to.
	imageBytesTarget = 15 * 1024 * 1024
	// imageRemoved replaces an image the request dropped.
	imageRemoved = "[This image was removed to reduce the request size and is no longer visible. Do not make claims about its contents from memory. If needed, retrieve it again with an available tool or ask the user to attach it again.]"
)

// boundImages drops the oldest inline images when their combined size passes
// the reference limit. The replacement note keeps the model from claiming to
// remember an image it can no longer see. PDFs and text attachments stay.
func boundImages(messages []model.Message) []model.Message {
	type ref struct {
		message int
		index   int
		size    int
	}
	refs := make([]ref, 0)
	total := 0
	for messageIndex, message := range messages {
		for attachmentIndex, attachment := range message.Attachments {
			if !strings.HasPrefix(strings.ToLower(attachment.Mime), "image/") || attachment.Data == "" {
				continue
			}
			size := len(attachment.Data)
			refs = append(refs, ref{message: messageIndex, index: attachmentIndex, size: size})
			total += size
		}
	}
	if total <= imageBytesTrigger {
		return messages
	}

	drop := map[int]map[int]bool{}
	for _, item := range refs {
		if total <= imageBytesTarget {
			break
		}
		if drop[item.message] == nil {
			drop[item.message] = map[int]bool{}
		}
		drop[item.message][item.index] = true
		total -= item.size
	}
	if len(drop) == 0 {
		return messages
	}

	out := make([]model.Message, len(messages))
	copy(out, messages)
	for messageIndex, indexes := range drop {
		kept := make([]model.Attachment, 0, len(out[messageIndex].Attachments)-len(indexes))
		for attachmentIndex, attachment := range out[messageIndex].Attachments {
			if indexes[attachmentIndex] {
				continue
			}
			kept = append(kept, attachment)
		}
		out[messageIndex].Attachments = kept
		if strings.TrimSpace(out[messageIndex].Content) == "" {
			out[messageIndex].Content = imageRemoved
		} else {
			out[messageIndex].Content = strings.TrimRight(out[messageIndex].Content, "\n") + "\n\n" + imageRemoved
		}
	}
	return out
}
