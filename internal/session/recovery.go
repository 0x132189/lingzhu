package session

import (
	"fmt"
	"sort"
	"time"

	"lingzhu/internal/event"
)

// QueuedInput is one persisted inbox item: a user prompt, synthetic input, or
// a control item such as a manual compaction or a session move.
type QueuedInput struct {
	ID string
	// Kind is prompt, synthetic, compaction, or move. Older logs omit it and
	// are treated as prompts.
	Kind     string
	Text     string
	Delivery string
	// Data carries the kind-specific payload: attachments and review metadata
	// for prompts, the replayed event for synthetic input, or a move target.
	Data    map[string]any
	Created time.Time
}

// Inbox item kinds. Prompt and synthetic add words to the conversation; the
// other two are controls the turn handles at a boundary.
const (
	KindPrompt     = "prompt"
	KindSynthetic  = "synthetic"
	KindCompaction = "compaction"
	KindMove       = "move"
)

// Recovery is durable work a previous process left behind. ClaimID is empty
// when only inputs are pending; Inputs are already ordered by arrival and mix
// prompts with controls.
type Recovery struct {
	SessionID string
	ClaimID   string
	Inputs    []QueuedInput
	// Attempts counts resumes already recorded for ClaimID, so a crash that
	// keeps hitting the same point is bounded instead of looping forever.
	Attempts int
}

// MaxResumeAttempts bounds how many times one surviving claim is resumed. A
// turn that dies in the same place every time must stop rather than restart
// with each process launch.
const MaxResumeAttempts = 10

// QueueInput persists an input before it can be delivered. The id is written
// into the delivered user message, so replay never delivers it twice.
func (s *Store) QueueInput(id, inputID, text string) (event.Event, error) {
	return s.QueueInputDelivery(id, inputID, text, "steer")
}

// QueueInputDelivery persists an input with its delivery preference.
func (s *Store) QueueInputDelivery(id, inputID, text, delivery string) (event.Event, error) {
	return s.QueueItem(id, QueuedInput{
		ID:       inputID,
		Kind:     KindPrompt,
		Text:     text,
		Delivery: delivery,
	})
}

// QueueItem persists one inbox item and returns the filled event, so a caller
// can publish the same fact to live clients immediately.
func (s *Store) QueueItem(id string, item QueuedInput) (event.Event, error) {
	return s.AppendEvent(id, inboxEvent(item))
}

// inboxEvent builds the durable event for one inbox item.
func inboxEvent(item QueuedInput) event.Event {
	switch item.Kind {
	case KindSynthetic:
		return event.SyntheticQueuedEventWith(item.ID, item.Text, item.Delivery, item.Data)
	case KindCompaction:
		return event.CompactionQueuedEvent(item.ID, item.Delivery)
	case KindMove:
		target, _ := item.Data["target"].(string)
		directory, _ := item.Data["directory"].(string)
		return event.MoveQueuedItemEvent(item.ID, item.Delivery, target, directory)
	default:
		return event.InputQueuedPayloadEvent(item.ID, item.Text, item.Delivery, item.Data)
	}
}

// CancelInput removes one pending input before delivery.
func (s *Store) CancelInput(id, inputID string) (event.Event, error) {
	return s.AppendEvent(id, event.InputCancelledEvent(inputID))
}

// UpdateInputDelivery changes a pending input's delivery preference.
func (s *Store) UpdateInputDelivery(id, inputID, delivery string) (event.Event, error) {
	return s.AppendEvent(id, event.InputUpdatedEvent(inputID, delivery))
}

// FinishTurn releases a completed or failed turn claim.
func (s *Store) FinishTurn(id, turnID string) error {
	_, err := s.Append(id, event.TurnFinishedEvent(turnID))
	return err
}

// ReleaseTurn releases a claim without cancelling pending input. A user
// interruption uses it so steering and queued work stay deliverable.
func (s *Store) ReleaseTurn(id, turnID string) error {
	_, err := s.Append(id, event.TurnReleasedEvent(turnID))
	return err
}

// CancelTurn releases a claim and cancels every input queued before it.
func (s *Store) CancelTurn(id, turnID string) error {
	_, err := s.Append(id, event.TurnCancelledEvent(turnID))
	return err
}

// Recovery returns the durable work left in one session.
func (s *Store) Recovery(id string) (Recovery, error) {
	events, err := s.Events(id)
	if err != nil {
		return Recovery{}, err
	}
	recovery := analyze(events)
	recovery.SessionID = id
	return recovery, nil
}

// Recoveries lists sessions with an open turn claim or undelivered input.
func (s *Store) Recoveries() ([]Recovery, error) {
	rows, err := s.db.Query(`SELECT DISTINCT session_id FROM events ORDER BY session_id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]Recovery, 0, len(ids))
	for _, id := range ids {
		events, err := s.Events(id)
		if err != nil {
			return nil, err
		}
		recovery := analyze(events)
		if recovery.ClaimID == "" && len(recovery.Inputs) == 0 {
			continue
		}
		recovery.SessionID = id
		out = append(out, recovery)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out, nil
}

// InputState reports whether one inbox id was admitted and is still pending. A
// client retry uses it to reconcile instead of admitting the same text twice.
func (s *Store) InputState(id, inputID string) (found bool, pending bool, err error) {
	if inputID == "" {
		return false, false, nil
	}
	events, err := s.Events(id)
	if err != nil {
		return false, false, err
	}
	recovery := analyze(events)
	for _, input := range recovery.Inputs {
		if input.ID == inputID {
			return true, true, nil
		}
	}
	for _, ev := range events {
		switch ev.Type {
		case event.InputQueued, event.SyntheticQueued, event.CompactionQueued, event.MoveQueued:
			if str(ev, "id") == inputID {
				return true, false, nil
			}
		case event.InputCancelled:
			if str(ev, "id") == inputID {
				return true, false, nil
			}
		case event.UserMessage, event.Synthetic, event.JobCompleted, event.SubagentCompleted,
			event.ScheduleFired, event.Compaction, event.CompactionFailed, event.SessionMoved, event.MoveFailed:
			if str(ev, "inputId") == inputID {
				return true, false, nil
			}
		}
	}
	return false, false, nil
}

// analyze folds turn claims and the ordered inbox into the work a restart must
// do. Every kind of pending item lives in one list so ordering survives a
// compaction or move boundary.
func analyze(events []event.Event) Recovery {
	var recovery Recovery
	pending := map[string]QueuedInput{}
	var order []string
	attempts := map[string]int{}

	add := func(input QueuedInput) {
		if input.ID == "" {
			return
		}
		input.Delivery = normalizeDelivery(input.Delivery)
		if _, exists := pending[input.ID]; !exists {
			order = append(order, input.ID)
		}
		pending[input.ID] = input
	}
	remove := func(id string) {
		if id == "" {
			return
		}
		delete(pending, id)
	}
	removeOldest := func(kind string) {
		for _, id := range order {
			if item, ok := pending[id]; ok && item.Kind == kind {
				delete(pending, id)
				return
			}
		}
	}
	removeByInput := func(ev event.Event) {
		if id := str(ev, "inputId"); id != "" {
			remove(id)
			return
		}
		switch ev.Type {
		case event.Compaction, event.CompactionFailed:
			removeOldest(KindCompaction)
		case event.SessionMoved, event.MoveFailed:
			removeOldest(KindMove)
		}
	}

	for _, ev := range events {
		switch ev.Type {
		case event.InputQueued:
			add(QueuedInput{
				ID:       str(ev, "id"),
				Kind:     KindPrompt,
				Text:     str(ev, "text"),
				Delivery: str(ev, "delivery"),
				Data:     mapData(ev, "payload"),
				Created:  ev.At,
			})
		case event.SyntheticQueued:
			add(QueuedInput{
				ID:       str(ev, "id"),
				Kind:     KindSynthetic,
				Text:     str(ev, "text"),
				Delivery: str(ev, "delivery"),
				Data:     mapData(ev, "payload"),
				Created:  ev.At,
			})
		case event.CompactionQueued:
			id := str(ev, "id")
			if id == "" {
				// A log written before control items had an identity keeps one
				// stable id so it can still be consumed by its terminal event.
				id = fmt.Sprintf("cmp_%d", ev.Seq)
			}
			add(QueuedInput{ID: id, Kind: KindCompaction, Delivery: str(ev, "delivery"), Created: ev.At})
		case event.MoveQueued:
			id := str(ev, "id")
			if id == "" {
				id = fmt.Sprintf("mov_%d", ev.Seq)
			}
			data := map[string]any{}
			if target := str(ev, "target"); target != "" {
				data["target"] = target
			}
			if directory := str(ev, "directory"); directory != "" {
				data["directory"] = directory
			}
			add(QueuedInput{ID: id, Kind: KindMove, Delivery: str(ev, "delivery"), Data: data, Created: ev.At})
		case event.InputUpdated:
			if item, ok := pending[str(ev, "id")]; ok {
				item.Delivery = normalizeDelivery(str(ev, "delivery"))
				pending[item.ID] = item
			}
		case event.InputCancelled:
			remove(str(ev, "id"))
		case event.UserMessage, event.Synthetic, event.JobCompleted, event.SubagentCompleted, event.ScheduleFired:
			removeByInput(ev)
		case event.Compaction, event.CompactionFailed, event.SessionMoved, event.MoveFailed:
			removeByInput(ev)
		case event.TurnStarted:
			if id := str(ev, "id"); id != "" {
				recovery.ClaimID = id
			}
		case event.TurnResumed:
			if id := str(ev, "id"); id != "" {
				attempts[id]++
			}
		case event.TurnFinished, event.TurnReleased:
			if id := str(ev, "id"); id != "" && recovery.ClaimID == id {
				recovery.ClaimID = ""
			}
		case event.TurnCancelled:
			if id := str(ev, "id"); id == "" || recovery.ClaimID == id {
				recovery.ClaimID = ""
				pending = map[string]QueuedInput{}
				order = nil
			}
		}
	}
	recovery.Attempts = attempts[recovery.ClaimID]
	for _, id := range order {
		if input, ok := pending[id]; ok {
			recovery.Inputs = append(recovery.Inputs, input)
		}
	}
	return recovery
}

func normalizeDelivery(delivery string) string {
	if delivery == "" {
		return "steer"
	}
	return delivery
}

func mapData(ev event.Event, key string) map[string]any {
	value, _ := ev.Data[key].(map[string]any)
	if value == nil {
		return nil
	}
	out := make(map[string]any, len(value))
	for k, v := range value {
		out[k] = v
	}
	return out
}

// SettleTools appends a failed result for every tool call after the latest
// compaction that never received one, so a resumed request is valid.
func (s *Store) SettleTools(id string) error {
	events, err := s.Events(id)
	if err != nil {
		return err
	}
	type call struct {
		id   string
		name string
	}
	var calls []call
	settled := map[string]bool{}
	for _, ev := range events {
		switch ev.Type {
		case event.Compaction:
			calls = nil
			settled = map[string]bool{}
		case event.ToolCall:
			calls = append(calls, call{id: str(ev, "id"), name: str(ev, "name")})
		case event.ToolResult:
			settled[str(ev, "id")] = true
		}
	}
	for _, call := range calls {
		if call.id == "" || settled[call.id] {
			continue
		}
		if _, err := s.Append(id, event.ToolResultEvent(call.id, call.name, "Tool execution interrupted", true)); err != nil {
			return err
		}
	}
	return nil
}
