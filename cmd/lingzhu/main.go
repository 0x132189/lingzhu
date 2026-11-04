// Command lingzhu is the core program entry point.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"lingzhu/internal/app"
	"lingzhu/internal/config"
	"lingzhu/internal/event"
	"lingzhu/internal/permission"
	"lingzhu/internal/provider"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "run":
		run(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("lingzhu", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `lingzhu - AI agent core

Usage:
  lingzhu serve [flags]      start the HTTP server
  lingzhu run <prompt>       run one conversation turn in the terminal
  lingzhu version            print the version

Environment:
  LINGZHU_CONFIG      config file path
  LINGZHU_ADDR        listen address, default 127.0.0.1:8787
  LINGZHU_DATA_DIR    data directory, default "data" next to config.json
  LINGZHU_WORKDIR     agent working directory, default the process directory
  LINGZHU_MODEL       model name
  LINGZHU_CONTEXT_WINDOW  model context size, 0 disables auto compaction
  LINGZHU_COMPACT_KEEP    recent tokens kept beside a summary, default 15000
  OPENAI_BASE_URL     model endpoint, default https://api.openai.com/v1
  OPENAI_API_KEY      endpoint API key
`)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", config.DefaultPath(), "config file path")
	addr := fs.String("addr", "", "listen address")
	data := fs.String("data", "", "data directory")
	model := fs.String("model", "", "model name")
	providerID := fs.String("provider", "", "provider id")
	workdir := fs.String("workdir", "", "agent working directory")
	_ = fs.Parse(args)

	cfg := mustConfig(*configPath, *addr, *data, *providerID, *model, *workdir)
	application, err := app.New(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	if err := application.Recover(context.Background()); err != nil {
		log.Printf("recover: %v", err)
	}
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           application.Server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("lingzhu %s listening on http://%s", version, cfg.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	// A desktop client can ask the core to stop through the shutdown endpoint;
	// signals do the same thing from a terminal.
	quit := make(chan struct{})
	var quitOnce sync.Once
	application.Server.OnShutdown(func() { quitOnce.Do(func() { close(quit) }) })

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// A desktop client that spawns this process sets LINGZHU_PARENT_PID; when
	// the client disappears, shut down instead of outliving it.
	watchParent(func() { quitOnce.Do(func() { close(quit) }) })

	select {
	case <-stop:
	case <-quit:
	}

	// Interrupt running turns first so their SSE handlers return, then close
	// the listener and wait for in-flight requests.
	log.Print("shutting down")
	runCtx, cancelRuns := context.WithTimeout(context.Background(), 10*time.Second)
	if err := application.Shutdown(runCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	cancelRuns()
	application.Server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("server shutdown: %v", err)
	}
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", config.DefaultPath(), "config file path")
	sessionID := fs.String("session", "cli", "session ID")
	addr := fs.String("addr", "", "listen address")
	data := fs.String("data", "", "data directory")
	model := fs.String("model", "", "model name")
	providerID := fs.String("provider", "", "provider id")
	workdir := fs.String("workdir", "", "agent working directory")
	_ = fs.Parse(args)

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		log.Fatal("run: prompt is empty")
	}

	cfg := mustConfig(*configPath, *addr, *data, *providerID, *model, *workdir)
	application, err := app.New(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := bufio.NewReader(os.Stdin)
	events, err := application.Agent.Run(ctx, *sessionID, prompt, provider.Selection{Provider: cfg.Provider, Model: cfg.Model})
	if err != nil {
		log.Fatalf("run: %v", err)
	}
	failed := false
	for ev := range events {
		switch ev.Type {
		case event.TextDelta:
			fmt.Print(ev.Data["text"])
		case event.ToolCall:
			fmt.Fprintf(os.Stderr, "\n[tool] %v %v\n", ev.Data["name"], ev.Data["arguments"])
		case event.ToolResult:
			fmt.Fprintf(os.Stderr, "[result] %v\n", ev.Data["output"])
		case event.CompactionStart:
			fmt.Fprint(os.Stderr, "\n[compact] summarizing older context\n")
		case event.Error:
			failed = true
			fmt.Fprintf(os.Stderr, "\n[error] %v\n", ev.Data["message"])
		case event.PermissionAsked:
			if err := askPermission(application, ev, stdin); err != nil {
				failed = true
				fmt.Fprintf(os.Stderr, "\n[error] %v\n", err)
				cancel()
			}
		}
	}
	fmt.Println()
	if failed {
		os.Exit(1)
	}
}

// askPermission asks the user about a pending permission request in the terminal.
func askPermission(application *app.App, ev event.Event, stdin *bufio.Reader) error {
	id, _ := ev.Data["id"].(string)
	action, _ := ev.Data["action"].(string)
	resources, _ := ev.Data["resources"].([]string)
	fmt.Fprintf(os.Stderr, "\n[permission] %s %s  [y] allow once / [a] always allow / [n] reject: ", action, strings.Join(resources, ", "))

	line, err := stdin.ReadString('\n')
	answer := strings.Trim(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(line)), "\ufeff"), "\x00 ")
	decision := permission.Reject
	switch answer {
	case "y", "yes":
		decision = permission.Once
	case "a", "always":
		decision = permission.Always
	}
	// Answer even when reading fails, otherwise the tool call waits forever.
	if replyErr := application.Agent.Reply(id, decision, ""); replyErr != nil {
		return replyErr
	}
	if err != nil && line == "" {
		return fmt.Errorf("permission: %w", err)
	}
	return nil
}

// mustConfig loads the config file and applies non-empty command-line overrides.
func mustConfig(path, addr, data, providerID, model, workdir string) config.Config {
	cfg, err := config.Load(path)
	if err != nil {
		log.Fatalf("config %s: %v", path, err)
	}
	if path != "" {
		log.Printf("config: %s", path)
	}
	if addr != "" {
		cfg.Addr = addr
	}
	if data != "" {
		cfg.DataDir = data
	}
	if providerID != "" {
		cfg.Provider = providerID
	}
	if model != "" {
		cfg.Model = model
	}
	if workdir != "" {
		cfg.Workdir = workdir
	}
	return cfg
}
