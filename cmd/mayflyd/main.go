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
	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/httpapi"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/pluginsvc"
	"github.com/hayfever/mayfly/internal/project"
	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/runner"
	"github.com/hayfever/mayfly/internal/session"
	"github.com/hayfever/mayfly/internal/state"
	"github.com/hayfever/mayfly/internal/version"
	"github.com/hayfever/mayfly/internal/workflow"
)

func main() {
	configPath := flag.String("config", envOr("MAYFLY_CONFIG", "config.yaml"), "path to config.yaml")
	showVersion := flag.Bool("version", false, "print version and exit")
	checkBundle := flag.Bool("check", false, "load the config and project bundle, print the resolved plans, and exit")
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
	bundle, err := project.Load(cfg.Agents.Dirs, project.Options{
		Concurrency:        cfg.Sources.Concurrency,
		Timeout:            cfg.Sources.Timeout.Std(),
		DeniedEnv:          cfg.SecretEnvNames(),
		RequirePluginImage: cfg.Runtime.Backend == "kubernetes",
	})
	if err != nil {
		logger.Error("load project bundle", "error", err)
		os.Exit(2)
	}
	if *checkBundle {
		// The same agent, model, skill, and MCP checks the session manager
		// runs at startup, so a clean check means the control plane starts.
		if err := session.Validate(cfg, bundle); err != nil {
			logger.Error("validate agents", "error", err)
			os.Exit(2)
		}
		fmt.Print(bundle.Describe())
		return
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

	var journalStore journal.Durable
	var ledger journal.Ledger
	switch cfg.Journal.Backend {
	case "memory":
		memory := journal.NewMemory()
		journalStore, ledger = memory, memory
	case "bbolt":
		boltJournal, err := journal.OpenBolt(cfg.Journal.Bolt.Path)
		if err != nil {
			logger.Error("configure journal", "error", err)
			os.Exit(2)
		}
		defer func() {
			if err := boltJournal.Close(); err != nil {
				logger.Warn("close journal", "error", err)
			}
		}()
		journalStore, ledger = boltJournal, boltJournal
	default:
		logger.Error("configure journal", "error", fmt.Errorf("unknown backend %q", cfg.Journal.Backend))
		os.Exit(2)
	}

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

	stateStore, err := state.New(cfg.Sources.StatePath)
	if err != nil {
		logger.Error("open schedule state", "error", err)
		os.Exit(2)
	}
	defer func() {
		if err := stateStore.Close(); err != nil {
			logger.Warn("close schedule state", "error", err)
		}
	}()

	h := hub.New(cfg.Sessions.EventBuffer)
	mgr, err := session.New(cfg, bundle, store, h, journalStore, sessionRunner, kube, logger)
	if err != nil {
		logger.Error("create session manager", "error", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The tool gateway is the only path to plugin and remote MCP tools. Its
	// ledger lives in the journal store, so a call record and the journal that
	// references it are deleted together at finalize.
	manifests := make([]plugin.Manifest, 0, len(bundle.Plugins))
	for _, manifest := range bundle.Plugins {
		manifests = append(manifests, manifest)
	}
	mcpServers := make(map[string]config.MCPServerConfig, len(cfg.MCP.Servers))
	for _, server := range cfg.MCP.Servers {
		mcpServers[server.Name] = server
	}

	// Service plugins run somewhere the control plane owns: a reconciled
	// Deployment in the cluster, or a shim child on the local backend. The
	// gateway asks this function where to send a call and which key signs it.
	serviceURL, readyCheck := startServices(ctx, cfg, kube, mgr, manifests, logger)

	audit, auditErr := gateway.NewAudit(cfg.Plugins.AuditPath, logger)
	if auditErr != nil {
		logger.Error("configure plugin audit log", "error", auditErr)
		os.Exit(2)
	}
	toolGateway := gateway.New(gateway.Options{
		Ledger:    ledger,
		Sequencer: journalStore,
		Manifests: func() []plugin.Manifest { return manifests },
		MCPServer: func(name string) (config.MCPServerConfig, bool) {
			server, ok := mcpServers[name]
			return server, ok
		},
		ServiceURL:  serviceURL,
		MaxInflight: cfg.Plugins.MaxInflightCalls,
		Audit:       audit,
		Logger:      logger,
	})
	// Grant registration happens at session create and adoption, so the
	// manager needs the gateway before it starts.
	mgr.SetGateway(toolGateway)

	if err := mgr.Start(ctx); err != nil {
		logger.Error("start manager", "error", err)
		os.Exit(2)
	}

	workflowRunner, err := workflow.New(bundle, workflow.Options{
		Store:    store,
		Queue:    queueStore,
		State:    stateStore,
		Sessions: mgr,
		MaxBytes: int64(cfg.Sources.MaxArtifactBytes),
		Logger:   logger,
	})
	if err != nil {
		logger.Error("create workflow runner", "error", err)
		os.Exit(2)
	}
	if err := workflowRunner.Start(ctx); err != nil {
		logger.Error("start scheduler", "error", err)
		os.Exit(2)
	}
	defer workflowRunner.Stop()

	scheduleAPI := httpapi.ScheduleService{Run: workflowRunner.Run, List: workflowRunner.Schedules()}

	// Accepted webhook work runs here, not in the request that accepted it.
	concurrency := cfg.Sessions.MaxConcurrent
	if concurrency <= 0 {
		concurrency = 8
	}
	handler := queue.Composite(queue.SessionHandler(mgr.Create), workflowRunner.Handle)
	worker := queue.NewWorker(queueStore, handler, concurrency, mgr.FreeSlots, cfg.Queue.PollInterval.Std(), logger)
	worker.SetTimeout(queue.KindSource, cfg.Sources.JobTimeout.Std())
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker.Run(ctx)
	}()

	server := &http.Server{
		Addr: cfg.Server.Addr,
		Handler: httpapi.New(cfg, mgr, queueStore, logger,
			httpapi.WithSchedules(scheduleAPI),
			httpapi.WithGateway(toolGateway),
			httpapi.WithReadyCheck(readyCheck),
		).Handler(),
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

// startServices starts the service plugin workloads and returns the gateway's
// service lookup and the readiness check their required plugins need.
func startServices(ctx context.Context, cfg *config.Config, kube *k8s.Client, mgr *session.Manager, manifests []plugin.Manifest, logger *slog.Logger) (func(string) (string, []byte, error), func() error) {
	manifestsFn := func() []plugin.Manifest { return manifests }
	switch cfg.Runtime.Backend {
	case "kubernetes":
		namespace := cfg.Plugins.Namespace
		if namespace == "" {
			namespace = cfg.Sessions.Namespace
		}
		selected := map[string]bool{}
		for _, agent := range mgr.Agents() {
			for _, name := range agent.Plugins {
				selected[name] = true
			}
		}
		reconciler, err := pluginsvc.New(pluginsvc.Options{
			Client:    kube.WithNamespace(namespace),
			Manifests: manifestsFn,
			Render: pluginsvc.RenderConfig{
				Namespace:         namespace,
				ToolsImage:        cfg.Runtime.Image,
				ImagePullPolicy:   cfg.Runtime.ImagePullPolicy,
				ImagePullSecrets:  cfg.Runtime.ImagePullSecrets,
				PlatformNamespace: cfg.Runtime.PlatformNamespace,
				PlatformLabels:    cfg.Runtime.PlatformLabels,
				DNSNamespace:      cfg.Runtime.DNSNamespace,
			},
			Interval: cfg.Plugins.ReconcileInterval.Std(),
			Prune:    config.Bool(cfg.Plugins.Prune, true),
			Selected: selected,
			Logger:   logger,
		})
		if err != nil {
			logger.Error("configure plugin reconciler", "error", err)
			os.Exit(2)
		}
		if err := reconciler.Reconcile(ctx); err != nil {
			logger.Error("reconcile plugin services", "error", err)
		}
		go reconciler.Run(ctx)
		return reconciler.ServiceURL, reconciler.RequiredErr
	case "local":
		local, err := pluginsvc.NewLocal(pluginsvc.LocalConfig{
			StateDir:  cfg.Runtime.StateDir,
			Manifests: manifestsFn,
			Logger:    logger,
		})
		if err != nil {
			logger.Error("configure local plugin services", "error", err)
			os.Exit(2)
		}
		if err := local.Start(ctx); err != nil {
			logger.Error("start local plugin services", "error", err)
			os.Exit(2)
		}
		go func() {
			<-ctx.Done()
			local.Close()
		}()
		return local.ServiceURL, nil
	default:
		return nil, nil
	}
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
