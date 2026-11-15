package agent

import (
	"context"
	"strings"

	"lingzhu/internal/agents"
	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
)

// maxTitleInput bounds the first message sent to the title model.
const maxTitleInput = 2000

// generateTitle names an untitled session from its first user message. It runs
// after a turn produced a reply, and any failure leaves the automatic label in
// place. The caller holds the per-session claim.
func (a *Agent) generateTitle(ctx context.Context, sessionID string, target provider.Target) {
	info, err := a.store.Info(sessionID)
	if err != nil {
		return
	}
	// Subagent children get a label from their parent, and a renamed session
	// keeps the user's title.
	if info.Parent != "" {
		return
	}
	first, err := a.firstUserText(sessionID)
	if err != nil || strings.TrimSpace(first) == "" {
		return
	}
	if !session.Untitled(info.Title, first) {
		return
	}

	text := []rune(first)
	if len(text) > maxTitleInput {
		first = string(text[:maxTitleInput])
	}
	messages := []model.Message{
		{Role: model.RoleUser, Content: session.TitleInput(first)},
	}
	titleTarget := titleTargetOf(a.catalog, target)
	system := ""
	if a.profiles != nil {
		if profile, ok := a.profiles.Get("title"); ok {
			system = profile.System
			if profile.Model != "" {
				if providerID, modelID, err := agents.ValidateModel(profile.Model); err == nil {
					if resolved, err := a.catalog.Resolve(provider.Selection{Provider: providerID, Model: modelID}); err == nil {
						titleTarget = resolved
					}
				}
			}
		}
	}
	reply, err := a.completeWithSystem(ctx, titleTarget, system, messages)
	if err != nil {
		return
	}
	title := session.CleanTitle(reply)
	if title == "" {
		return
	}
	// Re-read before writing so a manual rename during the model call wins.
	current, err := a.store.Info(sessionID)
	if err != nil || !session.Untitled(current.Title, first) {
		return
	}
	if err := a.store.SetTitle(sessionID, title); err != nil {
		return
	}
	_ = a.emit(sessionID, event.SessionRenamedEvent(sessionID, title))
}

// titleTargetOf prefers the provider's small text model, then the least
// reasoning variant, matching the reference title selection.
func titleTargetOf(catalog *provider.Catalog, target provider.Target) provider.Target {
	if catalog == nil {
		return target
	}
	if small, ok := catalog.Small(target.ProviderID); ok {
		return small
	}
	lowered, ok := catalog.LowerReasoning(provider.Selection{Provider: target.ProviderID, Model: target.Model})
	if !ok {
		return target
	}
	return lowered
}

// titleBusy serializes title generation per session.
func (a *Agent) titleBusy(sessionID string) bool {
	if _, loaded := a.titles.LoadOrStore(sessionID, true); loaded {
		return true
	}
	return false
}

// releaseTitle clears the per-session title claim.
func (a *Agent) releaseTitle(sessionID string) {
	a.titles.Delete(sessionID)
}

// firstUserText returns the text of the session's first user message.
func (a *Agent) firstUserText(sessionID string) (string, error) {
	events, err := a.store.Events(sessionID)
	if err != nil {
		return "", err
	}
	for _, ev := range events {
		if ev.Type != event.UserMessage {
			continue
		}
		if text, ok := ev.Data["text"].(string); ok {
			return text, nil
		}
	}
	return "", nil
}
