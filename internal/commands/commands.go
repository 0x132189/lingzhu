// Package commands discovers reusable prompt commands from configuration and
// markdown files. Names come from the path relative to a command root.
package commands

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Definition is one configured or discovered command.
type Definition struct {
	Description string `json:"description,omitempty"`
	Template    string `json:"template"`
	Agent       string `json:"agent,omitempty"`
	Model       string `json:"model,omitempty"`
}

// Config is the commands section of the configuration file.
type Config map[string]Definition

// Command is the client-facing view of a custom command.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Template    string `json:"template"`
	Plugin      string `json:"plugin"`
	Agent       string `json:"agent,omitempty"`
	Model       string `json:"model,omitempty"`
}

// Registry holds custom commands in stable name order.
type Registry struct {
	byName map[string]Command
	order  []string
}

// New builds a registry from configured definitions overlaid by discovered
// markdown definitions.
func New(config map[string]Definition, discovered map[string]Definition) *Registry {
	merged := map[string]Definition{}
	for name, def := range config {
		merged[name] = def
	}
	for name, def := range discovered {
		merged[name] = def
	}
	registry := &Registry{byName: map[string]Command{}}
	for name, def := range merged {
		template := strings.TrimSpace(def.Template)
		if name == "" || template == "" {
			continue
		}
		registry.byName[name] = Command{
			Name:        name,
			Description: strings.TrimSpace(def.Description),
			Template:    template,
			Plugin:      "custom",
			Agent:       strings.TrimSpace(def.Agent),
			Model:       strings.TrimSpace(def.Model),
		}
		registry.order = append(registry.order, name)
	}
	sort.Strings(registry.order)
	return registry
}

// Commands returns a copy in stable name order.
func (r *Registry) Commands() []Command {
	if r == nil {
		return nil
	}
	out := make([]Command, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.byName[name])
	}
	return out
}

// Get returns one command by name.
func (r *Registry) Get(name string) (Command, bool) {
	if r == nil {
		return Command{}, false
	}
	command, ok := r.byName[name]
	return command, ok
}

// Expand resolves a leading slash command to its rendered template.
func (r *Registry) Expand(text string) (string, bool) {
	if r == nil || !strings.HasPrefix(text, "/") {
		return text, false
	}
	body := strings.TrimPrefix(text, "/")
	index := strings.IndexFunc(body, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' })
	name := body
	args := ""
	if index >= 0 {
		name = body[:index]
		args = strings.TrimSpace(body[index:])
	}
	command, ok := r.Get(name)
	if !ok {
		return text, false
	}
	return Render(command.Template, args), true
}

var positional = regexp.MustCompile(`\$([1-9])`)

// Render expands $ARGUMENTS and $1..$9. The highest positional placeholder
// consumes that argument and every argument after it.
func Render(template, args string) string {
	out := strings.ReplaceAll(template, "$ARGUMENTS", args)
	matches := positional.FindAllStringSubmatch(template, -1)
	if len(matches) == 0 {
		if !strings.Contains(template, "$ARGUMENTS") && strings.TrimSpace(args) != "" {
			out = strings.TrimRight(out, "\n") + "\n\n" + args
		}
		return out
	}
	values := SplitArgs(args)
	highest := 0
	for _, match := range matches {
		var number int
		_, _ = fmt.Sscanf(match[1], "%d", &number)
		if number > highest {
			highest = number
		}
	}
	for number := 1; number <= highest; number++ {
		value := ""
		if number <= len(values) {
			if number == highest {
				value = strings.Join(values[number-1:], " ")
			} else {
				value = values[number-1]
			}
		}
		out = strings.ReplaceAll(out, fmt.Sprintf("$%d", number), value)
	}
	return out
}

// SplitArgs splits on whitespace while honoring single and double quotes.
func SplitArgs(value string) []string {
	var args []string
	var current strings.Builder
	quote := rune(0)
	for _, char := range value {
		switch {
		case quote != 0:
			if char == quote {
				quote = 0
			} else {
				current.WriteRune(char)
			}
		case char == '\'' || char == '"':
			quote = char
		case char == ' ' || char == '\t' || char == '\n':
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(char)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}
