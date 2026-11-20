package tool

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"lingzhu/internal/model"
)

// CatalogEntry is one tool rendered for the Code Mode catalog.
type CatalogEntry struct {
	// Path is the call expression inside execute, such as "read" or "lingzhu.models".
	Path string `json:"path"`
	// Signature is the rendered call expression with its parameter object and
	// return type.
	Signature string `json:"signature"`
	// Description is the first line of the tool description.
	Description string `json:"description"`
	// Namespace is the leading path segment, or "" for a top-level tool.
	Namespace string `json:"namespace"`
}

// descriptionLimit keeps one catalog line readable.
const descriptionLimit = 120

// catalogBudget is the approximate character budget for the inline catalog.
const catalogBudget = 8000

// searchSignature documents the `search` helper used by a partial catalog. It
// mirrors the returned JSON so a model can use the result without guessing
// field names or the accepted argument shapes.
const searchSignature = "search(query: string | { query?: string; namespace?: string; limit?: number; offset?: number }): { items: { path: string; signature: string; description: string; namespace: string }[]; remaining: number; next: { offset: number } | null }"

// noCodemodeTools is the reference harness wording for a session without
// Code Mode-eligible tools, such as a plain built-in-only session.
const noCodemodeTools = "# Code Mode\n\nNo Code Mode tools are currently available. Do not call `execute` unless there is at least one available Code Mode tool."

// Catalog renders every tool that execute can call, in stable path order.
func Catalog(registry *Registry) []CatalogEntry {
	if registry == nil {
		return nil
	}
	return catalogFromDefs(registry.CodemodeDefinitions())
}

// AllCatalog renders every non-execute tool, for the CodemodeOnly switch.
func AllCatalog(registry *Registry) []CatalogEntry {
	if registry == nil {
		return nil
	}
	return catalogFromDefs(registry.AllDefinitions())
}

func catalogFromDefs(defs []model.ToolDef) []CatalogEntry {
	entries := make([]CatalogEntry, 0, len(defs))
	for _, def := range defs {
		// execute cannot call itself, so it never appears in its own catalog.
		if def.Name == "execute" {
			continue
		}
		entries = append(entries, CatalogEntry{
			Path:        def.Name,
			Signature:   renderSignature(def.Name, def.Schema),
			Description: firstLine(def.Description),
			Namespace:   namespaceOf(def.Name),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries
}

func sectionFromDefs(defs []model.ToolDef) string {
	entries := catalogFromDefs(defs)
	if len(entries) == 0 {
		return ""
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		line := "  - tools." + entry.Signature
		if entry.Description != "" {
			line += " // " + entry.Description
		}
		lines = append(lines, line)
	}
	shown := 0
	total := 0
	for _, line := range lines {
		if total+len(line) > catalogBudget && shown > 0 {
			break
		}
		total += len(line) + 1
		shown++
	}

	var b strings.Builder
	b.WriteString("# Code Mode\n\n")
	if shown < len(lines) {
		b.WriteString("The tools listed below, and `search`, cannot be called directly: they only work as JavaScript inside the `execute` tool. Every other tool you have is a normal tool call: call it directly, never inside `execute`.\n\n")
		b.WriteString("The catalog is partial. Before calling a tool that is not listed below, find it with the global `search(...)` function. Do not guess tool names. Each result's `path` is the complete call expression: write it out as code, for example `await tools.ns.tool_name({ ... })`. Do not use it as a key such as `tools[path]`.\n")
		b.WriteString("- " + searchSignature + "\n")
	} else {
		b.WriteString("The tools listed below cannot be called directly: they only work as JavaScript inside the `execute` tool. Every other tool you have is a normal tool call: call it directly, never inside `execute`.\n\n")
		b.WriteString("The catalog is complete. Do not guess tool names.\n")
	}
	b.WriteString("Only the tools listed below, plus any returned by `search`, are available inside `execute`. Never call or recommend a tool that is not in this catalog.\n")
	b.WriteString("Every nested tool call resolves to the tool's text output as a string; `search` returns a structured object. Return a value or log with `console.log` to see it in the result.\n")
	b.WriteString("\n## Code Mode tools (call only inside `execute`)\n\n")
	b.WriteString(strings.Join(lines[:shown], "\n"))
	if shown < len(lines) {
		fmt.Fprintf(&b, "\n\n%d more tools are not shown. Use `search({ query })` to find them.", len(lines)-shown)
	}
	return b.String()
}

func searchEntries(entries []CatalogEntry, query, namespace string, limit, offset int) (items []CatalogEntry, remaining int, next int) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	matches := make([]CatalogEntry, 0)
	for _, entry := range entries {
		if namespace != "" && entry.Namespace != namespace {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(entry.Path), needle) && !strings.Contains(strings.ToLower(entry.Description), needle) {
			continue
		}
		matches = append(matches, entry)
	}
	if offset >= len(matches) {
		return nil, 0, -1
	}
	end := offset + limit
	if end > len(matches) {
		end = len(matches)
	}
	items = matches[offset:end]
	remaining = len(matches) - end
	next = -1
	if remaining > 0 {
		next = end
	}
	return items, remaining, next
}

// namespaceOf returns the leading path segment for grouped tool names.
func namespaceOf(name string) string {
	if index := strings.IndexAny(name, "_."); index > 0 {
		return name[:index]
	}
	return ""
}

// firstLine keeps the first sentence-sized line of a description.
func firstLine(text string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
	if len(line) <= descriptionLimit {
		return line
	}
	return strings.TrimSpace(line[:descriptionLimit-3]) + "..."
}

// renderSignature turns a JSON Schema into a TypeScript-shaped call expression.
// Code Mode resolves every tool to its text output, so the return type is
// always Promise<string>.
func renderSignature(name string, schema json.RawMessage) string {
	properties, required, order := schemaFields(schema)
	if len(properties) == 0 {
		return name + "(): Promise<string>"
	}
	var b strings.Builder
	b.WriteString(name + "({\n")
	for _, key := range order {
		raw, ok := properties[key]
		if !ok {
			continue
		}
		optional := !required[key]
		b.WriteString("  ")
		if doc := schemaDescription(raw); doc != "" {
			b.WriteString("/** " + doc + " */\n  ")
		}
		b.WriteString(key)
		if optional {
			b.WriteString("?")
		}
		b.WriteString(": " + schemaType(raw) + ",\n")
	}
	b.WriteString("}): Promise<string>")
	return b.String()
}

// schemaFields decodes properties, required names and their declaration order.
func schemaFields(schema json.RawMessage) (map[string]json.RawMessage, map[string]bool, []string) {
	properties := map[string]json.RawMessage{}
	required := map[string]bool{}
	order := []string{}
	if len(schema) == 0 {
		return properties, required, order
	}
	var root struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if json.Unmarshal(schema, &root) != nil {
		return properties, required, order
	}
	for _, name := range root.Required {
		required[name] = true
	}
	// Decode the properties object token by token so declaration order survives.
	decoder := json.NewDecoder(strings.NewReader(string(root.Properties)))
	token, err := decoder.Token()
	if err != nil {
		return properties, required, order
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return properties, required, order
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			break
		}
		name, _ := key.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			break
		}
		properties[name] = value
		order = append(order, name)
	}
	return properties, required, order
}

// schemaDescription reads the description annotation of one property.
func schemaDescription(raw json.RawMessage) string {
	var field struct {
		Description string `json:"description"`
	}
	if json.Unmarshal(raw, &field) != nil {
		return ""
	}
	return firstLine(field.Description)
}

// schemaType maps a JSON Schema fragment to a TypeScript type.
func schemaType(raw json.RawMessage) string {
	var field struct {
		Type        any               `json:"type"`
		Enum        []any             `json:"enum"`
		Items       json.RawMessage   `json:"items"`
		Properties  json.RawMessage   `json:"properties"`
		Required    []string          `json:"required"`
		AnyOf       []json.RawMessage `json:"anyOf"`
		Description string            `json:"description"`
	}
	if json.Unmarshal(raw, &field) != nil {
		return "unknown"
	}
	if len(field.Enum) > 0 {
		parts := make([]string, 0, len(field.Enum))
		for _, value := range field.Enum {
			switch typed := value.(type) {
			case string:
				parts = append(parts, `"`+typed+`"`)
			default:
				parts = append(parts, fmt.Sprint(typed))
			}
		}
		return strings.Join(parts, " | ")
	}
	if len(field.AnyOf) > 0 {
		parts := make([]string, 0, len(field.AnyOf))
		for _, item := range field.AnyOf {
			parts = append(parts, schemaType(item))
		}
		return strings.Join(parts, " | ")
	}
	kind := ""
	switch typed := field.Type.(type) {
	case string:
		kind = typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if name, ok := item.(string); ok {
				parts = append(parts, mapType(name, field))
			}
		}
		return strings.Join(parts, " | ")
	}
	return mapType(kind, field)
}

func mapType(kind string, field struct {
	Type        any               `json:"type"`
	Enum        []any             `json:"enum"`
	Items       json.RawMessage   `json:"items"`
	Properties  json.RawMessage   `json:"properties"`
	Required    []string          `json:"required"`
	AnyOf       []json.RawMessage `json:"anyOf"`
	Description string            `json:"description"`
}) string {
	switch kind {
	case "string":
		return "string"
	case "integer", "number":
		return "number"
	case "boolean":
		return "boolean"
	case "array":
		if len(field.Items) == 0 {
			return "unknown[]"
		}
		return schemaType(field.Items) + "[]"
	case "object":
		if len(field.Properties) == 0 {
			return "Record<string, unknown>"
		}
		properties, required, order := schemaFields(json.RawMessage(`{"properties":` + string(field.Properties) + `,"required":` + requiredJSON(field.Required) + `}`))
		parts := make([]string, 0, len(order))
		for _, key := range order {
			optional := ""
			if !required[key] {
				optional = "?"
			}
			parts = append(parts, key+optional+": "+schemaType(properties[key]))
		}
		return "{ " + strings.Join(parts, "; ") + " }"
	}
	return "unknown"
}

func requiredJSON(required []string) string {
	if len(required) == 0 {
		return "[]"
	}
	data, err := json.Marshal(required)
	if err != nil {
		return "[]"
	}
	return string(data)
}
