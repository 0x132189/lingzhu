package format

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func fakeBinary(t *testing.T, workdir, name string) {
	t.Helper()
	writeTestFile(t, filepath.Join(workdir, "node_modules", ".bin", name), "#!/bin/sh\n")
}

func statusByName(items []Status) map[string]Status {
	out := make(map[string]Status, len(items))
	for _, item := range items {
		out[item.Name] = item
	}
	return out
}

func TestBuiltinCoverage(t *testing.T) {
	t.Setenv("PATH", "")
	workdir := t.TempDir()
	for _, name := range []string{
		"gofmt", "mix", "oxfmt", "prettier", "biome", "zig", "clang-format", "ktlint",
		"ruff", "rubocop", "standardrb", "htmlbeautifier", "dart", "ocamlformat",
		"terraform", "latexindent", "gleam", "shfmt", "nixfmt", "rustfmt",
		"ormolu", "cljfmt", "dfmt", "php-cs-fixer",
	} {
		fakeBinary(t, workdir, name)
	}
	writeTestFile(t, filepath.Join(workdir, "package.json"), `{"devDependencies":{"prettier":"1","oxfmt":"1"}}`)
	writeTestFile(t, filepath.Join(workdir, "biome.json"), `{}`)
	writeTestFile(t, filepath.Join(workdir, ".clang-format"), `BasedOnStyle: LLVM`)
	writeTestFile(t, filepath.Join(workdir, ".ocamlformat"), `version=0.26.0`)
	writeTestFile(t, filepath.Join(workdir, "ruff.toml"), `line-length = 100`)
	writeTestFile(t, filepath.Join(workdir, "composer.json"), `{"require-dev":{"laravel/pint":"1","friendsofphp/php-cs-fixer":"1"}}`)
	writeTestFile(t, filepath.Join(workdir, "vendor", "bin", "pint"), "#!/bin/sh\n")
	writeTestFile(t, filepath.Join(workdir, "vendor", "bin", "php-cs-fixer"), "#!/bin/sh\n")

	manager := New(workdir, Config{}, t.Logf)
	statuses := statusByName(manager.Status())
	expected := []string{
		"gofmt", "mix", "oxfmt", "prettier", "biome", "zig", "clang-format", "ktlint",
		"ruff", "rubocop", "standardrb", "htmlbeautifier", "dart", "ocamlformat",
		"terraform", "latexindent", "gleam", "shfmt", "nixfmt", "rustfmt",
		"pint", "php-cs-fixer", "ormolu", "cljfmt", "dfmt",
	}
	for _, name := range expected {
		if _, ok := statuses[name]; !ok {
			t.Fatalf("builtin %q missing: %+v", name, statuses)
		}
	}
	if got := statuses["zig"]; !strings.Contains(strings.Join(got.Command, " "), "fmt") || !contains(got.Extensions, ".zig") {
		t.Fatalf("unexpected zig: %+v", got)
	}
	if got := statuses["ktlint"]; !strings.Contains(strings.Join(got.Command, " "), "-F") || !contains(got.Extensions, ".kts") {
		t.Fatalf("unexpected ktlint: %+v", got)
	}
	if got := statuses["mix"]; !contains(got.Extensions, ".ex") || !contains(got.Extensions, ".heex") {
		t.Fatalf("unexpected mix: %+v", got)
	}
	if got := statuses["htmlbeautifier"]; !contains(got.Extensions, ".html.erb") {
		t.Fatalf("unexpected htmlbeautifier: %+v", got)
	}
	if got := statuses["pint"]; !strings.Contains(strings.Join(got.Command, " "), "vendor") {
		t.Fatalf("pint should use vendor/bin: %+v", got)
	}
	if got := statuses["prettier"]; !contains(got.Extensions, ".svelte") || !contains(got.Extensions, ".graphql") {
		t.Fatalf("unexpected prettier extensions: %+v", got)
	}
}

func TestJSBuiltinsRequireProjectEvidence(t *testing.T) {
	t.Setenv("PATH", "")
	workdir := t.TempDir()
	for _, name := range []string{"prettier", "oxfmt", "biome"} {
		fakeBinary(t, workdir, name)
	}

	statuses := statusByName(New(workdir, Config{}, t.Logf).Status())
	for _, name := range []string{"prettier", "oxfmt", "biome"} {
		if _, ok := statuses[name]; ok {
			t.Fatalf("%s should not run without project evidence", name)
		}
	}

	writeTestFile(t, filepath.Join(workdir, "package.json"), `{"dependencies":{"prettier":"1"}}`)
	statuses = statusByName(New(workdir, Config{}, t.Logf).Status())
	if _, ok := statuses["prettier"]; !ok {
		t.Fatal("prettier dependency should enable the builtin")
	}
	if _, ok := statuses["oxfmt"]; ok {
		t.Fatal("oxfmt without a dependency should stay disabled")
	}

	writeTestFile(t, filepath.Join(workdir, "biome.json"), `{}`)
	statuses = statusByName(New(workdir, Config{}, t.Logf).Status())
	if _, ok := statuses["biome"]; !ok {
		t.Fatal("biome.json should enable biome")
	}
}

func TestConfigGatedBuiltins(t *testing.T) {
	t.Setenv("PATH", "")
	workdir := t.TempDir()
	for _, name := range []string{"clang-format", "ocamlformat", "ruff"} {
		fakeBinary(t, workdir, name)
	}

	statuses := statusByName(New(workdir, Config{}, t.Logf).Status())
	for _, name := range []string{"clang-format", "ocamlformat", "ruff"} {
		if _, ok := statuses[name]; ok {
			t.Fatalf("%s should not run without config", name)
		}
	}

	writeTestFile(t, filepath.Join(workdir, ".clang-format"), `BasedOnStyle: LLVM`)
	writeTestFile(t, filepath.Join(workdir, ".ocamlformat"), `version=0.26.0`)
	writeTestFile(t, filepath.Join(workdir, "pyproject.toml"), `[tool.ruff]
line-length = 100
`)
	statuses = statusByName(New(workdir, Config{}, t.Logf).Status())
	for _, name := range []string{"clang-format", "ocamlformat", "ruff"} {
		if _, ok := statuses[name]; !ok {
			t.Fatalf("%s should be enabled by its config", name)
		}
	}

	// pyproject without the ruff table and without a dependency keeps ruff off.
	other := t.TempDir()
	fakeBinary(t, other, "ruff")
	writeTestFile(t, filepath.Join(other, "pyproject.toml"), `[project]
name = "demo"
`)
	if _, ok := statusByName(New(other, Config{}, t.Logf).Status())["ruff"]; ok {
		t.Fatal("ruff without configuration or dependency should stay disabled")
	}
	writeTestFile(t, filepath.Join(other, "pyproject.toml"), `[project]
name = "demo"
dependencies = ["ruff>=0.4"]
`)
	if _, ok := statusByName(New(other, Config{}, t.Logf).Status())["ruff"]; !ok {
		t.Fatal("ruff dependency should enable the builtin")
	}
}

func TestPHPCsFixerCanBeEnabledByConfig(t *testing.T) {
	t.Setenv("PATH", "")
	workdir := t.TempDir()
	fakeBinary(t, workdir, "php-cs-fixer")
	if _, ok := statusByName(New(workdir, Config{}, t.Logf).Status())["php-cs-fixer"]; ok {
		t.Fatal("php-cs-fixer needs config or composer dependency")
	}
	writeTestFile(t, filepath.Join(workdir, ".php-cs-fixer.dist.php"), `<?php return [];`)
	status := statusByName(New(workdir, Config{}, t.Logf).Status())["php-cs-fixer"]
	if !contains(status.Extensions, ".php") {
		t.Fatalf("php-cs-fixer should handle php: %+v", status)
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
