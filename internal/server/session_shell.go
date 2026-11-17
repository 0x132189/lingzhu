package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"lingzhu/internal/agent"
	"lingzhu/internal/config"
	"lingzhu/internal/event"
	"lingzhu/internal/job"
	"lingzhu/internal/proc"
	"lingzhu/internal/provider"
)

const (
	defaultSessionShellTimeout = 120 * time.Second
	maxSessionShellTimeout     = 10 * time.Minute
	maxSessionShellOutput      = 64 * 1024
)

type sessionShellResult struct {
	Output    string `json:"output"`
	Exit      int    `json:"exit"`
	TimedOut  bool   `json:"timedOut"`
	Truncated bool   `json:"truncated"`
}

type skillSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// listSkills returns the discovered skill catalog without file contents.
func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	items := []skillSummary{}
	if manager := s.skillsFor(r); manager != nil {
		for _, info := range manager.List() {
			items = append(items, skillSummary{ID: info.ID, Name: info.Name, Description: info.Description})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": items})
}

// runSkill injects one skill's instructions as a user turn.
func (s *Server) runSkill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	skills := s.skillsFor(r)
	if skills == nil {
		writeError(w, http.StatusNotImplemented, errText("skills are not available"))
		return
	}
	var body struct {
		Name      string `json:"name"`
		ID        string `json:"id"`
		Arguments string `json:"arguments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = strings.TrimSpace(body.ID)
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, errText("skill name is empty"))
		return
	}
	item, ok := skills.Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, errText("skill not found"))
		return
	}
	prompt := item.Loaded()
	if arguments := strings.TrimSpace(body.Arguments); arguments != "" {
		prompt += "\n\nArguments: " + arguments
	}
	target := s.targetFor(id)
	events, err := s.agentFor(id).Run(context.Background(), id, prompt, provider.Selection{
		Provider: target.ProviderID,
		Model:    target.Model,
	})
	if err != nil {
		if errors.Is(err, agent.ErrBusy) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	go func() {
		for range events {
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "skill": item.ID})
}

// runSessionShell runs one command in the workspace, in the foreground unless
// the request asks for a background job.
func (s *Server) runSessionShell(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	var body struct {
		Command    string `json:"command"`
		TimeoutMS  int    `json:"timeout_ms"`
		Background bool   `json:"background"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	command := strings.TrimSpace(body.Command)
	if command == "" {
		writeError(w, http.StatusBadRequest, errText("command is empty"))
		return
	}
	shell := s.shellCommandForRequest(r)
	dir := s.shellDirForRequest(r)
	timeout := sessionShellTimeout(body.TimeoutMS)
	if body.Background {
		item, err := s.startJob(id, shell, dir, command, sessionBackgroundTimeout(body.TimeoutMS))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusAccepted, item)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	cmd := exec.Command(shell[0], append(append([]string{}, shell[1:]...), command)...)
	cmd.Dir = dir
	proc.Configure(cmd)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	proc.Track(cmd)
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			proc.Stop(cmd, proc.Grace)
		case <-stopped:
		}
	}()
	runErr := cmd.Wait()
	close(stopped)
	proc.Release(cmd)
	result := sessionShellResult{}
	result.Output, result.Truncated = truncateShellOutput(output.Bytes())
	result.Exit = 0
	if errors.Is(runErr, exec.ErrWaitDelay) {
		runErr = nil
		if cmd.ProcessState != nil {
			result.Exit = cmd.ProcessState.ExitCode()
		}
	}
	if runErr != nil {
		result.Exit = -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			result.Exit = exitErr.ExitCode()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.TimedOut = true
		}
	}
	// The command and its output belong to the conversation, like the reference:
	// the next prompt sees them and the transcript keeps the shell message.
	s.recordShell(id, command, result)
	writeJSON(w, http.StatusOK, result)
}

// recordShell persists one finished shell run and publishes it to live clients.
func (s *Server) recordShell(sessionID, command string, result sessionShellResult) {
	ev := event.ShellEndedEvent(command, result.Output, result.Exit, result.TimedOut, result.Truncated)
	seq, err := s.store.Append(sessionID, ev)
	if err != nil {
		return
	}
	ev.Seq = seq
	if loop := s.agentFor(sessionID); loop != nil {
		loop.Publish(sessionID, ev)
	}
}

// startBackground starts a background command for the session.
func (s *Server) startBackground(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	var body struct {
		Command   string `json:"command"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	command := strings.TrimSpace(body.Command)
	if command == "" {
		writeError(w, http.StatusBadRequest, errText("command is empty"))
		return
	}
	timeout := sessionBackgroundTimeout(body.TimeoutMS)
	item, err := s.startJob(id, s.shellCommandForRequest(r), s.shellDirForRequest(r), command, timeout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, item)
}

// listSessionJobs returns the session's background commands, newest first.
func (s *Server) listSessionJobs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	items := []job.Job{}
	if s.jobs != nil {
		items = s.jobs.List(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": items})
}

// listWorkspaceJobs lists background jobs across every session.
func (s *Server) listWorkspaceJobs(w http.ResponseWriter, r *http.Request) {
	items := []job.Job{}
	if s.jobs != nil {
		items = s.jobs.List("")
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": items})
}

func (s *Server) startJob(sessionID string, shell []string, dir, command string, timeout time.Duration) (job.Job, error) {
	if s.jobs == nil {
		return job.Job{}, errors.New("background jobs are not available")
	}
	return s.jobs.Start(sessionID, shell, dir, command, timeout)
}

func (s *Server) shellCommand() []string {
	return shellCommandFor(s.config)
}

func (s *Server) shellDir() string {
	return shellDirFor(s.config, s.root)
}

// shellCommandForRequest is the shell prefix of the workspace a request names.
func (s *Server) shellCommandForRequest(r *http.Request) []string {
	return shellCommandFor(s.configFor(r))
}

// shellDirForRequest is the directory a request's shell commands start in.
func (s *Server) shellDirForRequest(r *http.Request) string {
	return shellDirFor(s.configFor(r), s.rootFor(r))
}

// shellCommandFor is the configured shell prefix of one workspace.
func shellCommandFor(cfg config.Config) []string {
	if len(cfg.Shell) > 0 {
		return cfg.Shell
	}
	return config.DefaultShell()
}

// shellDirFor is the directory a workspace's shell commands start in.
func shellDirFor(cfg config.Config, root string) string {
	if strings.TrimSpace(cfg.Workdir) != "" {
		return cfg.Workdir
	}
	if strings.TrimSpace(root) != "" {
		return root
	}
	return "."
}

// sessionShellTimeout applies the default and the upper bound to a request.
func sessionShellTimeout(timeoutMS int) time.Duration {
	if timeoutMS <= 0 {
		return defaultSessionShellTimeout
	}
	timeout := time.Duration(timeoutMS) * time.Millisecond
	if timeout > maxSessionShellTimeout {
		return maxSessionShellTimeout
	}
	return timeout
}

// sessionBackgroundTimeout leaves background commands unbounded by default.
func sessionBackgroundTimeout(timeoutMS int) time.Duration {
	if timeoutMS <= 0 {
		return 0
	}
	timeout := time.Duration(timeoutMS) * time.Millisecond
	if timeout > maxSessionShellTimeout {
		return maxSessionShellTimeout
	}
	return timeout
}

// truncateShellOutput keeps the last maxSessionShellOutput bytes.
func truncateShellOutput(data []byte) (string, bool) {
	truncated := false
	if len(data) > maxSessionShellOutput {
		data = data[len(data)-maxSessionShellOutput:]
		truncated = true
	}
	return strings.ToValidUTF8(string(data), ""), truncated
}
