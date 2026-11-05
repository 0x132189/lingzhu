package session

import (
	"sort"

	"lingzhu/internal/event"
)

// QueuedInput is one persisted steering item.
type QueuedInput struct {
	ID   string
	Text string
}

// Recovery is durable work a previous process left behind. ClaimID is empty
// when only inputs are pending; Inputs are already ordered by arrival.
type Recovery struct {
	SessionID string
	ClaimID   string
	Inputs    []QueuedInput
}

// QueueInput persists an input before it can be delivered. The id is written
// into the delivered user message, so replay never delivers it twice.
func (s *Store) QueueInput(id, inputID, text string) (int64, error) {
	return s.Append(id, event.InputQueuedEvent(inputID, text))
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
		if recovery.ClaimID == "" && len(recovery.Inputs) == 0 {
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
	for _, ev := range events {
		switch ev.Type {
		case event.InputQueued:
			id := str(ev, "id")
			if id == "" {
				continue
			}
			if _, exists := pending[id]; !exists {
				order = append(order, id)
			}
			pending[id] = QueuedInput{ID: id, Text: str(ev, "text")}
		case event.UserMessage:
			if id := str(ev, "inputId"); id != "" {
				delete(pending, id)
			}
		case event.TurnStarted:
			if id := str(ev, "id"); id != "" {
				recovery.ClaimID = id
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
			}
		}
	}
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
