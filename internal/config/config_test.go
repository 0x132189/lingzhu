package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/global"
)

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"addr":":9000","data_dir":"data","model":"m","max_steps":4,"workdir":"ws","context_window":64000,"compact_keep":8000}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":9000" || cfg.Model != "m" || cfg.MaxSteps != 4 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if want := filepath.Join(dir, "data"); cfg.DataDir != want {
		t.Fatalf("data dir should resolve next to config: want %q, got %q", want, cfg.DataDir)
	}
	if want := filepath.Join(dir, "ws"); cfg.Workdir != want {
		t.Fatalf("workdir should resolve next to config: want %q, got %q", want, cfg.Workdir)
	}
	if cfg.ContextWindow != 64000 || cfg.CompactKeep != 8000 {
		t.Fatalf("unexpected context settings: %+v", cfg)
	}
	if cfg.Path != path {
		t.Fatalf("config path not recorded: want %q, got %q", path, cfg.Path)
	}
}

func TestSavePermissionPreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"addr":":9000","custom":{"keep":true},"permission":[{"action":"read","resource":"*.env","effect":"ask"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := []Rule{
		{Action: "*", Resource: "*", Effect: "allow"},
		{Action: "bash", Resource: "git *", Effect: "allow"},
	}
	if err := SavePermission(path, rules); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if file["addr"] != ":9000" {
		t.Fatalf("other fields should be preserved: %+v", file)
	}
	if custom, ok := file["custom"].(map[string]any); !ok || custom["keep"] != true {
		t.Fatalf("unknown fields should be preserved: %+v", file["custom"])
	}
	// Approvals go to their own key so they can never be confused with the
	// configured permission rules.
	entries, ok := file["permission_always"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("learned rules should be written to permission_always: %+v", file["permission_always"])
	}
	if last, _ := entries[1].(map[string]any); last["action"] != "bash" || last["resource"] != "git *" {
		t.Fatalf("unexpected last rule: %+v", entries[1])
	}
	configured, ok := file["permission"].([]any)
	if !ok || len(configured) != 1 {
		t.Fatalf("configured permission must stay untouched: %+v", file["permission"])
	}
	if first, _ := configured[0].(map[string]any); first["action"] != "read" {
		t.Fatalf("unexpected configured rule: %+v", configured[0])
	}
}

func TestZeroContextWindowDisablesCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"context_window":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ContextWindow != 0 {
		t.Fatalf("explicit zero should disable compaction, got %d", cfg.ContextWindow)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"addr":":9000","model":"file-model"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINGZHU_ADDR", ":7000")
	t.Setenv("LINGZHU_MODEL", "env-model")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":7000" || cfg.Model != "env-model" {
		t.Fatalf("env should override file: %+v", cfg)
	}
}

func TestMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:8787" || cfg.MaxSteps != 0 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.DataDir != global.Data() {
		t.Fatalf("data dir should default to the user data directory: %q", cfg.DataDir)
	}
}

func TestDefaultPathPrefersProjectFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("LINGZHU_CONFIG", "")
	t.Setenv("LINGZHU_CONFIG_DIR", filepath.Join(dir, "user"))

	if got, want := DefaultPath(), filepath.Join(dir, "user", "config.json"); got != want {
		t.Fatalf("without a project file the user config wins: got %q, want %q", got, want)
	}

	project := filepath.Join(dir, "lingzhu.json")
	if err := os.WriteFile(project, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DefaultPath(); got != project {
		t.Fatalf("a project file should win: got %q, want %q", got, project)
	}
}

func TestDefaultPathNeverReadsBuildDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("LINGZHU_CONFIG", "")
	t.Setenv("LINGZHU_CONFIG_DIR", filepath.Join(dir, "user"))

	legacy := filepath.Join(dir, "build")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := DefaultPath(), filepath.Join(dir, "user", "config.json"); got != want {
		t.Fatalf("build/config.json must not be discovered: got %q, want %q", got, want)
	}
}

func TestBrokenFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("want error for invalid json")
	}
}

func TestExplicitEmptyProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"providers":{},"provider":"","model":""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ProvidersSet || len(cfg.Providers) != 0 {
		t.Fatalf("explicit empty providers = %+v", cfg.Providers)
	}
}

func TestLoadProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"provider":"work","model":"fast","providers":{"work":{"name":"Work","base_url":"https://work.test/v1/","api_key_env":"WORK_KEY","models":[{"id":"fast","name":"Fast","context_window":32000}]}}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "work" || cfg.Model != "fast" {
		t.Fatalf("unexpected selection: %+v", cfg)
	}
	provider := cfg.Providers["work"]
	if provider.Name != "Work" || provider.BaseURL != "https://work.test/v1" {
		t.Fatalf("unexpected provider: %+v", provider)
	}
	if len(provider.Models) != 1 || provider.Models[0].ContextWindow == nil || *provider.Models[0].ContextWindow != 32000 {
		t.Fatalf("unexpected models: %+v", provider.Models)
	}
}

func TestLoadMCPConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"mcp":{"servers":{"demo":{"type":"local","command":["node","server.js"],"environment":{"TOKEN":"x"}}}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	server, ok := cfg.MCP.Servers["demo"]
	if !ok || len(server.Command) != 2 || server.Environment["TOKEN"] != "x" {
		t.Fatalf("mcp config not loaded: %+v", cfg.MCP)
	}
}

func TestLoadSkillConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	list := `{"skills":["one","two"]}`
	if err := os.WriteFile(path, []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Skills.Paths) != 2 {
		t.Fatalf("skills array not loaded: %+v", cfg.Skills)
	}

	object := `{"skills":{"paths":["three"],"urls":["https://example.com"]}}`
	if err := os.WriteFile(path, []byte(object), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Skills.Paths) != 1 || len(cfg.Skills.URLs) != 1 {
		t.Fatalf("skills object not loaded: %+v", cfg.Skills)
	}
}

func TestLoadPluginConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"plugins":{"demo":{"command":["node","plugin.js"],"environment":{"TOKEN":"x"}}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	plugin, ok := cfg.Plugins.Plugins["demo"]
	if !ok || len(plugin.Command) != 2 || plugin.Environment["TOKEN"] != "x" {
		t.Fatalf("plugin config not loaded: %+v", cfg.Plugins)
	}
}

func TestLoadAgentConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"default_agent":"plan","agents":{"build":{"description":"custom","model":"work/fast","steps":4,"permissions":[{"action":"edit","resource":"*","effect":"deny"}]}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultAgent != "plan" {
		t.Fatalf("default agent not loaded: %q", cfg.DefaultAgent)
	}
	build, ok := cfg.Agents.Agents["build"]
	if !ok || build.Model != "work/fast" || build.Steps != 4 || len(build.Permissions) != 1 {
		t.Fatalf("agent config not loaded: %+v", cfg.Agents)
	}
}

func TestLoadFormatterConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"formatter":{"fixer":{"command":["fix","$FILE"],"extensions":["txt"],"environment":{"A":"B"}}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	def, ok := cfg.Formatter.Formatters["fixer"]
	if !ok || len(def.Command) != 2 || len(def.Extensions) != 1 || def.Environment["A"] != "B" {
		t.Fatalf("formatter config not loaded: %+v", cfg.Formatter)
	}

	if err := os.WriteFile(path, []byte(`{"formatter":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Formatter.Enabled == nil || *cfg.Formatter.Enabled {
		t.Fatalf("formatter false not loaded: %+v", cfg.Formatter)
	}
}

func TestLoadOAuthConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"oauth":{"work":{"token_url":"https://example.com/token","client_id":"id","client_secret":"secret","scopes":["read"],"grant":"client_credentials"}},"providers":{"work":{"base_url":"https://api.example.com/v1","oauth":"work","models":[{"id":"m"}]}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := cfg.OAuth.Clients["work"]
	if !ok || client.ClientID != "id" || len(client.Scopes) != 1 || client.Grant != "client_credentials" {
		t.Fatalf("oauth config not loaded: %+v", cfg.OAuth)
	}
	if cfg.Providers["work"].OAuth != "work" {
		t.Fatalf("provider oauth reference missing: %+v", cfg.Providers["work"])
	}
}

func TestLoadWebSearchConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"websearch":{"provider":"exa","endpoint":"https://example.com/search","api_key_env":"SEARCH_KEY","max_results":3}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebSearch.Provider != "exa" || cfg.WebSearch.APIKeyEnv != "SEARCH_KEY" || cfg.WebSearch.MaxResults != 3 {
		t.Fatalf("websearch config not loaded: %+v", cfg.WebSearch)
	}
}

func TestLoadImageConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"image":{"provider":"generic","endpoint":"https://img.example/generate","model":"m","size":"512x512"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Image.Provider != "generic" || cfg.Image.Endpoint != "https://img.example/generate" || cfg.Image.Model != "m" || cfg.Image.Size != "512x512" {
		t.Fatalf("image config not loaded: %+v", cfg.Image)
	}
}

func TestLoadArtifactConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"artifact":{"enabled":false,"base_url":"https://artifacts.example"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Artifact.Enabled == nil || *cfg.Artifact.Enabled || cfg.Artifact.BaseURL != "https://artifacts.example" {
		t.Fatalf("artifact config not loaded: %+v", cfg.Artifact)
	}
}

func TestLoadScheduleConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"schedule":{"enabled":false}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Schedule.Enabled == nil || *cfg.Schedule.Enabled {
		t.Fatalf("schedule config not loaded: %+v", cfg.Schedule)
	}
}

func TestLoadShareConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"share":{"mode":"auto","endpoint":"https://share.example/upload","api_key_env":"SHARE_KEY","secret_env":"SHARE_SECRET","expiry":"7d"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Share.Mode != "auto" || cfg.Share.Endpoint != "https://share.example/upload" || cfg.Share.APIKeyEnv != "SHARE_KEY" || cfg.Share.SecretEnv != "SHARE_SECRET" || cfg.Share.Expiry != "7d" {
		t.Fatalf("share config not loaded: %+v", cfg.Share)
	}
}

func TestLoadKeepsAbsolutePaths(t *testing.T) {
	base := t.TempDir()
	absWork := t.TempDir()
	absData := t.TempDir()
	path := filepath.Join(base, "config.json")
	body := `{"workdir":"` + strings.ReplaceAll(absWork, `\`, `\\`) + `","data_dir":"` + strings.ReplaceAll(absData, `\`, `\\`) + `"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workdir != absWork {
		t.Fatalf("absolute workdir was rewritten: %q != %q", cfg.Workdir, absWork)
	}
	if cfg.DataDir != absData {
		t.Fatalf("absolute data_dir was rewritten: %q != %q", cfg.DataDir, absData)
	}
}

func TestResolvePathRelative(t *testing.T) {
	base := t.TempDir()
	got := resolvePath(base, "work")
	if got != filepath.Join(base, "work") {
		t.Fatalf("relative path not joined: %q", got)
	}
}

func TestUpdateMergesAndPreserves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"provider":"old","providers":{"fake":{"api_key":"secret"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Update(path, map[string]any{"provider": "fake", "model": "m", "max_steps": 4}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["provider"] != "fake" || stored["model"] != "m" || stored["max_steps"].(float64) != 4 {
		t.Fatalf("fields not merged: %s", data)
	}
	providers, ok := stored["providers"].(map[string]any)
	if !ok || providers["fake"].(map[string]any)["api_key"] != "secret" {
		t.Fatalf("existing credentials lost: %s", data)
	}
	if err := Update(path, map[string]any{"model": nil}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), `"model"`) {
		t.Fatalf("nil should remove the field: %s", data)
	}
	if err := Update("", map[string]any{"model": "x"}); err == nil {
		t.Fatal("empty path should fail")
	}
}

func TestLoadCommands(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"commands":{"review":{"description":"Review","template":"Review $ARGUMENTS.","agent":"plan","model":"test/m"}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := cfg.Commands["review"]
	if !ok || definition.Template != "Review $ARGUMENTS." || definition.Agent != "plan" || definition.Model != "test/m" {
		t.Fatalf("unexpected command config: %+v", cfg.Commands)
	}
}

func TestLoadMediaImageConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"media":{"image":{"auto_resize":false,"max_width":1200,"max_height":800,"max_base64_bytes":262144}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Media.AutoResizeEnabled() || cfg.Media.Image.MaxWidth != 1200 || cfg.Media.Image.MaxHeight != 800 || cfg.Media.Image.MaxBase64Bytes != 262144 {
		t.Fatalf("unexpected media config: %+v", cfg.Media)
	}
	record, err := Load(filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !record.Media.AutoResizeEnabled() || record.Media.Image.MaxWidth != 2000 {
		t.Fatalf("media defaults not applied: %+v", record.Media)
	}
}

func TestLoadPolicies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"policies":[{"action":"provider.use","resource":"*","effect":"deny"},{"action":"provider.use","resource":"anthropic","effect":"allow"},{"action":"bad","resource":"","effect":"deny"}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Policies) != 2 {
		t.Fatalf("invalid policy should be dropped: %+v", cfg.Policies)
	}
	if !cfg.Policies.Denied("provider.use", "openai") || cfg.Policies.Denied("provider.use", "anthropic") {
		t.Fatalf("unexpected policy evaluation: %+v", cfg.Policies)
	}
}

func TestLoadWarmingConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"warming":{"prompt":"stay","interval":"2m","duration":"1h"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Warming.Enabled || cfg.Warming.Prompt != "stay" || cfg.Warming.IntervalFor(0) != 2*time.Minute {
		t.Fatalf("unexpected warming config: %+v", cfg.Warming)
	}
	boolPath := filepath.Join(dir, "bool.json")
	if err := os.WriteFile(boolPath, []byte(`{"warming":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	boolCfg, err := Load(boolPath)
	if err != nil {
		t.Fatal(err)
	}
	if boolCfg.Warming.Enabled {
		t.Fatalf("boolean false should disable warming: %+v", boolCfg.Warming)
	}
}

func TestLoadToleratesUTF8BOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"max_steps":7}`)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSteps != 7 {
		t.Fatalf("config behind a BOM was not loaded: %+v", cfg)
	}
}

func TestLoadConfiguredInstructions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"instructions":["one.md","two.md"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Instructions) != 2 || cfg.Instructions[0] != "one.md" {
		t.Fatalf("instructions not loaded: %+v", cfg.Instructions)
	}
}

func TestLoadConfiguredReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"references":[{"name":"docs","path":"../docs","description":"Product docs"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.References) != 1 {
		t.Fatalf("references not loaded: %+v", cfg.References)
	}
	if cfg.References[0].Name != "docs" || cfg.References[0].Path != "../docs" || cfg.References[0].Description != "Product docs" {
		t.Fatalf("unexpected reference: %+v", cfg.References[0])
	}
}

func TestLoadCodemodeOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"codemode_only":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CodemodeOnly {
		t.Fatal("codemode_only was not loaded")
	}
}

func TestLoadFullCodemode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"full_codemode":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.FullCodemode {
		t.Fatal("full_codemode was not loaded")
	}
}

func TestForDirMergesGlobalAndProjectSkills(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "config.json")
	projectDir := filepath.Join(root, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte(`{"skills":{"paths":["global"],"urls":["https://global.example"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "lingzhu.json"), []byte(`{"skills":["project","https://project.example"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ForDir(projectDir, global)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Skills.Paths) != 2 || cfg.Skills.Paths[0] != "global" || cfg.Skills.Paths[1] != "project" {
		t.Fatalf("unexpected merged paths: %+v", cfg.Skills)
	}
	if len(cfg.Skills.URLs) != 2 || cfg.Skills.URLs[0] != "https://global.example" {
		t.Fatalf("unexpected merged urls: %+v", cfg.Skills)
	}
}
