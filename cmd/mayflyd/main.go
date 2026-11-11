// Command mayflyd is the Mayfly control plane.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/httpapi"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/runner"
	"github.com/hayfever/mayfly/internal/session"
	"github.com/hayfever/mayfly/internal/version"
)

func main() {
	configPath := flag.String("config", envOr("MAYFLY_CONFIG", "config.yaml"), "path to config.yaml")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String())
		return
	}

	logger := newLogger()
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(2)
	}
	if cfg.AdminToken == "" {
		buf := make([]byte, 24)
		if _, err := rand.Read(buf); err != nil {
			logger.Error("generate admin token", "error", err)
			os.Exit(1)
		}
		cfg.AdminToken = hex.EncodeToString(buf)
		logger.Warn("MAYFLY_API_TOKEN is not set; generated a session token for this process", "admin_token", cfg.AdminToken)
	}

	store, err := artifact.New(context.Background(), cfg.Artifacts)
	if err != nil {
		logger.Error("configure artifact store", "error", err)
		os.Exit(2)
	}

	queueStore, err := queue.New(context.Background(), cfg.Queue)
	if err != nil {
		logger.Error("configure queue", "error", err)
		os.Exit(2)
	}
	defer func() {
		if err := queueStore.Close(); err != nil {
			logger.Warn("close queue", "error", err)
		}
	}()

	var sessionRunner runner.Runner
	var kube *k8s.Client
	switch cfg.Runtime.Backend {
	case "kubernetes":
		kube, err = k8s.NewClient(cfg.Sessions.Namespace)
		if err != nil {
			logger.Error("connect to kubernetes", "error", err)
			os.Exit(2)
		}
		sessionRunner = runner.NewKubernetes(kube)
	case "local":
		sessionRunner, err = runner.NewLocal(cfg.Runtime.AgentBinary, cfg.Runtime.StateDir, logger)
		if err != nil {
			logger.Error("configure local runtime", "error", err)
			os.Exit(2)
		}
	}

	h := hub.New(cfg.Sessions.EventBuffer)
	mgr, err := session.New(cfg, store, h, sessionRunner, kube, logger)
	if err != nil {
		logger.Error("create session manager", "error", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := mgr.Start(ctx); err != nil {
		logger.Error("start manager", "error", err)
		os.Exit(2)
	}

	// Accepted webhook work runs here, not in the request that accepted it.
	concurrency := cfg.Sessions.MaxConcurrent
	if concurrency <= 0 {
		concurrency = 8
	}
	worker := queue.NewWorker(queueStore, func(ctx context.Context, job queue.Job) error {
		if _, err := mgr.Create(ctx, job.Options); err != nil {
			if errors.Is(err, session.ErrCapacity) {
				return fmt.Errorf("%w: %v", queue.ErrRetryLater, err)
			}
			return err
		}
		return nil
	}, concurrency, mgr.FreeSlots, cfg.Queue.PollInterval.Std(), logger)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker.Run(ctx)
	}()

	server := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           httpapi.New(cfg, mgr, queueStore, logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		logger.Info("mayflyd listening",
			"addr", cfg.Server.Addr,
			"public_url", cfg.Server.PublicURL,
			"backend", cfg.Runtime.Backend,
			"artifact_store", cfg.Artifacts.Store,
			"version", version.Version,
			"commit", version.Commit,
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	select {
	case <-workerDone:
	case <-time.After(20 * time.Second):
		logger.Warn("queue worker did not stop in time; in-flight jobs will be retried")
	}
	mgr.Shutdown(shutdownCtx)
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
