package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
)

// maxToolOutput caps one tool result inside a summary transcript.
const maxToolOutput = 2000

const compactionPrompt = `Summarize the conversation above so another agent can resume the work. Reply with exactly these sections, using terse single-line bullets and "(none)" for empty ones:

## Objective
## Decisions
## Work State
## Next Move
## Relevant Files

Rules:
- Preserve exact file paths, symbols, commands and error strings.
- Carry forward only requests that are still open; drop resolved ones.
- Do not describe this summary or mention that context was compacted.
- Do not call tools. Return only the summary.`

// compact summarizes everything before the kept tail and records a checkpoint.
// keep is the token budget of the tail left verbatim; zero keeps only the last
// exchange.
func (a *Agent) compact(ctx context.Context, sessionID string, target provider.Target, current session.Context, keep int) error {
	split := keepTail(current.Messages, keep)
	if split <= 0 {
		return errors.New("compact: nothing to compact yet")
	}
	if err := a.emit(sessionID, event.CompactionStartEvent()); err != nil {
		return err
	}

	summary, usage, err := a.summarize(ctx, target, current.Messages[:split])
	if err != nil {
		return fmt.Errorf("compact: %w", err)
	}
	if strings.TrimSpace(summary) == "" {
		return errors.New("compact: model returned an empty summary")
	}
	if usage != nil {
		if err := a.emit(sessionID, event.UsageEvent(usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite)); err != nil {
			return err
		}
	}

	recent := transcript(current.Messages[split:], 0)
	return a.emit(sessionID, event.CompactionEvent(session.Checkpoint(summary, recent), split))
}

// summarize runs one model call that writes the structured summary.
func (a *Agent) summarize(ctx context.Context, target provider.Target, history []model.Message) (string, *provider.Usage, error) {
	budget := session.Ceiling(target.Window)
	overhead := session.Estimate(a.system()) + session.Estimate(compactionPrompt)
	text := transcript(history, 0)
	if budget > 0 && session.Estimate(text)+overhead > budget {
		text = transcript(history, max(budget-overhead-1000, 1000))
	}
	if strings.TrimSpace(text) == "" {
		return "", nil, errors.New("history is empty")
	}

	messages := []model.Message{{Role: model.RoleUser, Content: text + "\n\n" + compactionPrompt}}
	stream, err := target.Provider.Stream(ctx, model.Request{Model: target.Model, Messages: a.withSystem(messages)})
	if err != nil {
		return "", nil, err
	}
	defer stream.Close()

	var summary strings.Builder
	var usage *provider.Usage
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", usage, err
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		summary.WriteString(chunk.Text)
	}
	return strings.TrimSpace(summary.String()), usage, nil
}

// keepTail returns the index where the verbatim tail starts: the earliest user
// message whose tail still fits keep tokens, else the earliest message that
// fits, else only the newest message stays out of the summary.
func keepTail(messages []model.Message, keep int) int {
	user, any, total := -1, -1, 0
	for i := len(messages) - 1; i >= 0; i-- {
		total += session.Estimate(messages[i].Content)
		if total > keep {
			continue
		}
		any = i
		if messages[i].Role == model.RoleUser {
			user = i
		}
	}
	if user > 0 {
		return user
	}
	if any > 0 {
		return any
	}
	if len(messages) > 1 {
		return len(messages) - 1
	}
	return 0
}

// transcript renders messages as plain text, dropping whole exchanges from the
// oldest end until the text fits budget. Budget zero keeps everything.
func transcript(messages []model.Message, budget int) string {
	blocks := make([]string, 0, len(messages))
	for _, group := range exchanges(messages) {
		blocks = append(blocks, renderGroup(group))
	}
	total := 0
	for _, block := range blocks {
		total += session.Estimate(block)
	}
	drop := 0
	for budget > 0 && total > budget && drop < len(blocks)-1 {
		total -= session.Estimate(blocks[drop])
		drop++
	}

	var b strings.Builder
	if drop > 0 {
		fmt.Fprintf(&b, "[%d older exchanges omitted]\n\n", drop)
	}
	for i, block := range blocks[drop:] {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(block)
	}
	return b.String()
}

// exchanges groups messages into turns, each starting at a user message.
func exchanges(messages []model.Message) [][]model.Message {
	var groups [][]model.Message
	for _, message := range messages {
		if message.Role == model.RoleUser || len(groups) == 0 {
			groups = append(groups, nil)
		}
		last := len(groups) - 1
		groups[last] = append(groups[last], message)
	}
	return groups
}

func renderGroup(messages []model.Message) string {
	lines := make([]string, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case model.RoleUser:
			lines = append(lines, "[User]: "+message.Content)
		case model.RoleAssistant:
			if message.Content != "" {
				lines = append(lines, "[Assistant]: "+message.Content)
			}
			for _, call := range message.ToolCalls {
				lines = append(lines, fmt.Sprintf("[Assistant tool call]: %s(%s)", call.Name, clip(call.Arguments)))
			}
		case model.RoleTool:
			lines = append(lines, fmt.Sprintf("[Tool %s result]: %s", message.Name, clip(message.Content)))
		}
	}
	return strings.Join(lines, "\n")
}

// clip keeps the start and end of long text, dropping the middle. It counts
// runes so multi-byte characters are never split.
func clip(text string) string {
	runes := []rune(text)
	if len(runes) <= maxToolOutput {
		return text
	}
	half := maxToolOutput / 2
	return string(runes[:half]) + fmt.Sprintf("\n[… %d chars omitted …]\n", len(runes)-maxToolOutput) + string(runes[len(runes)-half:])
}
