package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type catalogTool struct {
	name        string
	description string
	schema      string
}

func (t catalogTool) Name() string            { return t.name }
func (t catalogTool) Description() string     { return t.description }
func (t catalogTool) Schema() json.RawMessage { return json.RawMessage(t.schema) }
func (t catalogTool) Run(context.Context, Call) (string, error) {
	return "", nil
}

func catalogRegistry() *Registry {
	registry := NewRegistry()
	registry.Add(Codemode(catalogTool{
		name:        "read",
		description: "Read the contents of a file or directory.\nSecond line is dropped.",
		schema: `{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "File or directory to read"},
				"offset": {"type": "integer", "description": "Line to start from"},
				"format": {"type": "string", "enum": ["text", "json"]}
			},
			"required": ["path"]
		}`,
	}))
	registry.Add(Codemode(catalogTool{
		name:        "lingzhu_models",
		description: "List models.",
		schema:      `{"type": "object", "properties": {"query": {"type": "string"}}}`,
	}))
	registry.Add(catalogTool{name: "execute", description: "Code Mode.", schema: `{"type":"object"}`})
	return registry
}

func TestDefinitionsSplitDirectAndCodemode(t *testing.T) {
	registry := NewRegistry()
	registry.Add(Direct(catalogTool{name: "read", description: "Read.", schema: `{"type":"object"}`}))
	registry.Add(Codemode(catalogTool{name: "lingzhu_models", description: "List models.", schema: `{"type":"object"}`}))
	registry.Add(catalogTool{name: "execute", description: "Code Mode.", schema: `{"type":"object"}`})

	defs := registry.Definitions()
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Name)
	}
	if strings.Join(names, ",") != "execute,read" {
		t.Fatalf("direct definitions should hold read and execute: %v", names)
	}

	code := registry.CodemodeDefinitions()
	if len(code) != 1 || code[0].Name != "lingzhu_models" {
		t.Fatalf("code mode definitions should only hold the eligible tool: %+v", code)
	}
}

func TestCatalogSkipsDirectTools(t *testing.T) {
	registry := NewRegistry()
	registry.Add(Direct(catalogTool{name: "shell", description: "Shell.", schema: `{"type":"object"}`}))
	registry.Add(Codemode(catalogTool{name: "lingzhu_models", description: "List models.", schema: `{"type":"object"}`}))

	entries := Catalog(registry)
	if len(entries) != 1 || entries[0].Path != "lingzhu_models" {
		t.Fatalf("direct tools must not enter the Code Mode catalog: %+v", entries)
	}
}

func TestCatalogSkipsExecute(t *testing.T) {
	entries := Catalog(catalogRegistry())
	for _, entry := range entries {
		if entry.Path == "execute" {
			t.Fatal("execute must not appear in its own catalog")
		}
	}
	if len(entries) != 2 {
		t.Fatalf("want two entries, got %d", len(entries))
	}
}

func TestCatalogSignature(t *testing.T) {
	entries := Catalog(catalogRegistry())
	var read CatalogEntry
	for _, entry := range entries {
		if entry.Path == "read" {
			read = entry
		}
	}
	for _, want := range []string{"read({", "path: string", "offset?: number", `format?: "text" | "json"`, "File or directory to read", "}): Promise<string>"} {
		if !strings.Contains(read.Signature, want) {
			t.Fatalf("signature missing %q:\n%s", want, read.Signature)
		}
	}
}

func TestCatalogSectionExplainsNestedResults(t *testing.T) {
	section := catalogRegistry().Catalog()
	for _, want := range []string{"# Code Mode", "Code Mode tools (call only inside `execute`)", "text output as a string"} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "also available as normal tool calls") {
		t.Fatalf("catalog tools must not offer direct calls:\n%s", section)
	}
}

func TestCatalogDescriptionKeepsFirstLine(t *testing.T) {
	entries := Catalog(catalogRegistry())
	for _, entry := range entries {
		if entry.Path != "read" {
			continue
		}
		if strings.Contains(entry.Description, "Second line") {
			t.Fatalf("description should stop at the first line: %q", entry.Description)
		}
	}
}

func TestCatalogSectionMentionsSearch(t *testing.T) {
	section := catalogRegistry().Catalog()
	for _, want := range []string{"# Code Mode", "tools.read({", "The catalog is complete"} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q:\n%s", want, section)
		}
	}
}

func TestSearchCatalogPages(t *testing.T) {
	registry := catalogRegistry()
	items, remaining, next := searchEntries(Catalog(registry), "", "", 1, 0)
	if len(items) != 1 || remaining != 1 || next != 1 {
		t.Fatalf("unexpected first page: %v remaining=%d next=%d", items, remaining, next)
	}
	items, remaining, next = searchEntries(Catalog(registry), "", "", 1, 1)
	if len(items) != 1 || remaining != 0 || next != -1 {
		t.Fatalf("unexpected last page: %v remaining=%d next=%d", items, remaining, next)
	}
}

func TestSearchCatalogFilters(t *testing.T) {
	items, _, _ := searchEntries(Catalog(catalogRegistry()), "models", "", 20, 0)
	if len(items) != 1 || items[0].Path != "lingzhu_models" {
		t.Fatalf("query filter failed: %v", items)
	}
	items, _, _ = searchEntries(Catalog(catalogRegistry()), "", "lingzhu", 20, 0)
	if len(items) != 1 || items[0].Namespace != "lingzhu" {
		t.Fatalf("namespace filter failed: %v", items)
	}
}

func TestCatalogSectionExecuteOnly(t *testing.T) {
	section := catalogRegistry().CatalogExecuteOnly()
	for _, want := range []string{
		"The tools listed below cannot be called directly",
		"The catalog is complete",
		"## Code Mode tools (call only inside `execute`)",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("execute-only section missing %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "also available as normal tool calls") {
		t.Fatalf("execute-only section must not offer direct calls:\n%s", section)
	}
}
