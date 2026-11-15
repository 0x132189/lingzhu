package apptools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/event"
	"lingzhu/internal/permission"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
	"lingzhu/internal/worktree"
)

func testCatalog(t *testing.T) *provider.Catalog {
	t.Helper()
	catalog, err := provider.NewCatalog([]provider.ProviderSpec{
		{
			ID: "work", Name: "Work", BaseURL: "https://work.test/v1", APIKey: "k",
			Models: []provider.ModelSpec{
				{ID: "fast", Name: "Fast", ContextWindow: 32000},
				{ID: "smart", Name: "Smart", ContextWindow: 128000},
			},
		},
		{
			ID: "home", Name: "Home", BaseURL: "https://home.test/v1", APIKey: "k",
			Models: []provider.ModelSpec{{ID: "tiny", Name: "Tiny", ContextWindow: 8000}},
		},
	}, "work", "fast")
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func testRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "test"},
		{"config", "user.email", "test@example.com"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-q", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func toolSet(t *testing.T, store *session.Store, manager *worktree.Manager, catalog *provider.Catalog) map[string]tool.Tool {
	t.Helper()
	out := map[string]tool.Tool{}
	for _, item := range Tools(store, manager, catalog) {
		out[item.Name()] = item
	}
	return out
}

func runTool(t *testing.T, item tool.Tool, sessionID, input string) string {
	t.Helper()
	out, err := item.Run(context.Background(), tool.Call{
		Name:      item.Name(),
		Input:     json.RawMessage(input),
		SessionID: sessionID,
		Perm:      permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSessionRename(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Append("s1", event.User("hello")); err != nil {
		t.Fatal(err)
	}
	tools := toolSet(t, store, nil, testCatalog(t))
	out := runTool(t, tools["lingzhu_session_rename"], "s1", `{"title":"Renamed by model"}`)
	if !strings.Contains(out, "Renamed session s1") {
		t.Fatalf("unexpected output: %q", out)
	}
	info, err := store.Info("s1")
	if err != nil || info.Title != "Renamed by model" {
		t.Fatalf("title not stored: %+v %v", info, err)
	}
	if _, err := tools["lingzhu_session_rename"].Run(context.Background(), tool.Call{
		Input:     json.RawMessage(`{"title":""}`),
		SessionID: "s1",
		Perm:      permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}}),
	}); err == nil {
		t.Fatal("empty title should fail")
	}
}

func TestWorktreeTools(t *testing.T) {
	repo := testRepo(t)
	manager, err := worktree.New(repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := toolSet(t, nil, manager, testCatalog(t))

	created := runTool(t, tools["lingzhu_worktree_create"], "s1", `{"name":"agent-tree"}`)
	if !strings.HasPrefix(created, "Created worktree in ") || !strings.HasSuffix(strings.TrimSpace(created), ".") {
		t.Fatalf("unexpected create output: %q", created)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(created), "Created worktree in "), ".")
	if _, err := os.Stat(filepath.Join(path, "a.txt")); err != nil {
		t.Fatalf("worktree files missing: %v", err)
	}

	listed := runTool(t, tools["lingzhu_worktree_list"], "s1", `{}`)
	if !strings.Contains(listed, path) || !strings.Contains(listed, "[main]") {
		t.Fatalf("unexpected list: %q", listed)
	}

	out := runTool(t, tools["lingzhu_worktree_remove"], "s1", `{"directory":"`+strings.ReplaceAll(path, `\`, `\\`)+`"}`)
	if !strings.Contains(out, "Removed worktree") {
		t.Fatalf("unexpected remove output: %q", out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree should be gone: %v", err)
	}
}

func TestWorktreeToolsWithoutRepository(t *testing.T) {
	tools := toolSet(t, nil, nil, testCatalog(t))
	if _, err := tools["lingzhu_worktree_list"].Run(context.Background(), tool.Call{
		Input: json.RawMessage(`{}`),
		Perm:  permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}}),
	}); err == nil {
		t.Fatal("worktree list without a repository should fail")
	}
}

func TestModelsSearchPaging(t *testing.T) {
	tools := toolSet(t, nil, nil, testCatalog(t))
	all := runTool(t, tools["lingzhu_models"], "s1", `{}`)
	if !strings.Contains(all, "work/fast") || !strings.Contains(all, "work/smart") || !strings.Contains(all, "home/tiny") || !strings.Contains(all, "Showing 1-3 of 3") {
		t.Fatalf("unexpected models output: %q", all)
	}
	filtered := runTool(t, tools["lingzhu_models"], "s1", `{"query":"smart"}`)
	if !strings.Contains(filtered, "work/smart") || strings.Contains(filtered, "work/fast") {
		t.Fatalf("query filter failed: %q", filtered)
	}
	providerOnly := runTool(t, tools["lingzhu_models"], "s1", `{"provider":"home"}`)
	if !strings.Contains(providerOnly, "home/tiny") || strings.Contains(providerOnly, "work/") {
		t.Fatalf("provider filter failed: %q", providerOnly)
	}
	paged := runTool(t, tools["lingzhu_models"], "s1", `{"limit":1,"offset":1}`)
	if !strings.Contains(paged, "Showing 2-2 of 3") || !strings.Contains(paged, "work/fast") {
		t.Fatalf("paging failed: %q", paged)
	}
}

func TestModelsTypeAndFamilies(t *testing.T) {
	catalog, err := provider.NewCatalog([]provider.ProviderSpec{
		{
			ID: "work", Name: "Work", BaseURL: "https://work.test/v1", APIKey: "k",
			Models: []provider.ModelSpec{
				{ID: "chat-1", Name: "Chat One", Family: "chat", Released: 100},
				{ID: "chat-2", Name: "Chat Two", Family: "chat", Released: 200},
				{ID: "gen-1", Name: "Image One", Type: "image", Family: "gen", Released: 100},
				{ID: "gen-2", Name: "Image Two", Type: "image", Family: "gen", Released: 200},
			},
		},
	}, "work", "chat-2")
	if err != nil {
		t.Fatal(err)
	}
	tools := toolSet(t, nil, nil, catalog)

	newest := runTool(t, tools["lingzhu_models"], "s1", `{}`)
	if !strings.Contains(newest, "work/chat-2") || strings.Contains(newest, "work/chat-1") || strings.Contains(newest, "gen-") {
		t.Fatalf("chat family filter failed: %q", newest)
	}
	everything := runTool(t, tools["lingzhu_models"], "s1", `{"all":true}`)
	if !strings.Contains(everything, "work/chat-1") || !strings.Contains(everything, "work/chat-2") {
		t.Fatalf("all families failed: %q", everything)
	}
	images := runTool(t, tools["lingzhu_models"], "s1", `{"type":"image"}`)
	if !strings.Contains(images, "work/gen-2") || strings.Contains(images, "work/gen-1") || strings.Contains(images, "chat-") {
		t.Fatalf("image type filter failed: %q", images)
	}
	if _, err := tools["lingzhu_models"].Run(context.Background(), tool.Call{
		Input: json.RawMessage(`{"type":"video"}`),
		Perm:  permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}}),
	}); err == nil {
		t.Fatal("unknown model type should fail")
	}
}
