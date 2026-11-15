package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentWatcherTracksAgentContent(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "lingzhu.json")
	if err := os.WriteFile(configPath, []byte(`{"agents":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(agentsDir, "reviewer.md")
	if err := os.WriteFile(file, []byte("---\ndescription: first\n---\nReview.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	watch := &agentWatcher{configPath: configPath, roots: []string{agentsDir}}
	firstQuick, firstContent := watch.scan()
	if quick, content := watch.scan(); quick != firstQuick || content != firstContent {
		t.Fatal("signature changed without a write")
	}

	// An edit moves both signatures.
	if err := os.WriteFile(file, []byte("---\ndescription: second\n---\nReview.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secondQuick, secondContent := watch.scan()
	if secondQuick == firstQuick || secondContent == firstContent {
		t.Fatal("signature did not change after an agent edit")
	}

	// A config edit moves both signatures too.
	if err := os.WriteFile(configPath, []byte(`{"agents":{"reviewer":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	thirdQuick, thirdContent := watch.scan()
	if thirdQuick == secondQuick || thirdContent == secondContent {
		t.Fatal("signature did not change after a config edit")
	}
}

func TestAgentWatcherSkipsReloadForIdenticalBytes(t *testing.T) {
	root := t.TempDir()
	agentsDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(agentsDir, "reviewer.md")
	body := []byte("---\ndescription: first\n---\nReview.\n")
	if err := os.WriteFile(file, body, 0o644); err != nil {
		t.Fatal(err)
	}
	watch := &agentWatcher{roots: []string{agentsDir}}
	firstQuick, firstContent := watch.scan()

	// An editor that rewrites the same bytes moves the stat signature but not
	// the content hash, so the watcher can skip the reload.
	if err := os.WriteFile(file, body, 0o644); err != nil {
		t.Fatal(err)
	}
	later := filepath.Join(agentsDir, "reviewer.md")
	if err := os.Chtimes(later, time.Now().Add(time.Minute), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	secondQuick, secondContent := watch.scan()
	if secondQuick == firstQuick {
		t.Fatal("a touch should move the stat signature")
	}
	if secondContent != firstContent {
		t.Fatal("identical bytes must not change the content hash")
	}
}
