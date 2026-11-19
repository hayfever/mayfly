// Command loadtest drives concurrent sessions and webhook deliveries against a
// deployed Mayfly release and reports request latency, error rate, session
// throughput, and control-plane runtime growth. It runs against a kubeconfig
// through kubectl port-forward, or against any reachable base URL.
//
// The harness never calls a model. Point the release at llmstub, as
// docs/loadtest.md describes, and no API key or cloud resource is needed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// options is one resolved harness run.
type options struct {
	mode             string
	concurrency      int
	duration         time.Duration
	iterations       int
	baseURL          string
	kubeconfig       string
	kubeContext      string
	namespace        string
	service          string
	tokenEnv         string
	prompt           string
	ttl              string
	sessionTimeout   time.Duration
	pollInterval     time.Duration
	sampleInterval   time.Duration
	webhookTrigger   string
	webhookSecretEnv string
	metricsURL       string
	dryRun           bool
}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain parses flags, resolves the target, runs the load, and writes the
// report. It returns the process exit code.
func realMain(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "loadtest:", err)
		return 2
	}
	if opts.dryRun {
		printPlan(stdout, opts, os.Getenv)
		return 0
	}
	token := os.Getenv(opts.tokenEnv)
	if token == "" {
		fmt.Fprintf(stderr, "loadtest: %s is not set; it must hold an API token with the admin scope\n", opts.tokenEnv)
		return 2
	}
	var webhookSecret string
	if opts.mode != "sessions" {
		webhookSecret = os.Getenv(opts.webhookSecretEnv)
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	tgt, err := openTarget(ctx, opts)
	if err != nil {
		fmt.Fprintln(stderr, "loadtest:", err)
		return 1
	}
	defer tgt.stop()

	client := newAPIClient(tgt.base, token, webhookSecret)
	metricsURL := opts.metricsURL
	if metricsURL == "" {
		metricsURL = tgt.base + "/metrics"
	}
	cfg := loadConfig{
		mode:           opts.mode,
		concurrency:    opts.concurrency,
		duration:       opts.duration,
		iterations:     opts.iterations,
		prompt:         opts.prompt,
		ttl:            opts.ttl,
		sessionTimeout: opts.sessionTimeout,
		pollInterval:   opts.pollInterval,
		sampleInterval: opts.sampleInterval,
		webhookTrigger: opts.webhookTrigger,
		metricsURL:     metricsURL,
	}
	summary, runErr := runLoad(ctx, cfg, client, tgt.lost)
	printSummary(stdout, summary)
	if runErr != nil {
		fmt.Fprintln(stderr, "loadtest:", runErr)
		return 1
	}
	return 0
}

// parseOptions reads and validates the command line. Help goes to errOut.
func parseOptions(args []string, errOut io.Writer) (options, error) {
	opts := options{}
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&opts.mode, "mode", "sessions", "load to generate: sessions, webhooks, or both")
	fs.IntVar(&opts.concurrency, "n", 5, "concurrent workers; each holds at most one session")
	fs.DurationVar(&opts.duration, "duration", 2*time.Minute, "load and soak window")
	fs.IntVar(&opts.iterations, "iterations", 0, "operations per worker; 0 runs for the full duration")
	fs.StringVar(&opts.baseURL, "url", "", "base URL of the control plane; skips kubectl port-forward")
	fs.StringVar(&opts.kubeconfig, "kubeconfig", "", "kubeconfig file for kubectl; empty uses KUBECONFIG")
	fs.StringVar(&opts.kubeContext, "context", "", "kubectl context; empty uses the current context")
	fs.StringVar(&opts.namespace, "namespace", "mayfly", "namespace of the control plane Service")
	fs.StringVar(&opts.service, "service", "mayfly", "name of the control plane Service")
	fs.StringVar(&opts.tokenEnv, "token-env", "MAYFLY_API_TOKEN", "environment variable holding the API token")
	fs.StringVar(&opts.prompt, "prompt", "loadtest session", "prompt sent with every session")
	fs.StringVar(&opts.ttl, "ttl", "10m", "session TTL sent with every create")
	fs.DurationVar(&opts.sessionTimeout, "session-timeout", 5*time.Minute, "wait for one session to reach a terminal state")
	fs.DurationVar(&opts.pollInterval, "poll-interval", 500*time.Millisecond, "poll interval for session and job state")
	fs.DurationVar(&opts.sampleInterval, "sample-interval", 5*time.Second, "interval between control-plane runtime samples")
	fs.StringVar(&opts.webhookTrigger, "webhook-trigger", "generic", "generic trigger name under /hooks/{name}")
	fs.StringVar(&opts.webhookSecretEnv, "webhook-secret-env", "MAYFLY_WEBHOOK_SECRET", "environment variable holding the trigger signing secret")
	fs.StringVar(&opts.metricsURL, "metrics-url", "", "metrics endpoint; empty uses <url>/metrics")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "print the resolved plan and exit without contacting the target")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := opts.validate(); err != nil {
		return options{}, err
	}
	return opts, nil
}

// validate rejects a configuration that cannot produce a useful run.
func (o *options) validate() error {
	switch o.mode {
	case "sessions", "webhooks", "both":
	default:
		return fmt.Errorf("mode must be sessions, webhooks, or both, not %q", o.mode)
	}
	if o.concurrency < 1 {
		return errors.New("n must be at least 1")
	}
	if o.duration <= 0 {
		return errors.New("duration must be positive")
	}
	if o.iterations < 0 {
		return errors.New("iterations cannot be negative")
	}
	if o.pollInterval <= 0 {
		return errors.New("poll-interval must be positive")
	}
	if o.sampleInterval <= 0 {
		return errors.New("sample-interval must be positive")
	}
	if o.sessionTimeout < o.pollInterval {
		return errors.New("session-timeout must be at least poll-interval")
	}
	if _, err := time.ParseDuration(o.ttl); err != nil {
		return fmt.Errorf("ttl must be a duration such as 10m: %w", err)
	}
	if o.mode != "sessions" && o.webhookTrigger == "" {
		return errors.New("webhook-trigger is required for webhooks and both")
	}
	if o.baseURL != "" {
		u, err := url.Parse(o.baseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("url must be an absolute http or https URL, not %q", o.baseURL)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("url must use http or https, not %q", u.Scheme)
		}
		return nil
	}
	if o.namespace == "" || o.service == "" {
		return errors.New("namespace and service are required without url")
	}
	return nil
}

// printPlan describes what a run would do. It never contacts the cluster and
// never prints a secret value.
func printPlan(w io.Writer, opts options, getenv func(string) string) {
	where := fmt.Sprintf("context %q, namespace %q, service %q, kubectl port-forward",
		orCurrent(opts.kubeContext), opts.namespace, opts.service)
	if opts.baseURL != "" {
		where = opts.baseURL
	}
	fmt.Fprintf(w, "loadtest plan\n")
	fmt.Fprintf(w, "  mode:        %s\n", opts.mode)
	fmt.Fprintf(w, "  workers:     %d\n", opts.concurrency)
	fmt.Fprintf(w, "  window:      %s", opts.duration)
	if opts.iterations > 0 {
		fmt.Fprintf(w, " or %d operations per worker", opts.iterations)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  target:      %s\n", where)
	fmt.Fprintf(w, "  token:       $%s (%s)\n", opts.tokenEnv, setState(getenv(opts.tokenEnv)))
	if opts.mode != "sessions" {
		fmt.Fprintf(w, "  trigger:     %s, $%s (%s)\n", opts.webhookTrigger, opts.webhookSecretEnv, setState(getenv(opts.webhookSecretEnv)))
	}
	fmt.Fprintf(w, "  metrics:     %s\n", orDefault(opts.metricsURL, "target /metrics"))
	fmt.Fprintf(w, "  prompt:      %q, ttl %s\n", opts.prompt, opts.ttl)
}

func orCurrent(s string) string {
	if s == "" {
		return "current"
	}
	return s
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func setState(value string) string {
	if value == "" {
		return "not set"
	}
	return "set"
}
