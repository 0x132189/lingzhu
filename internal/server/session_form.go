package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"lingzhu/internal/event"
)

type formRecord struct {
	ID         string            `json:"id"`
	SessionID  string            `json:"sessionID"`
	Title      string            `json:"title"`
	Fields     []event.FormField `json:"fields"`
	Status     string            `json:"status"`
	AskedAt    time.Time         `json:"askedAt"`
	AnsweredAt *time.Time        `json:"answeredAt,omitempty"`
	Values     map[string]any    `json:"values,omitempty"`
	Cancelled  bool              `json:"cancelled,omitempty"`
}

// listForms returns the session's forms, newest first. status=pending filters
// out answered and cancelled requests.
func (s *Server) listForms(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	forms, err := s.forms(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" && status != "all" {
		filtered := make([]formRecord, 0, len(forms))
		for _, form := range forms {
			if form.Status == status {
				filtered = append(filtered, form)
			}
		}
		forms = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"forms": forms})
}

// createForm records a structured form request for the session.
func (s *Server) createForm(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	var body struct {
		Title  string            `json:"title"`
		Fields []event.FormField `json:"fields"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	fields, err := normalizeFormFields(body.Fields)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = "Form"
	}
	formID := fmt.Sprintf("form_%d", time.Now().UnixNano())
	asked := event.FormAskedEvent(formID, title, fields)
	if _, err := s.store.Append(id, asked); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, formRecord{
		ID:        formID,
		SessionID: id,
		Title:     title,
		Fields:    fields,
		Status:    "pending",
		AskedAt:   asked.At,
	})
}

// replyForm answers or cancels a pending form. The answer is persisted and
// delivered to the model as a synthetic user message.
func (s *Server) replyForm(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	formID := r.PathValue("formID")
	var body struct {
		Values    map[string]any `json:"values"`
		Cancelled bool           `json:"cancelled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	forms, err := s.forms(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var target *formRecord
	for index := range forms {
		if forms[index].ID == formID {
			target = &forms[index]
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, errText("form not found"))
		return
	}
	if target.Status != "pending" {
		writeError(w, http.StatusConflict, errText("form is already answered"))
		return
	}
	if !body.Cancelled {
		if err := validateFormValues(*target, body.Values); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	text := formReplyText(*target, body.Values, body.Cancelled)
	replied := event.FormRepliedEvent(target.ID, target.Title, text, body.Values, body.Cancelled)
	if err := s.agentFor(id).Notify(id, replied); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	answered := replied.At
	target.Status = "answered"
	if body.Cancelled {
		target.Status = "cancelled"
	}
	target.AnsweredAt = &answered
	target.Values = body.Values
	target.Cancelled = body.Cancelled
	writeJSON(w, http.StatusOK, target)
}

// forms rebuilds every form request of one session from durable events.
func (s *Server) forms(sessionID string) ([]formRecord, error) {
	events, err := s.store.Events(sessionID)
	if err != nil {
		return nil, err
	}
	forms := []formRecord{}
	index := map[string]int{}
	for _, ev := range events {
		switch ev.Type {
		case event.FormAsked:
			fields := []event.FormField{}
			if raw, ok := ev.Data["fields"]; ok {
				data, marshalErr := json.Marshal(raw)
				if marshalErr == nil {
					_ = json.Unmarshal(data, &fields)
				}
			}
			record := formRecord{
				ID:        eventText(ev, "id"),
				SessionID: sessionID,
				Title:     eventText(ev, "title"),
				Fields:    fields,
				Status:    "pending",
				AskedAt:   ev.At,
			}
			index[record.ID] = len(forms)
			forms = append(forms, record)
		case event.FormReplied:
			position, ok := index[eventText(ev, "id")]
			if !ok {
				continue
			}
			answered := ev.At
			forms[position].Status = "answered"
			forms[position].AnsweredAt = &answered
			if cancelled, _ := ev.Data["cancelled"].(bool); cancelled {
				forms[position].Status = "cancelled"
				forms[position].Cancelled = true
			}
			if values, ok := ev.Data["values"].(map[string]any); ok {
				forms[position].Values = values
			}
		}
	}
	return forms, nil
}

// normalizeFormFields trims names, defaults the type and rejects duplicates.
func normalizeFormFields(fields []event.FormField) ([]event.FormField, error) {
	if len(fields) == 0 {
		return nil, errors.New("fields are empty")
	}
	seen := map[string]bool{}
	out := make([]event.FormField, 0, len(fields))
	for _, field := range fields {
		field.Name = strings.TrimSpace(field.Name)
		if field.Name == "" {
			return nil, errors.New("field name is empty")
		}
		if seen[field.Name] {
			return nil, fmt.Errorf("duplicate field %q", field.Name)
		}
		seen[field.Name] = true
		if field.Label == "" {
			field.Label = field.Name
		}
		if field.Type == "" {
			field.Type = "text"
		}
		options := make([]string, 0, len(field.Options))
		for _, option := range field.Options {
			if trimmed := strings.TrimSpace(option); trimmed != "" {
				options = append(options, trimmed)
			}
		}
		field.Options = options
		out = append(out, field)
	}
	return out, nil
}

// validateFormValues checks that every required field has a value.
func validateFormValues(form formRecord, values map[string]any) error {
	for _, field := range form.Fields {
		if !field.Required {
			continue
		}
		value, ok := values[field.Name]
		if !ok || strings.TrimSpace(fmt.Sprint(value)) == "" {
			return fmt.Errorf("field %q is required", field.Name)
		}
	}
	return nil
}

// formReplyText renders the model-visible summary of one answer.
func formReplyText(form formRecord, values map[string]any, cancelled bool) string {
	if cancelled {
		return fmt.Sprintf("[Form cancelled]\nForm: %s (%s)", form.Title, form.ID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[Form answered]\nForm: %s (%s)\n", form.Title, form.ID)
	for _, field := range form.Fields {
		value, ok := values[field.Name]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "- %s: %v\n", field.Name, value)
	}
	return strings.TrimRight(b.String(), "\n")
}
