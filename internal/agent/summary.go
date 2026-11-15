package agent

import (
	"context"
	"errors"
	"io"
	"strings"

	"lingzhu/internal/agents"
	"lingzhu/internal/model"
	"lingzhu/internal/provider"
)

// SummarizePrompt is the user turn that asks the hidden summary agent for a
// pull-request-style summary.
const SummarizePrompt = "Summarize what was done in this conversation."

// Summarize runs the hidden summary agent over the session history. It never
// offers tools and does not mutate the session, so it is safe to call on an
// idle conversation.
func (a *Agent) Summarize(ctx context.Context, sessionID string, selection provider.Selection) (string, error) {
	if a.Running(sessionID) {
		return "", ErrBusy
	}
	profile := agents.Profile{ID: "summary", Name: "Summary"}
	if a.profiles != nil {
		if found, ok := a.profiles.Get("summary"); ok {
			profile = found
		}
	}
	if strings.TrimSpace(profile.System) == "" {
		return "", errors.New("summary agent is not configured")
	}
	if profile.Model != "" {
		if providerID, modelID, err := agents.ValidateModel(profile.Model); err == nil {
			selection = provider.Selection{Provider: providerID, Model: modelID}
		}
	}
	if selection.Provider == "" && selection.Model == "" {
		if meta, err := a.store.Meta(sessionID); err == nil {
			selection = provider.Selection{Provider: meta.Provider, Model: meta.Model}
		}
	}
	target, err := a.catalog.Resolve(selection)
	if err != nil {
		return "", err
	}
	if target.Model == "" {
		return "", errors.New("model is not configured")
	}
	current, err := a.store.Context(sessionID)
	if err != nil {
		return "", err
	}
	messages := append([]model.Message{}, current.Messages...)
	messages = append(messages, model.Message{Role: model.RoleUser, Content: SummarizePrompt})
	return a.completeWithSystem(ctx, target, profile.System, messages)
}

// completeWithSystem sends one non-tool completion under an explicit system
// prompt. Titles and summaries use it when they must not inherit the session's
// agent prompt.
func (a *Agent) completeWithSystem(ctx context.Context, target provider.Target, system string, messages []model.Message) (string, error) {
	wire := target.RequestModel
	if wire == "" {
		wire = target.Model
	}
	promptMessages := make([]model.Message, 0, len(messages)+1)
	if system != "" {
		promptMessages = append(promptMessages, model.Message{Role: model.RoleSystem, Content: system})
	}
	promptMessages = append(promptMessages, messages...)
	stream, err := target.Provider.Stream(ctx, model.Request{
		Model:    wire,
		Messages: promptMessages,
		Headers:  target.Headers,
		Options:  target.Options,
	})
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var b strings.Builder
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return strings.TrimSpace(b.String()), err
		}
		b.WriteString(chunk.Text)
	}
	return strings.TrimSpace(b.String()), nil
}
