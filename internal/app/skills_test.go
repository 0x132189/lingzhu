package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/config"
	"lingzhu/internal/skill"
)

func TestWatchSkillsReloadsChangedContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "demo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("---\ndescription: demo\n---\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("one")
	manager := skill.Discover([]string{root}, t.Logf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchSkills(ctx, manager, func() []string { return []string{root} }, func(string) bool { return true }, t.Logf, 20*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	write("two")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if item, ok := manager.Get("demo"); ok && strings.Contains(item.Content, "two") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("watcher did not reload changed content: %+v", manager.List())
}

func TestWatchSkillsReloadsResourceFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "demo", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: demo\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := skill.Discover([]string{root}, t.Logf)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchSkills(ctx, manager, func() []string { return []string{root} }, func(string) bool { return true }, t.Logf, 20*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	resource := filepath.Join(root, "demo", "scripts", "run.sh")
	if err := os.MkdirAll(filepath.Dir(resource), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resource, []byte("echo run"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if item, ok := manager.Get("demo"); ok {
			for _, file := range item.Files {
				if strings.HasSuffix(filepath.ToSlash(file), "scripts/run.sh") {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("watcher did not refresh the resource file list: %+v", manager.List())
}

func TestSkillsStateRefreshReloadsLocalRoot(t *testing.T) {
	root := t.TempDir()
	manager := skill.Discover([]string{root}, t.Logf)
	cfg := config.Config{Skills: skill.Config{Paths: []string{root}}}
	state := newSkillsState(manager, []string{root}, cfg, root, t.Logf, func(string) bool { return true })

	path := filepath.Join(root, "fresh", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: fresh\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := state.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Get("fresh"); !ok {
		t.Fatalf("refresh did not discover the new skill: %+v", manager.List())
	}
}

func TestSkillSignatureTracksMissingRoots(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	if !strings.Contains(skillSignature([]string{missing}), "missing") {
		t.Fatal("missing root should be part of the signature")
	}
	if err := os.MkdirAll(missing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(missing, "SKILL.md"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(skillSignature([]string{missing}), "missing\n") {
		t.Fatal("created root should not be marked missing")
	}
}
