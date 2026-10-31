// Command lingzhu 是核心程序入口。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"lingzhu/internal/app"
	"lingzhu/internal/config"
	"lingzhu/internal/event"
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

用法:
  lingzhu serve [flags]      启动 HTTP 服务
  lingzhu run <prompt>       在终端执行一轮对话
  lingzhu version            打印版本

环境变量:
  LINGZHU_CONFIG      配置文件路径
  LINGZHU_ADDR        监听地址，默认 127.0.0.1:8787
  LINGZHU_DATA_DIR    数据目录，默认 config.json 同级的 data
  LINGZHU_MODEL       模型名
  OPENAI_BASE_URL     模型接口，默认 https://api.openai.com/v1
  OPENAI_API_KEY      接口密钥
`)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", config.DefaultPath(), "配置文件路径")
	addr := fs.String("addr", "", "监听地址")
	data := fs.String("data", "", "数据目录")
	model := fs.String("model", "", "模型名")
	_ = fs.Parse(args)

	cfg := mustConfig(*configPath, *addr, *data, *model)
	application, err := app.New(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
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

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", config.DefaultPath(), "配置文件路径")
	sessionID := fs.String("session", "cli", "会话 ID")
	addr := fs.String("addr", "", "监听地址")
	data := fs.String("data", "", "数据目录")
	model := fs.String("model", "", "模型名")
	_ = fs.Parse(args)

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		log.Fatal("run: prompt is empty")
	}

	cfg := mustConfig(*configPath, *addr, *data, *model)
	application, err := app.New(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	events, err := application.Agent.Run(context.Background(), *sessionID, prompt)
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
		case event.Error:
			failed = true
			fmt.Fprintf(os.Stderr, "\n[error] %v\n", ev.Data["message"])
		}
	}
	fmt.Println()
	if failed {
		os.Exit(1)
	}
}

// mustConfig 读取配置文件并叠加命令行参数，命令行非空才覆盖。
func mustConfig(path, addr, data, model string) config.Config {
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
	if model != "" {
		cfg.Model = model
	}
	return cfg
}
