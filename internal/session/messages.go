package session

import (
	"encoding/json"
	"strings"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
)

// Messages rebuilds the conversation from the event log for the next request.
func (s *Store) Messages(id string) ([]model.Message, error) {
	events, err := s.Events(id)
	if err != nil {
		return nil, err
	}
	return BuildMessages(events), nil
}

// MessageRecord is one rebuilt message with the durable time it entered the
// conversation. The time is API/UI metadata; provider requests keep using the
// plain model.Message.
type MessageRecord struct {
	model.Message
	Created time.Time
	// Agent, Provider and Model are recorded on user turns so history can show
	// the same footer meta the live turn showed.
	Agent    string
	Provider string
	Model    string
	// Compaction marks a checkpoint that replaced earlier turns.
	Compaction        bool
	CompactionCovered int
	CompactionReason  string
}

type messageMeta struct {
	agent             string
	provider          string
	model             string
	compaction        bool
	compactionCovered int
	compactionReason  string
}

// MessageRecords rebuilds the conversation with one creation time per message.
func (s *Store) MessageRecords(id string) ([]MessageRecord, error) {
	events, err := s.Events(id)
	if err != nil {
		return nil, err
	}
	return BuildMessageRecords(events), nil
}

func BuildMessageRecords(events []event.Event) []MessageRecord {
	messages, times, metas := buildWithTimes(events, nil, true)
	records := make([]MessageRecord, len(messages))
	for index := range messages {
		records[index] = MessageRecord{
			Message:           messages[index],
			Created:           times[index],
			Agent:             metas[index].agent,
			Provider:          metas[index].provider,
			Model:             metas[index].model,
			Compaction:        metas[index].compaction,
			CompactionCovered: metas[index].compactionCovered,
			CompactionReason:  metas[index].compactionReason,
		}
	}
	return records
}

func BuildMessages(events []event.Event) []model.Message {
	messages, _, _ := buildWithTimes(events, nil, false)
	out := make([]model.Message, 0, len(messages))
	for _, message := range messages {
		if message.Switch != nil || message.Failure != nil {
			continue
		}
		out = append(out, message)
	}
	return out
}

// visible drops the turns hidden by every revert that has not been reapplied.
// Each revert hides the half-open sequence range from its user turn to the
// revert event; a later reapply pops the most recent range.
func visible(events []event.Event) []event.Event {
	var ranges [][2]int64
	for _, ev := range events {
		switch ev.Type {
		case event.SessionReverted:
			if turnSeq := eventInt(ev, "turnSeq"); turnSeq > 0 {
				ranges = append(ranges, [2]int64{turnSeq, ev.Seq})
			}
		case event.SessionReapplied:
			if len(ranges) > 0 {
				ranges = ranges[:len(ranges)-1]
			}
		}
	}
	if len(ranges) == 0 {
		return events
	}
	out := make([]event.Event, 0, len(events))
	for _, ev := range events {
		hidden := false
		for _, span := range ranges {
			if ev.Seq >= span[0] && ev.Seq <= span[1] {
				hidden = true
				break
			}
		}
		if !hidden {
			out = append(out, ev)
		}
	}
	return out
}

func eventInt(ev event.Event, key string) int64 {
	switch value := ev.Data[key].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	case json.Number:
		n, _ := value.Int64()
		return n
	}
	return 0
}

func eventBool(ev event.Event, key string) bool {
	value, _ := ev.Data[key].(bool)
	return value
}

// switchPrevious reads one field of the previous selection a model switch
// records, tolerating a missing or decoded map.
func switchPrevious(previous map[string]any, key string) string {
	if previous == nil {
		return ""
	}
	value, _ := previous[key].(string)
	return value
}

// stringSlice reads a JSON string array, tolerating []any from decoded events.
func stringSlice(ev event.Event, key string) []string {
	values, ok := ev.Data[key].([]string)
	if ok {
		return values
	}
	items, ok := ev.Data[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// syntheticNotice names what produced a synthetic turn for the transcript's
// notice row. The core's stored text stays model-facing. id identifies the job
// or child session behind the notice, so a transcript can tell two background
// reports apart instead of collapsing them into one row.
func syntheticNotice(ev event.Event) (source, description, state, id string, sources []string) {
	state = "completed"
	switch ev.Type {
	case event.JobCompleted:
		source = "shell"
		description = str(ev, "command")
		id = str(ev, "id")
		switch str(ev, "status") {
		case "failed":
			state = "error"
		case "cancelled":
			state = "cancelled"
		}
	case event.SubagentCompleted:
		source = "subagent"
		description = str(ev, "description")
		id = str(ev, "id")
		switch str(ev, "status") {
		case "failed", "error":
			state = "error"
		case "cancelled":
			state = "cancelled"
		}
	case event.ScheduleFired:
		source = "schedule"
		description = str(ev, "name")
		id = str(ev, "id")
	case event.FormReplied:
		source = "form"
		description = str(ev, "title")
		id = str(ev, "id")
	case event.Synthetic:
		source = str(ev, "source")
		if source == "" {
			source = "synthetic"
		}
		description = str(ev, "description")
		id = str(ev, "id")
		sources = stringSlice(ev, "sources")
		// An instruction update is informational, not a completed task.
		if source == "instructions" {
			state = ""
		}
	}
	return source, description, state, id, sources
}

// shellContext is the user-turn text a shell result contributes to the model.
func shellContext(command, output string) string {
	return "The following shell command was executed by the user:\n\nCommand:\n" +
		command + "\n\nOutput:\n" + output
}

// build reconstructs the model-facing messages and reports, after each event,
// how many messages exist. Compaction events replace everything before them
// with one checkpoint. Switch rows are transcript-only, so the model never
// receives them.
func build(events []event.Event, observe func(messages int, ev event.Event)) []model.Message {
	messages, _, _ := buildWithTimes(events, observe, false)
	out := make([]model.Message, 0, len(messages))
	for _, message := range messages {
		if message.Switch != nil || message.Failure != nil {
			continue
		}
		out = append(out, message)
	}
	return out
}

// buildWithTimes mirrors build while recording the durable time and user-turn
// meta of each message. Tool-call events extend the preceding assistant
// message and add no time. A transcript build keeps every turn and renders a
// compaction as one divider row; only a model build restarts at its checkpoint.
func buildWithTimes(events []event.Event, observe func(messages int, ev event.Event), transcript bool) ([]model.Message, []time.Time, []messageMeta) {
	events = visible(events)
	// Instruction updates before the latest checkpoint are stale: the checkpoint
	// reset the instruction baseline, so the transcript stops showing them while
	// it keeps the rest of the earlier turns.
	var epoch int64
	for _, ev := range events {
		if ev.Type == event.Compaction {
			epoch = ev.Seq
		}
	}
	messages := make([]model.Message, 0, len(events))
	times := make([]time.Time, 0, len(events))
	metas := make([]messageMeta, 0, len(events))
	// Switch rows are transcript-only, so the count the observer sees excludes
	// them and matches the messages a provider would receive.
	modelMessages := 0
	appendMessage := func(message model.Message, at time.Time, meta messageMeta) {
		messages = append(messages, message)
		times = append(times, at)
		metas = append(metas, meta)
		// Switch and failure rows are transcript-only, so the count a reader
		// sees matches the messages a provider would receive.
		if message.Switch == nil && message.Failure == nil {
			modelMessages++
		}
	}
	for _, ev := range events {
		if transcript && ev.Seq < epoch && ev.Type == event.Synthetic && str(ev, "source") == "instructions" {
			continue
		}
		switch ev.Type {
		case event.Compaction:
			checkpoint := model.Message{Role: model.RoleUser, Content: str(ev, "checkpoint")}
			meta := messageMeta{compaction: true, compactionCovered: int(eventInt(ev, "covered")), compactionReason: str(ev, "reason")}
			if transcript {
				// The transcript keeps every turn; the checkpoint is one divider
				// row between them, and only the model request restarts from it.
				messages = append(messages, checkpoint)
				times = append(times, ev.At)
				metas = append(metas, meta)
				break
			}
			messages = []model.Message{checkpoint}
			times = []time.Time{ev.At}
			metas = []messageMeta{meta}
			modelMessages = 1
		case event.UserMessage:
			appendMessage(
				model.Message{
					Role:        model.RoleUser,
					Content:     text(ev),
					Attachments: attachmentsOf(ev),
					Comments:    commentsOf(ev),
					Voice:       eventBool(ev, "voice"),
					References:  referencesOf(ev),
				},
				ev.At,
				messageMeta{agent: str(ev, "agent"), provider: str(ev, "provider"), model: str(ev, "model")},
			)
		case event.JobCompleted, event.SubagentCompleted, event.ScheduleFired, event.Synthetic, event.FormReplied:
			if notice := str(ev, "text"); notice != "" {
				source, description, state, id, sources := syntheticNotice(ev)
				appendMessage(model.Message{
					Role:                 model.RoleUser,
					Content:              notice,
					Synthetic:            true,
					SyntheticSource:      source,
					SyntheticDescription: description,
					SyntheticState:       state,
					SyntheticID:          id,
					SyntheticSources:     sources,
					SyntheticPaths:       stringSlice(ev, "paths"),
				}, ev.At, messageMeta{})
			}

		case event.ShellEnded:
			command := str(ev, "command")
			output := str(ev, "output")
			appendMessage(
				model.Message{
					Role:    model.RoleUser,
					Content: shellContext(command, output),
					Shell: &model.ShellRun{
						Command:   command,
						Output:    output,
						Exit:      int(eventInt(ev, "exit")),
						TimedOut:  eventBool(ev, "timedOut"),
						Truncated: eventBool(ev, "truncated"),
					},
				},
				ev.At,
				messageMeta{},
			)
		case event.AssistantMessage:
			// Tool calls may be announced while the assistant response is
			// still streaming, so the placeholder message can already exist.
			// Merge the streamed text into it instead of appending a second
			// assistant message after the tool calls.
			if len(messages) > 0 && messages[len(messages)-1].Role == model.RoleAssistant &&
				strings.TrimSpace(messages[len(messages)-1].Content) == "" && len(messages[len(messages)-1].ToolCalls) > 0 {
				last := &messages[len(messages)-1]
				last.Content = text(ev)
				last.Reasoning = str(ev, "reasoning")
				last.ReasoningCreated = str(ev, "reasoningCreated")
				last.ReasoningCompleted = str(ev, "reasoningCompleted")
				last.Parts = partsOf(ev)
				applyStepMetrics(last, ev)
				times[len(messages)-1] = ev.At
				break
			}
			message := model.Message{
				Role:               model.RoleAssistant,
				Content:            text(ev),
				Reasoning:          str(ev, "reasoning"),
				ReasoningCreated:   str(ev, "reasoningCreated"),
				ReasoningCompleted: str(ev, "reasoningCompleted"),
				Parts:              partsOf(ev),
			}
			applyStepMetrics(&message, ev)
			appendMessage(message, ev.At, messageMeta{})
		case event.ToolCall:
			if len(messages) == 0 || messages[len(messages)-1].Role != model.RoleAssistant {
				appendMessage(model.Message{Role: model.RoleAssistant}, ev.At, messageMeta{})
			}
			last := &messages[len(messages)-1]
			last.ToolCalls = append(last.ToolCalls, model.ToolCall{
				ID:        str(ev, "id"),
				Name:      str(ev, "name"),
				Arguments: str(ev, "arguments"),
			})
		case event.ToolResult:
			appendMessage(model.Message{
				Role:        model.RoleTool,
				Content:     str(ev, "output"),
				ToolCallID:  str(ev, "id"),
				Name:        str(ev, "name"),
				Diff:        str(ev, "diff"),
				Failed:      eventBool(ev, "failed"),
				Cancelled:   eventBool(ev, "cancelled"),
				Metadata:    metadataOf(ev),
				Attachments: attachmentsOf(ev),
			}, ev.At, messageMeta{})
		case event.AgentSelected:
			appendMessage(model.Message{
				Role: model.RoleUser,
				Switch: &model.Switch{
					Kind:     "agent",
					Agent:    str(ev, "agent"),
					Previous: str(ev, "previous"),
				},
			}, ev.At, messageMeta{})
		case event.ModelSelected:
			previous, _ := ev.Data["previous"].(map[string]any)
			appendMessage(model.Message{
				Role: model.RoleUser,
				Switch: &model.Switch{
					Kind:             "model",
					Provider:         str(ev, "provider"),
					Model:            str(ev, "model"),
					PreviousProvider: switchPrevious(previous, "provider"),
					PreviousModel:    switchPrevious(previous, "model"),
				},
			}, ev.At, messageMeta{})
		case event.RunFailed:
			// A failed turn is a transcript row, not a model message: the next
			// request must not replay the failure as conversation.
			appendMessage(model.Message{
				Role:    model.RoleAssistant,
				Failure: &model.Failure{Message: str(ev, "message")},
			}, ev.At, messageMeta{})
		}
		if observe != nil {
			observe(modelMessages, ev)
		}
	}
	return messages, times, metas
}

// applyStepMetrics copies one step's timing and token counts onto its message.
// The fields are UI metadata; requests keep sending only content and tool calls.
func applyStepMetrics(message *model.Message, ev event.Event) {
	message.Started = str(ev, "started")
	message.FirstToken = str(ev, "firstToken")
	message.Streamed = str(ev, "streamed")
	message.Completed = str(ev, "completed")
	message.OutputTokens = int(eventInt(ev, "outputTokens"))
	message.ReasoningTokens = int(eventInt(ev, "reasoningTokens"))
}

// partsOf decodes the ordered reasoning/text segments of one assistant step.
func partsOf(ev event.Event) []model.MessagePart {
	value, ok := ev.Data["parts"]
	if !ok || value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var parts []model.MessagePart
	if json.Unmarshal(data, &parts) != nil {
		return nil
	}
	return parts
}

// metadataOf returns the UI-only fields stored on a tool-result event.
func metadataOf(ev event.Event) map[string]any {
	value, ok := ev.Data["metadata"]
	if !ok || value == nil {
		return nil
	}
	if fields, ok := value.(map[string]any); ok {
		return fields
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	fields := map[string]any{}
	if json.Unmarshal(data, &fields) != nil {
		return nil
	}
	return fields
}

// commentsOf decodes structured review comments stored on a user event.
func commentsOf(ev event.Event) []model.Comment {
	raw, ok := ev.Data["comments"]
	if !ok || raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var comments []model.Comment
	if json.Unmarshal(data, &comments) != nil {
		return nil
	}
	return comments
}

// referencesOf decodes server-side file references stored on a user event.
func referencesOf(ev event.Event) []model.Reference {
	raw, ok := ev.Data["references"]
	if !ok || raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var references []model.Reference
	if json.Unmarshal(data, &references) != nil {
		return nil
	}
	return references
}

// attachmentsOf decodes inline media stored on user and tool-result events.
func attachmentsOf(ev event.Event) []model.Attachment {
	raw, ok := ev.Data["attachments"]
	if !ok || raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var attachments []model.Attachment
	if err := json.Unmarshal(data, &attachments); err != nil {
		return nil
	}
	return attachments
}
