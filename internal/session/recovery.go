package session

import (
	"sort"
	"time"

	"lingzhu/internal/event"
)

// QueuedInput is one persisted steering item.
type QueuedInput struct {
	ID       string
	Text     string
	Delivery string
	Created  time.Time
}

// Recovery is durable work a previous process left behind. ClaimID is empty
// when only inputs are pending; Inputs are already ordered by arrival.
type Recovery struct {
	SessionID string
	ClaimID   string
	Compact   bool
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
func (s *Store) QueueInput(id, inputID, text string) (int64, error) {
	return s.QueueInputDelivery(id, inputID, text, "steer")
}

// QueueInputDelivery persists an input with its delivery preference.
func (s *Store) QueueInputDelivery(id, inputID, text, delivery string) (int64, error) {
	return s.Append(id, event.InputQueuedDeliveryEvent(inputID, text, delivery))
}

// CancelInput removes one pending input before delivery.
func (s *Store) CancelInput(id, inputID string) (int64, error) {
	return s.Append(id, event.InputCancelledEvent(inputID))
}

// UpdateInputDelivery changes a pending input's delivery preference.
func (s *Store) UpdateInputDelivery(id, inputID, delivery string) error {
	_, err := s.Append(id, event.InputUpdatedEvent(inputID, delivery))
	return err
}

// FinishTurn releases a completed or failed turn claim.
func (s *Store) FinishTurn(id, turnID string) error {
	_, err := s.Append(id, event.TurnFinishedEvent(turnID))
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
		if recovery.ClaimID == "" && len(recovery.Inputs) == 0 && !recovery.Compact {
			continue
		}
		recovery.SessionID = id
		out = append(out, recovery)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out, nil
}

// analyze folds turn claims and queued input into the work a restart must do.
func analyze(events []event.Event) Recovery {
	var recovery Recovery
	pending := map[string]QueuedInput{}
	var order []string
	compact := false
	attempts := map[string]int{}
	for _, ev := range events {
		switch ev.Type {
		case event.CompactionQueued:
			compact = true
		case event.Compaction, event.CompactionFailed:
			compact = false
		case event.InputQueued:
			id := str(ev, "id")
			if id == "" {
				continue
			}
			if _, exists := pending[id]; !exists {
				order = append(order, id)
			}
			delivery := str(ev, "delivery")
			if delivery == "" {
				delivery = "steer"
			}
			pending[id] = QueuedInput{ID: id, Text: str(ev, "text"), Delivery: delivery, Created: ev.At}
		case event.InputUpdated:
			id := str(ev, "id")
			if item, ok := pending[id]; ok {
				item.Delivery = str(ev, "delivery")
				pending[id] = item
			}
		case event.InputCancelled:
			delete(pending, str(ev, "id"))
		case event.UserMessage:
			if id := str(ev, "inputId"); id != "" {
				delete(pending, id)
			}
		case event.TurnStarted:
			if id := str(ev, "id"); id != "" {
				recovery.ClaimID = id
			}
		case event.TurnResumed:
			if id := str(ev, "id"); id != "" {
				attempts[id]++
			}
		case event.TurnFinished:
			if id := str(ev, "id"); id != "" && recovery.ClaimID == id {
				recovery.ClaimID = ""
			}
		case event.TurnCancelled:
			if id := str(ev, "id"); id == "" || recovery.ClaimID == id {
				recovery.ClaimID = ""
				pending = map[string]QueuedInput{}
				order = nil
				compact = false
			}
		}
	}
	recovery.Compact = compact
	recovery.Attempts = attempts[recovery.ClaimID]
	for _, id := range order {
		if input, ok := pending[id]; ok {
			recovery.Inputs = append(recovery.Inputs, input)
		}
	}
	return recovery
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
