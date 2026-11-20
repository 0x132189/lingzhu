package tool

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestFullCodemodePromotesBuiltins(t *testing.T) {
	registry := NewRegistry()
	registry.Add(catalogTool{name: "shell", description: "Shell.", schema: `{"type":"object"}`})
	registry.Add(catalogTool{name: "read", description: "Read.", schema: `{"type":"object"}`})
	registry.Add(catalogTool{name: "glob", description: "Glob.", schema: `{"type":"object"}`})
	registry.Add(Codemode(catalogTool{name: "remote", description: "Remote.", schema: `{"type":"object"}`}))
	registry.Add(catalogTool{name: "execute", description: "Code Mode.", schema: `{"type":"object"}`})

	registry.SetFullCodemode(true)

	defs := registry.Definitions()
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Name)
	}
	if strings.Contains(strings.Join(names, ","), "shell") || strings.Contains(strings.Join(names, ","), "read") {
		t.Fatalf("promoted tools should leave the direct list: %v", names)
	}
	if !strings.Contains(strings.Join(names, ","), "glob") {
		t.Fatalf("glob should stay direct: %v", names)
	}

	code := registry.CodemodeDefinitions()
	codeNames := make([]string, 0, len(code))
	for _, def := range code {
		codeNames = append(codeNames, def.Name)
	}
	if !strings.Contains(strings.Join(codeNames, ","), "shell") || !strings.Contains(strings.Join(codeNames, ","), "read") {
		t.Fatalf("promoted tools missing from Code Mode: %v", codeNames)
	}
	if _, ok := registry.CodemodeGet("shell"); !ok {
		t.Fatal("promoted shell should be callable inside execute")
	}
	if _, ok := registry.CodemodeGet("glob"); ok {
		t.Fatal("glob should stay direct-only")
	}
	if catalog := registry.Catalog(); !strings.Contains(catalog, "tools.shell") || !strings.Contains(catalog, "tools.remote") {
		t.Fatalf("promoted tools missing from catalog: %q", catalog)
	}

	registry.SetFullCodemode(false)
	if _, ok := registry.CodemodeGet("shell"); ok {
		t.Fatal("shell should be direct-only again")
	}
}

func TestRegistryRemoveInvalidatesCatalog(t *testing.T) {
	registry := NewRegistry()
	registry.Add(Codemode(catalogTool{name: "remote", description: "Remote.", schema: `{"type":"object"}`}))
	if catalog := registry.Catalog(); !strings.Contains(catalog, "remote") {
		t.Fatalf("tool missing from catalog: %q", catalog)
	}
	if !registry.Remove("remote") {
		t.Fatal("expected the tool to be removed")
	}
	if registry.Remove("remote") {
		t.Fatal("removing twice should report false")
	}
	if catalog := registry.Catalog(); strings.Contains(catalog, "remote") {
		t.Fatalf("removed tool still in catalog: %q", catalog)
	}
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

func TestCatalogEntryJSONTags(t *testing.T) {
	data, err := json.Marshal(CatalogEntry{Path: "tools.echo", Signature: "echo()", Description: "Echo.", Namespace: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"path":"tools.echo"`, `"signature":"echo()"`, `"description":"Echo."`, `"namespace":"echo"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("catalog entry missing %s: %s", want, data)
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

func TestCatalogSectionMentionsCatalogOnlyRule(t *testing.T) {
	section := catalogRegistry().Catalog()
	if !strings.Contains(section, "Never call or recommend a tool that is not in this catalog") {
		t.Fatalf("catalog must forbid tools outside the catalog:\n%s", section)
	}
}

func TestCatalogPartialSectionMentionsSearchSignature(t *testing.T) {
	registry := NewRegistry()
	for i := 0; i < 200; i++ {
		registry.Add(Codemode(catalogTool{
			name:        fmt.Sprintf("sample_%03d", i),
			description: strings.Repeat("x", 40),
			schema:      `{"type":"object"}`,
		}))
	}
	section := registry.Catalog()
	for _, want := range []string{"The catalog is partial", "search(query: string |", "remaining: number", "next: { offset: number } | null"} {
		if !strings.Contains(section, want) {
			t.Fatalf("partial catalog missing %q:\n%s", want, section)
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
