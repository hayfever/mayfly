// Command mayfly-agent runs one ephemeral agent session. It is started by the
// control plane inside a hardened container.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hayden/mayfly/internal/agent"
	"github.com/hayden/mayfly/internal/sessioncfg"
	"github.com/hayden/mayfly/internal/version"
)

func main() {
	configPath := flag.String("config", os.Getenv("MAYFLY_CONFIG"), "path to session.json")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String())
		return
	}

	logger := newLogger()
	slog.SetDefault(logger)

	if *configPath == "" {
		logger.Error("no session config path: set --config or MAYFLY_CONFIG")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		logger.Error("read session config", "error", err)
		os.Exit(2)
	}
	var cfg sessioncfg.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		logger.Error("parse session config", "error", err)
		os.Exit(2)
	}
	token := os.Getenv("MAYFLY_SESSION_TOKEN")
	if token == "" {
		logger.Error("MAYFLY_SESSION_TOKEN is not set")
		os.Exit(2)
	}

	runtime, err := agent.New(cfg, token, logger)
	if err != nil {
		logger.Error("create runtime", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := runtime.Run(ctx); err != nil {
		logger.Error("session failed", "error", err)
		os.Exit(1)
	}
	logger.Info("session finished")
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("MAYFLY_LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
