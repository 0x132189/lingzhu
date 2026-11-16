package agent

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"lingzhu/internal/event"
	"lingzhu/internal/goal"
	"lingzhu/internal/model"
	"lingzhu/internal/provider"
)

// bootID identifies this process so a goal armed by an earlier process comes
// back paused instead of resuming on its own.
var bootID = func() string {
	state := goal.New("boot", "")
	return state.ID
}()

// Goal returns the stored goal of a session, or nil.
func (a *Agent) Goal(sessionID string) *goal.State {
	raw, err := a.store.Goal(sessionID)
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil
	}
	var state goal.State
	if json.Unmarshal([]byte(raw), &state) != nil {
		return nil
	}
	return &state
}

// SetGoal stores a goal and publishes it. An empty state clears the goal.
func (a *Agent) SetGoal(sessionID string, state *goal.State) error {
	if state == nil {
		if err := a.store.SetGoal(sessionID, ""); err != nil {
			return err
		}
		a.hub.Publish(sessionID, goalEvent(sessionID, nil))
		return nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := a.store.SetGoal(sessionID, string(data)); err != nil {
		return err
	}
	a.hub.Publish(sessionID, goalEvent(sessionID, state))
	return nil
}

// goalEvent is a live-only notice so clients refresh their goal view.
func goalEvent(sessionID string, state *goal.State) event.Event {
	ev := event.New(event.Synthetic, map[string]any{"kind": "goal", "sessionID": sessionID})
	if state != nil {
		ev.Data["goal"] = state
	}
	return ev
}

// StartGoal arms a goal and sends the kickoff as a synthetic message. The turn
// that follows runs through the normal loop.
func (a *Agent) StartGoal(ctx context.Context, sessionID, objective string, selection provider.Selection) (<-chan event.Event, error) {
	text := strings.TrimSpace(objective)
	if text == "" {
		return nil, errGoalEmpty
	}
	if len([]rune(text)) > goal.MaxObjective {
		return nil, errGoalTooLong
	}
	state := goal.New(text, bootID)
	if err := a.SetGoal(sessionID, &state); err != nil {
		return nil, err
	}
	// A goal created before any reply names the session, matching how a first
	// user message becomes the title.
	if info, err := a.store.Info(sessionID); err == nil && strings.TrimSpace(info.Title) == "" {
		_ = a.store.SetTitle(sessionID, goal.Truncate(text, 60))
	}
	kickoff := goal.Render(goal.Kickoff, map[string]string{"objective": goal.Escape(text)})
	return a.runSubmission(ctx, sessionID, Submission{Text: kickoff}, selection, false)
}

// PauseGoal stops an active goal.
func (a *Agent) PauseGoal(sessionID, reason string) error {
	state := a.Goal(sessionID)
	if state == nil || state.Status != goal.Active {
		return errGoalNotActive
	}
	state.Status = goal.Paused
	state.Reason = reason
	state.UpdatedAt = nowUTC()
	return a.SetGoal(sessionID, state)
}

// ResumeGoal restarts a paused goal and sends a continuation.
func (a *Agent) ResumeGoal(ctx context.Context, sessionID string, selection provider.Selection) (<-chan event.Event, error) {
	state := a.Goal(sessionID)
	if state == nil || state.Status != goal.Paused {
		return nil, errGoalNotPaused
	}
	state.Status = goal.Active
	state.Rounds = 1
	state.Stalls = 0
	state.Reason = "The user resumed the goal."
	state.Boot = bootID
	state.UpdatedAt = nowUTC()
	if err := a.SetGoal(sessionID, state); err != nil {
		return nil, err
	}
	text := goal.Render(goal.Continuation, map[string]string{
		"objective": goal.Escape(state.Objective),
		"feedback":  "The user resumed the goal.",
	})
	return a.runSubmission(ctx, sessionID, Submission{Text: text}, selection, false)
}

// ClearGoal removes a goal.
func (a *Agent) ClearGoal(sessionID string) error {
	if a.Goal(sessionID) == nil {
		return errGoalMissing
	}
	return a.SetGoal(sessionID, nil)
}

// judgeGoal asks the reviewer whether the objective is met. A missing or
// unparsable verdict pauses the goal rather than looping blindly.
func (a *Agent) judgeGoal(ctx context.Context, sessionID string, target provider.Target, state *goal.State) (goal.Verdict, bool) {
	prompt := goal.Render(goal.Judge, map[string]string{
		"objective":  goal.Escape(state.Objective),
		"background": "No background shells are running.",
	})
	current, err := a.store.Context(sessionID)
	if err != nil {
		return goal.Verdict{}, false
	}
	messages := make([]model.Message, 0, len(current.Messages)+1)
	messages = append(messages, current.Messages...)
	messages = append(messages, model.Message{Role: model.RoleUser, Content: prompt})
	reply, usage, err := a.complete(ctx, sessionID, target, messages)
	if usage != nil {
		_ = a.emit(sessionID, event.UsageEvent(usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite))
	}
	if err != nil {
		return goal.Verdict{}, false
	}
	return goal.ParseVerdict(reply)
}

// continueGoal runs one reviewer round and either continues the session or
// settles the goal. It reports whether a continuation was queued.
func (a *Agent) continueGoal(ctx context.Context, sessionID string, target provider.Target) bool {
	state := a.Goal(sessionID)
	if state == nil || state.Status != goal.Active {
		return false
	}
	if state.Boot != bootID {
		a.settleGoal(sessionID, state, goal.Paused, "The server restarted while the goal was active")
		return false
	}
	if state.Rounds >= state.MaxRounds {
		a.settleGoal(sessionID, state, goal.Paused, "Reached the round limit")
		return false
	}

	verdict, ok := a.judgeGoal(ctx, sessionID, target, state)
	if !ok {
		a.settleGoal(sessionID, state, goal.Paused, "The goal reviewer gave no usable verdict")
		return false
	}
	// A control command may have landed while the reviewer ran.
	latest := a.Goal(sessionID)
	if latest == nil || latest.ID != state.ID || latest.Status != goal.Active {
		return false
	}
	switch {
	case verdict.IsDone():
		a.settleGoal(sessionID, latest, goal.Complete, verdict.Reason)
		return false
	case verdict.IsBlocked(), verdict.IsPaused():
		a.settleGoal(sessionID, latest, goal.Paused, verdict.Reason)
		return false
	case !verdict.IsContinue():
		a.settleGoal(sessionID, latest, goal.Paused, verdict.Reason)
		return false
	}

	stalled := goal.SameReason(latest.Reason, verdict.Reason)
	stalls := 0
	if stalled {
		stalls = latest.Stalls + 1
	}
	if stalls >= goal.MaxStalls {
		a.settleGoal(sessionID, latest, goal.Paused, "Not making progress: "+verdict.Reason)
		return false
	}

	next := *latest
	next.Rounds++
	next.Stalls = stalls
	next.Reason = verdict.Reason
	next.UpdatedAt = nowUTC()
	if err := a.SetGoal(sessionID, &next); err != nil {
		return false
	}
	feedback := verdict.Reason
	if verdict.NextStep != "" {
		feedback += "\nNext step: " + verdict.NextStep
	}
	text := goal.Render(goal.Continuation, map[string]string{
		"objective": goal.Escape(next.Objective),
		"feedback":  feedback,
	})
	if err := a.emit(sessionID, event.SyntheticEvent(text)); err != nil {
		log.Printf("agent: goal continuation %s: %v", sessionID, err)
		return false
	}
	return true
}

// settleGoal records a terminal status.
func (a *Agent) settleGoal(sessionID string, state *goal.State, status goal.Status, reason string) {
	next := *state
	next.Status = status
	next.Reason = reason
	next.UpdatedAt = nowUTC()
	if err := a.SetGoal(sessionID, &next); err != nil {
		log.Printf("agent: settle goal %s: %v", sessionID, err)
	}
}

// goalReminder returns the reminder text when an active goal's objective is no
// longer visible in the history.
func (a *Agent) goalReminder(sessionID string, messages []model.Message) string {
	state := a.Goal(sessionID)
	if state == nil || state.Status != goal.Active {
		return ""
	}
	block := goal.ObjectiveBlock(state.Objective)
	for _, message := range messages {
		if strings.Contains(message.Content, block) {
			return ""
		}
	}
	return goal.Render(goal.Reminder, map[string]string{"objective": goal.Escape(state.Objective)})
}
