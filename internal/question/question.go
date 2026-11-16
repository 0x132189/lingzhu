// Package question asks the user for decisions while a turn is running. The
// tool blocks until an HTTP reply arrives, a client cancels it, or the run is
// interrupted.
package question

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"lingzhu/internal/event"
	"lingzhu/internal/tool"
)

// Option is one selectable answer.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Prompt is one question.
type Prompt struct {
	Question string   `json:"question"`
	Header   string   `json:"header"`
	Options  []Option `json:"options"`
	Multiple bool     `json:"multiple,omitempty"`
}

// Request is a pending question set.
type Request struct {
	ID        string   `json:"id"`
	SessionID string   `json:"sessionId"`
	Questions []Prompt `json:"questions"`
}

// Response is the user's answer or dismissal.
type Response struct {
	Answers   [][]string `json:"answers"`
	Cancelled bool       `json:"cancelled"`
	Message   string     `json:"message,omitempty"`
}

// ErrNotFound is returned for an unknown pending question.
type ErrNotFound struct{ ID string }

func (e ErrNotFound) Error() string { return "question request not found: " + e.ID }

type pending struct {
	request Request
	reply   chan Response
}

// Manager tracks pending questions for every session in one process.
type Manager struct {
	mu      sync.Mutex
	pending map[string]pending
	seq     atomic.Int64
}

// New creates an empty manager.
func New() *Manager {
	return &Manager{pending: map[string]pending{}}
}

// Ask registers a question set, announces it, and waits for a reply.
func (m *Manager) Ask(ctx context.Context, sessionID string, questions []Prompt, announce func(Request) error) (Response, error) {
	if len(questions) == 0 {
		return Response{}, errors.New("question: questions are empty")
	}
	request := Request{
		ID:        fmt.Sprintf("q_%d", m.seq.Add(1)),
		SessionID: sessionID,
		Questions: questions,
	}
	ch := make(chan Response, 1)
	m.mu.Lock()
	m.pending[request.ID] = pending{request: request, reply: ch}
	m.mu.Unlock()
	defer m.release(request.ID)

	if announce != nil {
		if err := announce(request); err != nil {
			return Response{}, err
		}
	}
	select {
	case response := <-ch:
		return response, nil
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// Reply answers a pending question. The returned request lets the caller record
// which session and questions were resolved.
func (m *Manager) Reply(id string, response Response) (Request, error) {
	m.mu.Lock()
	item, ok := m.pending[id]
	m.mu.Unlock()
	if !ok {
		return Request{}, ErrNotFound{ID: id}
	}
	if !response.Cancelled && len(response.Answers) != len(item.request.Questions) {
		return Request{}, fmt.Errorf("question: expected %d answers, got %d", len(item.request.Questions), len(response.Answers))
	}
	select {
	case item.reply <- response:
		return item.request, nil
	default:
		return Request{}, errors.New("question request already answered: " + id)
	}
}

// Get returns one pending request.
func (m *Manager) Get(id string) (Request, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.pending[id]
	if !ok {
		return Request{}, false
	}
	return item.request, true
}

// Pending lists pending questions; an empty session returns all of them.
func (m *Manager) Pending(sessionID string) []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Request, 0, len(m.pending))
	for _, item := range m.pending {
		if sessionID != "" && item.request.SessionID != sessionID {
			continue
		}
		out = append(out, item.request)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *Manager) release(id string) {
	m.mu.Lock()
	delete(m.pending, id)
	m.mu.Unlock()
}

// Tool is the model-facing question tool.
type Tool struct {
	manager *Manager
}

// NewTool returns a question tool backed by manager.
func NewTool(manager *Manager) *Tool { return &Tool{manager: manager} }

func (t *Tool) Name() string { return "question" }

func (t *Tool) Description() string {
	return `Use this tool when you need to ask the user questions during execution. This allows you to:
1. Gather user preferences or requirements
2. Clarify ambiguous instructions
3. Get decisions on implementation choices as you work
4. Offer choices to the user about what direction to take.

Usage notes:
- A "Type your own answer" option is added automatically; don't include a separate option for free form answers
- Set ` + "`multiple: true`" + ` to allow selecting more than one option
- If you recommend a specific option, make that the first option in the list and add "(Recommended)" at the end of the label`
}

func (t *Tool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"questions": {
				"type": "array",
				"description": "Questions to ask",
				"items": {
					"type": "object",
					"properties": {
						"question": {"type": "string", "description": "Complete question"},
						"header": {"type": "string", "description": "Very short label (max 30 chars)"},
						"options": {
							"type": "array",
							"description": "Available choices",
							"items": {
								"type": "object",
								"properties": {
									"label": {"type": "string", "description": "Display text (1-5 words, concise)"},
									"description": {"type": "string", "description": "Explanation of choice"}
								},
								"required": ["label", "description"]
							}
						},
						"multiple": {"type": "boolean", "description": "Allow selecting multiple choices"}
					},
					"required": ["question", "header", "options"]
				}
			}
		},
		"required": ["questions"]
	}`)
}

func (t *Tool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		Questions []Prompt `json:"questions"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("question: %w", err)
	}
	if len(args.Questions) == 0 {
		return "", fmt.Errorf("question: at least one question is required")
	}
	for _, prompt := range args.Questions {
		if strings.TrimSpace(prompt.Question) == "" || strings.TrimSpace(prompt.Header) == "" || len(prompt.Options) == 0 {
			return "", fmt.Errorf("question: each question needs a header, question and options")
		}
	}
	if err := call.Allow(ctx, "question", []string{"*"}, ""); err != nil {
		return "", err
	}
	// Nobody is watching a scheduled wake-up or a subagent turn, so no answer
	// can arrive. Failing immediately keeps the turn moving instead of holding
	// the session open until it is cancelled.
	if call.Unattended {
		return "", errors.New("question: nobody is present to answer")
	}

	var request Request
	response, err := t.manager.Ask(ctx, call.SessionID, args.Questions, func(req Request) error {
		request = req
		if call.Emit == nil {
			return errors.New("question: event emitter is not configured")
		}
		return call.Emit(event.QuestionAskedEvent(req.ID, req.SessionID, req.Questions))
	})
	if err != nil {
		return "", fmt.Errorf("question: %w", err)
	}
	if call.Emit != nil {
		_ = call.Emit(event.QuestionRepliedEvent(request.ID, request.SessionID, response.Answers, response.Cancelled, response.Message))
	}
	if response.Cancelled {
		if message := strings.TrimSpace(response.Message); message != "" {
			return "", errors.New(message)
		}
		return "", errors.New("the user dismissed this question")
	}
	call.SetMetadata("answers", response.Answers)
	return formatAnswers(request.Questions, response.Answers), nil
}

func formatAnswers(questions []Prompt, answers [][]string) string {
	parts := make([]string, 0, len(questions))
	for index, prompt := range questions {
		answer := "Unanswered"
		if index < len(answers) && len(answers[index]) > 0 {
			answer = strings.Join(answers[index], ", ")
		}
		parts = append(parts, fmt.Sprintf("%q=%q", prompt.Question, answer))
	}
	return "User has answered your questions: " + strings.Join(parts, ", ") + ". You can now continue with the user's answers in mind."
}
