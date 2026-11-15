package tool

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// resolvedSchema caches the compiled form of one tool's input schema. Schemas
// are fixed at registration, so a compiled copy is reused for every call.
type resolvedSchema struct {
	value *jsonschema.Resolved
	// err records a schema that cannot be compiled. Validation is skipped for
	// it instead of rejecting every call, because a third-party tool (MCP or a
	// plugin) may publish a schema this compiler does not support.
	err error
}

var schemaCache sync.Map // map[string]resolvedSchema

// compileSchema resolves one input schema, reusing an earlier result.
func compileSchema(schema json.RawMessage) resolvedSchema {
	key := string(schema)
	if cached, ok := schemaCache.Load(key); ok {
		return cached.(resolvedSchema)
	}
	compiled := resolvedSchema{}
	var parsed jsonschema.Schema
	if err := json.Unmarshal(schema, &parsed); err != nil {
		compiled.err = err
	} else if resolved, err := parsed.Resolve(nil); err != nil {
		compiled.err = err
	} else {
		compiled.value = resolved
	}
	schemaCache.Store(key, compiled)
	return compiled
}

// Validate checks tool arguments against the tool's declared input schema.
//
// The returned error is written for the model: it names every rejected field
// and repeats the arguments it received, so the next attempt can correct them
// instead of guessing. A schema this package cannot compile is not enforced;
// a tool without a schema accepts anything.
func Validate(name string, schema json.RawMessage, input []byte) error {
	if len(schema) == 0 || string(schema) == "null" {
		return nil
	}
	compiled := compileSchema(schema)
	if compiled.err != nil || compiled.value == nil {
		return nil
	}
	var value any
	if len(input) == 0 || string(input) == "null" {
		value = map[string]any{}
	} else if err := json.Unmarshal(input, &value); err != nil {
		return fmt.Errorf("Invalid arguments for tool %q: %v\n\nUpdate the arguments and call the tool again.", name, err)
	}
	if err := compiled.value.Validate(value); err != nil {
		return validationError(name, err, value)
	}
	return nil
}

// maxIssues bounds how many rejected fields one message lists.
const maxIssues = 5

// validationError renders a validator failure as a field list the model can act
// on. The validator nests its reasons as "validating <pointer>: <reason>" pairs
// on one line, so each line is unwrapped into a dotted path plus its reason.
func validationError(name string, err error, value any) error {
	var issues []string
	for _, line := range strings.Split(err.Error(), "\n") {
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
		path, reason := unwrapValidation(text)
		if reason == "" {
			continue
		}
		issues = append(issues, "- "+path+": "+reason)
	}
	if len(issues) == 0 {
		issues = append(issues, "- "+strings.TrimSpace(err.Error()))
	}
	hidden := 0
	if len(issues) > maxIssues {
		hidden = len(issues) - maxIssues
		issues = issues[:maxIssues]
	}
	arguments, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		arguments = []byte("{}")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Invalid arguments for tool %q:\n", name)
	b.WriteString(strings.Join(issues, "\n"))
	if hidden > 0 {
		fmt.Fprintf(&b, "\n- ...and %d more issue(s)", hidden)
	}
	fmt.Fprintf(&b, "\n\nArguments provided:\n%s\n\nUpdate the arguments and call the tool again.", arguments)
	return fmt.Errorf("%s", b.String())
}

// unwrapValidation turns one nested validator line into a path and a reason.
// "validating root: validating /properties/timeout: type: ..." becomes
// ("timeout", "type: ..."). Each pointer is absolute, so the deepest one wins.
func unwrapValidation(line string) (string, string) {
	path := ""
	text := line
	for {
		rest, ok := strings.CutPrefix(text, "validating ")
		if !ok {
			break
		}
		at := strings.Index(rest, ": ")
		if at < 0 {
			break
		}
		target := strings.TrimSpace(rest[:at])
		text = rest[at+2:]
		if segment := pointerPath(target); segment != "" {
			path = segment
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return path, ""
	}
	if path == "" {
		return "arguments", text
	}
	return path, text
}

// pointerPath converts a JSON pointer into a dotted field path, dropping the
// structural segments a reader does not need.
func pointerPath(pointer string) string {
	if pointer == "" || pointer == "root" {
		return ""
	}
	if !strings.HasPrefix(pointer, "/") {
		return ""
	}
	var parts []string
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		switch token {
		case "properties", "items", "prefixItems", "anyOf", "oneOf", "allOf", "$defs", "definitions":
			continue
		}
		token = strings.ReplaceAll(token, "~1", "/")
		token = strings.ReplaceAll(token, "~0", "~")
		if token == "" {
			continue
		}
		parts = append(parts, token)
	}
	return strings.Join(parts, ".")
}
