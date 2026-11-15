// Package model defines messages shared by provider, tool, session and agent.
package model

import (
	"encoding/json"
	"strings"
	"time"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one message sent to the model. Tool calls live on the assistant
// message; each tool result is its own role=tool message.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	Name       string     `json:"name,omitempty"`
	// Diff is UI metadata for a tool result; providers never receive it.
	Diff string `json:"diff,omitempty"`
	// Failed marks a tool result that reported an error, so history keeps the
	// same error card the live event showed instead of grouping it away.
	Failed bool `json:"failed,omitempty"`
	// Metadata carries UI-only tool result fields such as counts and nested
	// Code Mode calls; providers never receive it.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Attachments are media returned by a tool result. Providers that accept
	// attachments send them as a following user message.
	Attachments []Attachment `json:"attachments,omitempty"`
	// Synthetic marks a system notification delivered as a user turn. Clients
	// hide it from the transcript while the model still sees it.
	Synthetic bool `json:"synthetic,omitempty"`
	// SyntheticSource, SyntheticDescription and SyntheticState are UI-only
	// metadata for a synthetic turn, so history can render the same notice the
	// live event showed instead of an authorless user message.
	SyntheticSource      string `json:"syntheticSource,omitempty"`
	SyntheticDescription string `json:"syntheticDescription,omitempty"`
	SyntheticState       string `json:"syntheticState,omitempty"`
	// SyntheticID is the job or child session the notice reports on, so a
	// transcript can tell two background reports apart.
	SyntheticID string `json:"syntheticId,omitempty"`
	// SyntheticPaths lists instruction files a synthetic turn loaded, so history
	// draws the same "Loaded ..." row the live event showed.
	SyntheticPaths []string `json:"syntheticPaths,omitempty"`
	// Reasoning is UI-only text reported by the provider; providers never
	// receive it back. ReasoningCreated and ReasoningCompleted are UI-only
	// timestamps for the reasoning part, in RFC3339 form.
	Reasoning          string `json:"reasoning,omitempty"`
	ReasoningCreated   string `json:"reasoningCreated,omitempty"`
	ReasoningCompleted string `json:"reasoningCompleted,omitempty"`
	// Parts preserves the order of interleaved reasoning and text from one
	// model step. Providers never receive it; older history falls back to the
	// aggregated Reasoning and Content fields.
	Parts []MessagePart `json:"parts,omitempty"`
	// Shell describes a command the user ran directly, rendered as a shell
	// message. Content carries the same output into the model context.
	Shell *ShellRun `json:"shell,omitempty"`
	// Comments are line comments attached to a prompt for the transcript only;
	// providers never receive them.
	Comments []Comment `json:"comments,omitempty"`
	// Voice marks a prompt dictated through voice input.
	Voice bool `json:"voice,omitempty"`
	// References are server-side file paths delivered with the prompt.
	References []Reference `json:"references,omitempty"`
	// Switch marks a UI-only transcript row that records an agent or model
	// change. It carries no prompt text and providers never receive it.
	Switch *Switch `json:"switch,omitempty"`
	// Failure marks a UI-only row that explains why a turn stopped with an
	// error. Providers never receive it.
	Failure *Failure `json:"failure,omitempty"`
}

// Failure is the reason a turn ended in an error, kept in history so a client
// that reconnects or restarts still shows what went wrong.
type Failure struct {
	Message string `json:"message"`
}

// Switch records who answers next after an agent or model change. Kind is
// "agent" or "model"; the other fields describe that change.
type Switch struct {
	Kind             string `json:"kind"`
	Agent            string `json:"agent,omitempty"`
	Previous         string `json:"previous,omitempty"`
	Provider         string `json:"provider,omitempty"`
	Model            string `json:"model,omitempty"`
	PreviousProvider string `json:"previousProvider,omitempty"`
	PreviousModel    string `json:"previousModel,omitempty"`
}

// Comment is one line comment attached to a user prompt.
type Comment struct {
	ID   string `json:"id,omitempty"`
	File string `json:"file"`
	Line int    `json:"line,omitempty"`
	Body string `json:"body"`
}

// Reference is one server-side file delivered as a path instead of inline bytes.
type Reference struct {
	Name string `json:"name"`
	Mime string `json:"mime,omitempty"`
	Path string `json:"path"`
}

// MessagePart is one ordered reasoning or text segment in an assistant step.
type MessagePart struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Created   string `json:"created,omitempty"`
	Completed string `json:"completed,omitempty"`
	// Signature is the provider's proof that a reasoning block came from it.
	// Anthropic rejects a replayed thinking block without one, so it travels
	// with the reasoning and is sent back on the next request.
	Signature string `json:"signature,omitempty"`
	// Redacted marks a reasoning block the provider returned encrypted. It is
	// replayed verbatim and never shown as text.
	Redacted string `json:"redacted,omitempty"`
	// Source names the API that issued Signature or Redacted. A proof is only
	// meaningful to the provider that produced it, so a replay skips parts
	// belonging to another protocol.
	Source string `json:"source,omitempty"`
}

// ShellRun is one user-executed shell command with its captured output.
type ShellRun struct {
	Command   string `json:"command"`
	Output    string `json:"output"`
	Exit      int    `json:"exit"`
	TimedOut  bool   `json:"timedOut,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Attachment is one inline file: image, PDF, UTF-8 text, or a directory
// listing. Data is base64 without a data URL prefix. Start and End are
// one-based source line numbers for a text range.
type Attachment struct {
	Mime  string `json:"mime"`
	Name  string `json:"name,omitempty"`
	Data  string `json:"data,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Start int    `json:"start,omitempty"`
	End   int    `json:"end,omitempty"`
}

// IsTextMime reports whether bytes of this MIME type can be inlined into the
// prompt as text rather than sent as provider media.
func IsTextMime(mime string) bool {
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	switch mime {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml",
		"application/javascript", "application/typescript", "application/x-sh", "application/toml":
		return true
	default:
		return false
	}
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

type Request struct {
	Model    string
	Messages []Message
	Tools    []ToolDef
	// ToolChoice selects the provider's tool mode. "none" keeps the tool
	// definitions (and their prompt cache) while forcing a text reply, which is
	// how the final max-steps step works in the reference harness.
	ToolChoice string
	// Headers are extra HTTP headers for this request.
	Headers map[string]string
	// Options are extra request-body fields for this model, such as
	// reasoning_effort or thinking.
	Options map[string]any
	// CacheTTL enables provider prompt-cache breakpoints when the catalog
	// reports a cache lifetime. Zero leaves caching to the provider.
	CacheTTL time.Duration
	// CacheKey is the stable affinity key for providers that accept one.
	CacheKey string
	// NativeCompaction asks the provider to replace its own context window
	// instead of returning a summary.
	NativeCompaction bool
	// CompactionTrigger is the token count at which the provider compacts.
	CompactionTrigger int
}
