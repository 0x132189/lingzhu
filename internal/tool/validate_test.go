package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const sampleSchema = `{
	"type": "object",
	"properties": {
		"command": {"type": "string"},
		"timeout": {"type": "integer"},
		"paths":   {"type": "array", "items": {"type": "string"}}
	},
	"required": ["command"]
}`

func TestValidateAcceptsWellFormedArguments(t *testing.T) {
	if err := Validate("shell", json.RawMessage(sampleSchema), []byte(`{"command":"ls","timeout":100,"paths":["a"]}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsMissingRequiredField(t *testing.T) {
	err := Validate("shell", json.RawMessage(sampleSchema), []byte(`{"timeout":100}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	text := err.Error()
	if !strings.Contains(text, `Invalid arguments for tool "shell"`) {
		t.Fatalf("message should name the tool: %s", text)
	}
	if !strings.Contains(text, "command") || !strings.Contains(text, "required") {
		t.Fatalf("message should name the missing field: %s", text)
	}
	if !strings.Contains(text, `"timeout":100`) {
		t.Fatalf("message should repeat the arguments: %s", text)
	}
}

func TestValidateRejectsWrongTypeWithFieldPath(t *testing.T) {
	err := Validate("shell", json.RawMessage(sampleSchema), []byte(`{"command":"ls","timeout":"100"}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	text := err.Error()
	if !strings.Contains(text, "timeout") || !strings.Contains(text, "integer") {
		t.Fatalf("message should name the field and its type: %s", text)
	}
	if strings.Contains(text, "properties") {
		t.Fatalf("message should use a dotted field path: %s", text)
	}
}

func TestValidateRejectsWrongItemType(t *testing.T) {
	err := Validate("shell", json.RawMessage(sampleSchema), []byte(`{"command":"ls","paths":"a"}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "paths") {
		t.Fatalf("message should name the field: %s", err.Error())
	}
}

func TestValidateAcceptsUnknownProperties(t *testing.T) {
	// Providers send extra keys often enough that rejecting them would fail
	// more calls than it fixes.
	if err := Validate("shell", json.RawMessage(sampleSchema), []byte(`{"command":"ls","extra":true}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAcceptsEmptyInputForSchemaWithoutRequiredFields(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`)
	if err := Validate("echo", schema, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := Validate("echo", schema, []byte(`null`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSkipsToolsWithoutSchema(t *testing.T) {
	if err := Validate("free", nil, []byte(`not json`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := Validate("free", json.RawMessage(`null`), []byte(`not json`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSkipsUncompilableSchema(t *testing.T) {
	// A third-party schema this compiler cannot resolve must not fail every
	// call; the tool still enforces its own arguments.
	schema := json.RawMessage(`{"type":"object","properties":{"a":{"$ref":"#/$defs/missing"}}}`)
	if err := Validate("mcp_tool", schema, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsMalformedJSON(t *testing.T) {
	err := Validate("shell", json.RawMessage(sampleSchema), []byte(`{"command":`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Update the arguments") {
		t.Fatalf("message should tell the model to retry: %s", err.Error())
	}
}

func TestValidationErrorLimitsListedIssues(t *testing.T) {
	// The validator stops at the first problem inside an object, so the cap is
	// exercised through the renderer that also serves composed schemas.
	err := validationError("many", errors.New(strings.Join([]string{
		"validating root: validating /properties/a: type: a",
		"validating root: validating /properties/b: type: b",
		"validating root: validating /properties/c: type: c",
		"validating root: validating /properties/d: type: d",
		"validating root: validating /properties/e: type: e",
		"validating root: validating /properties/f: type: f",
	}, "\n")), map[string]any{"a": "x"})
	text := err.Error()
	if !strings.Contains(text, "more issue") {
		t.Fatalf("message should summarize the remaining issues: %s", text)
	}
	if strings.Count(text, "\n- ") != maxIssues+1 {
		t.Fatalf("message should list exactly %d issues plus the summary: %s", maxIssues, text)
	}
}

func TestUnwrapValidationBuildsDottedPaths(t *testing.T) {
	cases := []struct{ line, path, reason string }{
		{"validating root: required: missing properties: [\"command\"]", "arguments", "required: missing properties: [\"command\"]"},
		{"validating root: validating /properties/timeout: type: 100 has type \"string\", want \"integer\"", "timeout", "type: 100 has type \"string\", want \"integer\""},
		{"validating root: validating /properties/questions: validating /properties/questions/items: validating /properties/questions/items/properties/header: type: 1", "questions.header", "type: 1"},
		{"validating /properties/tags/items: type: 1", "tags", "type: 1"},
	}
	for _, item := range cases {
		path, reason := unwrapValidation(item.line)
		if path != item.path || reason != item.reason {
			t.Fatalf("%q: got (%q, %q), want (%q, %q)", item.line, path, reason, item.path, item.reason)
		}
	}
}

func TestRegistryRunValidatesBeforeExecuting(t *testing.T) {
	registry := NewRegistry()
	called := false
	registry.Add(Direct(schemaTool{ran: &called}))
	_, err := registry.Run(t.Context(), Call{Name: "schema", Input: json.RawMessage(`{"count":"two"}`)})
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if called {
		t.Fatal("the tool ran despite invalid arguments")
	}
	if _, err := registry.Run(t.Context(), Call{Name: "schema", Input: json.RawMessage(`{"count":2}`)}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("the tool did not run with valid arguments")
	}
}

type schemaTool struct{ ran *bool }

func (s schemaTool) Name() string        { return "schema" }
func (s schemaTool) Description() string { return "test tool" }
func (s schemaTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`)
}

func (s schemaTool) Run(context.Context, Call) (string, error) {
	*s.ran = true
	return "ok", nil
}
