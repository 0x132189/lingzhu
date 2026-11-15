package artifact

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/permission"
	"lingzhu/internal/tool"
)

func allowAll() *permission.Manager {
	return permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})
}

func newManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	manager, err := New(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if manager == nil {
		t.Fatal("artifact manager should be enabled by default")
	}
	return manager, dir
}

func TestCreateGetPersist(t *testing.T) {
	manager, dir := newManager(t)
	files := map[string]string{
		"index.html":     "<h1>Hello</h1>",
		"assets/app.js":  "console.log(1)",
		"assets/app.css": "body{color:red}",
	}
	info, err := manager.Create("Demo", files, "", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !idPattern.MatchString(info.ID) || info.Entry != "index.html" || info.Title != "Demo" {
		t.Fatalf("unexpected info: %+v", info)
	}
	if info.SessionID != "s1" || len(info.Files) != 3 {
		t.Fatalf("unexpected metadata: %+v", info)
	}
	if info.Files[0] != "assets/app.css" || info.Files[2] != "index.html" {
		t.Fatalf("files not sorted: %+v", info.Files)
	}
	if !strings.HasSuffix(info.URL, "/artifacts/"+info.ID+"/raw/index.html") {
		t.Fatalf("unexpected url: %q", info.URL)
	}

	content, err := manager.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(content.Files) != 3 || content.Files["assets/app.js"] != "console.log(1)" {
		t.Fatalf("unexpected content: %+v", content.Files)
	}

	reopened, err := New(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := reopened.List()
	if err != nil || len(listed) != 1 || listed[0].ID != info.ID {
		t.Fatalf("artifact did not persist: %+v %v", listed, err)
	}
	stored, err := reopened.Get(info.ID)
	if err != nil || stored.Title != "Demo" {
		t.Fatalf("reopened get failed: %+v %v", stored, err)
	}
}

func TestEntryResolution(t *testing.T) {
	manager, _ := newManager(t)
	info, err := manager.Create("Sole", map[string]string{"report.txt": "hi"}, "", "")
	if err != nil || info.Entry != "report.txt" {
		t.Fatalf("sole file entry: %+v %v", info, err)
	}
	info, err = manager.Create("Markdown", map[string]string{"index.md": "# hi"}, "", "")
	if err != nil || info.Entry != "index.md" {
		t.Fatalf("index.md entry: %+v %v", info, err)
	}
	if _, err := manager.Create("Multi", map[string]string{"a.txt": "a", "b.txt": "b"}, "", ""); err == nil {
		t.Fatal("multiple files without an entry should fail")
	}
	info, err = manager.Create("Chosen", map[string]string{"a.txt": "a", "b.txt": "b"}, "b.txt", "")
	if err != nil || info.Entry != "b.txt" {
		t.Fatalf("explicit entry: %+v %v", info, err)
	}
	if _, err := manager.Create("Bad", map[string]string{"a.txt": "a"}, "missing.txt", ""); err == nil {
		t.Fatal("missing entry should fail")
	}
}

func TestUpdateKeepsIDAndURL(t *testing.T) {
	manager, _ := newManager(t)
	created, err := manager.Create("Original", map[string]string{"index.html": "one"}, "", "s1")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := manager.Update(created.ID, "", map[string]string{"index.html": "two", "extra.txt": "x"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID || updated.URL != created.URL {
		t.Fatalf("identity changed: %+v -> %+v", created, updated)
	}
	if updated.Title != "Original" {
		t.Fatalf("title should be preserved: %q", updated.Title)
	}
	content, err := manager.Get(created.ID)
	if err != nil || content.Files["index.html"] != "two" || content.Files["extra.txt"] != "x" {
		t.Fatalf("files not replaced: %+v %v", content.Files, err)
	}
	if _, err := manager.Update("art_"+"0", "", map[string]string{"a.txt": "a"}, "", ""); err == nil {
		t.Fatal("missing artifact update should fail")
	}
}

func TestDelete(t *testing.T) {
	manager, dir := newManager(t)
	info, err := manager.Create("Gone", map[string]string{"index.html": "x"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Delete(info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Get(info.ID); err == nil {
		t.Fatal("deleted artifact should be gone")
	}
	if _, err := os.Stat(filepath.Join(dir, "artifacts", info.ID)); !os.IsNotExist(err) {
		t.Fatalf("artifact directory should be removed: %v", err)
	}
	if err := manager.Delete(info.ID); err == nil {
		t.Fatal("second delete should fail")
	}
}

func TestRejectsUnsafePathsAndLimits(t *testing.T) {
	manager, _ := newManager(t)
	for _, name := range []string{"../escape", "/abs", "a/../b", `a\b`, "", "a//b", "a?b"} {
		if _, err := manager.Create("Bad", map[string]string{name: "x"}, "", ""); err == nil {
			t.Fatalf("path %q should be rejected", name)
		}
	}
	big := strings.Repeat("x", maxFileBytes+1)
	if _, err := manager.Create("Big", map[string]string{"index.html": big}, "", ""); err == nil {
		t.Fatal("oversized file should be rejected")
	}
	if _, err := manager.Create("  ", map[string]string{"index.html": "x"}, "", ""); err == nil {
		t.Fatal("empty title should be rejected")
	}
	if _, err := manager.Create("Empty", map[string]string{}, "", ""); err == nil {
		t.Fatal("empty file map should be rejected")
	}

	info, err := manager.Create("Ok", map[string]string{"index.html": "ok"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.OpenFile(info.ID, "../artifact.json"); err == nil {
		t.Fatal("OpenFile should reject traversal")
	}
	if _, err := manager.Get("../" + info.ID); err == nil {
		t.Fatal("Get should reject invalid ids")
	}
}

func TestOpenFileMediaType(t *testing.T) {
	manager, _ := newManager(t)
	info, err := manager.Create("Page", map[string]string{"index.html": "<h1>x</h1>", "style.css": "body{}"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	file, mediaType, err := manager.OpenFile(info.ID, "style.css")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if mediaType != "text/css; charset=utf-8" {
		t.Fatalf("unexpected media type: %q", mediaType)
	}
	data := make([]byte, 16)
	n, _ := file.Read(data)
	if string(data[:n]) != "body{}" {
		t.Fatalf("unexpected file body: %q", data[:n])
	}
	if MediaType("a.svg") != "image/svg+xml" || MediaType("a.bin") != "application/octet-stream" {
		t.Fatal("unexpected media type mapping")
	}
}

func TestDisabledManager(t *testing.T) {
	off := false
	if manager, err := New(t.TempDir(), Config{Enabled: &off}); err != nil || manager != nil {
		t.Fatalf("disabled config should yield a nil manager: %v %v", manager, err)
	}
	var manager *Manager
	if manager.Enabled() {
		t.Fatal("nil manager is disabled")
	}
	if _, err := manager.Create("x", map[string]string{"index.html": "x"}, "", ""); err == nil {
		t.Fatal("disabled manager should reject create")
	}
}

func TestTools(t *testing.T) {
	manager, _ := newManager(t)
	set := map[string]tool.Tool{}
	for _, item := range Tools(manager) {
		set[item.Name()] = item
	}
	out, err := set["lingzhu_artifact_create"].Run(context.Background(), tool.Call{
		Name:      "lingzhu_artifact_create",
		Input:     json.RawMessage(`{"title":"Demo","files":{"index.html":"<h1>hi</h1>"}}`),
		SessionID: "s1",
		Perm:      allowAll(),
	})
	if err != nil || !strings.Contains(out, "Published Demo (art_") || !strings.Contains(out, "/artifacts/") {
		t.Fatalf("unexpected create: %q %v", out, err)
	}
	id := strings.TrimSpace(strings.Split(strings.Split(out, "(")[1], ")")[0])

	out, err = set["lingzhu_artifact_read"].Run(context.Background(), tool.Call{
		Name:  "lingzhu_artifact_read",
		Input: json.RawMessage(`{"id":"` + id + `"}`),
		Perm:  allowAll(),
	})
	if err != nil || !strings.Contains(out, "<h1>hi</h1>") || !strings.Contains(out, "entry: index.html") {
		t.Fatalf("unexpected read: %q %v", out, err)
	}

	out, err = set["lingzhu_artifact_update"].Run(context.Background(), tool.Call{
		Name:  "lingzhu_artifact_update",
		Input: json.RawMessage(`{"id":"` + id + `","title":"Renamed","files":{"index.html":"<h1>bye</h1>"}}`),
		Perm:  allowAll(),
	})
	if err != nil || !strings.Contains(out, "Updated Renamed") {
		t.Fatalf("unexpected update: %q %v", out, err)
	}

	deny := permission.New(permission.Rules{{Action: "lingzhu_artifact_delete", Resource: "*", Effect: permission.Deny}})
	if _, err := set["lingzhu_artifact_delete"].Run(context.Background(), tool.Call{
		Name:  "lingzhu_artifact_delete",
		Input: json.RawMessage(`{"id":"` + id + `"}`),
		Perm:  deny,
	}); err == nil {
		t.Fatal("denied delete should fail")
	}

	out, err = set["lingzhu_artifact_delete"].Run(context.Background(), tool.Call{
		Name:  "lingzhu_artifact_delete",
		Input: json.RawMessage(`{"id":"` + id + `"}`),
		Perm:  allowAll(),
	})
	if err != nil || out != "Deleted "+id {
		t.Fatalf("unexpected delete: %q %v", out, err)
	}
}
