// Package policy applies ordered allow/deny statements that can only tighten
// what permissions and configuration already allow.
package policy

import (
	"regexp"
	"strings"
)

type Effect string

const (
	Allow Effect = "allow"
	Deny  Effect = "deny"
)

// Statement controls one action on a named resource. Resources use whole-value
// wildcards: * matches any run and ? matches one character.
type Statement struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   Effect `json:"effect"`
}

// Rules is an ordered policy list. The last matching statement wins.
type Rules []Statement

// Normalize drops statements that cannot be evaluated and keeps order.
func Normalize(input []Statement) Rules {
	out := make(Rules, 0, len(input))
	for _, statement := range input {
		action := strings.TrimSpace(statement.Action)
		resource := strings.TrimSpace(statement.Resource)
		effect := Effect(strings.ToLower(strings.TrimSpace(string(statement.Effect))))
		if action == "" || resource == "" || (effect != Allow && effect != Deny) {
			continue
		}
		out = append(out, Statement{Action: action, Resource: resource, Effect: effect})
	}
	return out
}

// Evaluate returns the last matching effect, defaulting to allow.
func (r Rules) Evaluate(action, resource string) Effect {
	effect := Allow
	for _, statement := range r {
		if match(action, statement.Action) && match(resource, statement.Resource) {
			effect = statement.Effect
		}
	}
	return effect
}

// Denied reports whether the action is blocked after evaluating the resource.
func (r Rules) Denied(action, resource string) bool {
	return r.Evaluate(action, resource) == Deny
}

// DeniedAll reports one deny for any resource in the list.
func (r Rules) DeniedAll(action string, resources []string) bool {
	if len(resources) == 0 {
		return r.Denied(action, "*")
	}
	for _, resource := range resources {
		if r.Denied(action, resource) {
			return true
		}
	}
	return false
}

// match is a case-insensitive whole-value wildcard match.
func match(input, pattern string) bool {
	normalized := strings.ReplaceAll(input, "\\", "/")
	pattern = strings.ReplaceAll(pattern, "\\", "/")
	var builder strings.Builder
	builder.WriteString("(?is)^")
	for _, char := range pattern {
		switch char {
		case '*':
			builder.WriteString(".*")
		case '?':
			builder.WriteString(".")
		default:
			builder.WriteString(regexp.QuoteMeta(string(char)))
		}
	}
	builder.WriteString("$")
	re, err := regexp.Compile(builder.String())
	if err != nil {
		return false
	}
	return re.MatchString(normalized)
}
