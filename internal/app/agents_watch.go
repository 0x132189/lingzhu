package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lingzhu/internal/agents"
	"lingzhu/internal/config"
)

// agentWatchInterval is short enough to feel live and long enough that a
// directory walk is negligible beside a model request.
const agentWatchInterval = 2 * time.Second

// watchAgents keeps the shared registry in sync with the config file and the
// agent markdown roots. Config and agent files both feed the fingerprint so an
// edit is applied at the next step without restarting the core.
func watchAgents(ctx context.Context, registry *agents.Registry, cfg config.Config, workdir string) {
	roots := agents.Roots(cfg.Path, workdir)
	watch := &agentWatcher{configPath: cfg.Path, roots: roots}
	watch.quick, watch.content = watch.scan()
	ticker := time.NewTicker(agentWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// A stat-only pass is enough while nothing on disk changed; reading every
		// file each tick would cost more than the reload it guards.
		quick, content := watch.scan()
		if quick == watch.quick {
			continue
		}
		watch.quick = quick
		if content == watch.content {
			// An editor rewrote identical bytes, so nothing needs reloading.
			continue
		}
		watch.content = content
		if cfg.Path != "" {
			if latest, err := config.Load(cfg.Path); err == nil {
				cfg.Agents = latest.Agents
				cfg.DefaultAgent = latest.DefaultAgent
			} else {
				log.Printf("agents: reload config: %v", err)
			}
		}
		discovered := agents.Discover(roots, log.Printf)
		registry.Reload(cfg.Agents, discovered, cfg.DefaultAgent)
		log.Printf("agents: reloaded %d markdown profiles", len(discovered))
	}
}

// agentWatcher tracks the config and markdown files that feed the registry.
// quick is a stat-only signature; content hashes the bytes and only runs when
// the stat signature moved, so a steady state costs one stat per file.
type agentWatcher struct {
	configPath string
	roots      []string
	quick      string
	content    string
}

// scan returns the stat signature and the content hash of every watched file.
func (w *agentWatcher) scan() (quick, content string) {
	files := make([]string, 0, 8)
	if w.configPath != "" {
		files = append(files, w.configPath)
	}
	for _, root := range w.roots {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
				return nil
			}
			files = append(files, path)
			return nil
		})
	}
	sort.Strings(files)

	stat := sha256.New()
	body := sha256.New()
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		fmt.Fprintf(stat, "%s\x00%d\x00%d\n", filepath.ToSlash(path), info.Size(), info.ModTime().UnixNano())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		body.Write([]byte(filepath.ToSlash(path)))
		body.Write([]byte{0})
		body.Write(data)
		body.Write([]byte{0})
	}
	return hex.EncodeToString(stat.Sum(nil)), hex.EncodeToString(body.Sum(nil))
}
