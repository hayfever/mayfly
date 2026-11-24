// Package config loads and validates the control plane configuration.
package config

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/hayfever/mayfly/internal/content"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// Duration accepts Go duration strings in YAML ("30m", "1h").
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string such as 30m: %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the standard library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// CurrentVersion is the config file format this build reads. A file without a
// version field is read as version 1. Any other value is an error, so a
// downgrade fails at startup instead of silently ignoring keys it does not
// understand.
const CurrentVersion = 1

// Config is the root configuration document.
type Config struct {
	// Version is the config file format version. Load writes CurrentVersion
	// when the file omits the field.
	Version   int             `yaml:"version"`
	Server    ServerConfig    `yaml:"server"`
	Sessions  SessionsConfig  `yaml:"sessions"`
	Runtime   RuntimeConfig   `yaml:"runtime"`
	LLM       LLMConfig       `yaml:"llm"`
	Artifacts ArtifactsConfig `yaml:"artifacts"`
	Skills    SkillsConfig    `yaml:"skills"`
	MCP       MCPConfig       `yaml:"mcp"`
	Agents    AgentsConfig    `yaml:"agents"`
	Sources   SourcesConfig   `yaml:"sources"`
	Queue     QueueConfig     `yaml:"queue"`
	Journal   JournalConfig   `yaml:"journal"`
	Plugins   PluginsConfig   `yaml:"plugins"`
	Triggers  []TriggerConfig `yaml:"triggers"`

	// AdminToken is the token from server.token_env, resolved at load time
	// and never serialized. APITokens exposes it as the token named "default"
	// with the admin scope when server.tokens is empty.
	AdminToken string `yaml:"-"`
}

// ServerConfig covers the public HTTP listener.
type ServerConfig struct {
	Addr        string `yaml:"addr"`
	PublicURL   string `yaml:"public_url"`
	InternalURL string `yaml:"internal_url"`
	// TokenEnv names the environment variable that holds the single admin
	// token. It is used only when Tokens is empty.
	TokenEnv string `yaml:"token_env"`
	// Tokens declares named bearer tokens with explicit scopes. A non-empty
	// list replaces TokenEnv.
	Tokens []APITokenConfig `yaml:"tokens"`
	// TrustForwardedFor makes the auth-failure limiter key on the first
	// X-Forwarded-For hop. Set it only when a trusted proxy overwrites that
	// header; otherwise a client chooses its own limiter key.
	TrustForwardedFor bool     `yaml:"trust_forwarded_for"`
	AttachTicketTTL   Duration `yaml:"attach_ticket_ttl"`
	MaxBodyBytes      int64    `yaml:"max_body_bytes"`
	// WebhookMaxBodyBytes bounds a webhook body when its trigger sets no
	// max_body_bytes. Webhook bodies are smaller than admin JSON bodies.
	WebhookMaxBodyBytes int64 `yaml:"webhook_max_body_bytes"`
	// BodyReadTimeout bounds how long one request body may take to arrive.
	// Handlers set it per request instead of Server.ReadTimeout, so WebSocket
	// attach and streamed responses keep working. Zero or less uses the
	// default.
	BodyReadTimeout Duration `yaml:"body_read_timeout"`
	// ShutdownBudget bounds the whole stop sequence: HTTP drain, queue worker
	// drain, and session manager shutdown. It must fit inside the pod's
	// terminationGracePeriodSeconds with margin.
	ShutdownBudget Duration      `yaml:"shutdown_budget"`
	Metrics        MetricsConfig `yaml:"metrics"`
}

// MetricsConfig covers the Prometheus endpoint.
type MetricsConfig struct {
	// Enabled serves the metrics endpoint. A nil value means enabled.
	Enabled *bool `yaml:"enabled"`
	// Address serves metrics on a listener of its own. Empty serves /metrics
	// on the main HTTP listener.
	Address string `yaml:"address"`
	// RequireAuth demands a token with the admin scope. A nil value means
	// required on the main listener and no auth on a separate address.
	RequireAuth *bool `yaml:"require_auth"`
}

// MetricsEnabled reports whether the metrics endpoint is served.
func (c ServerConfig) MetricsEnabled() bool {
	return Bool(c.Metrics.Enabled, true)
}

// MetricsOnMainPort reports whether the main HTTP listener serves /metrics.
// A non-empty metrics address serves the endpoint on its own listener instead.
func (c ServerConfig) MetricsOnMainPort() bool {
	return c.MetricsEnabled() && c.Metrics.Address == ""
}

// MetricsRequireAuth reports whether the metrics endpoint needs a token with
// the admin scope. An unset value means required on the main listener, which
// is the public API, and not required on a separate address.
func (c ServerConfig) MetricsRequireAuth() bool {
	return Bool(c.Metrics.RequireAuth, c.MetricsOnMainPort())
}

// DefaultShutdownBudget leaves five seconds of the thirty-second default pod
// grace period for the runtime to kill the process.
const DefaultShutdownBudget = 25 * time.Second

// DefaultMaxEventBytes bounds one runtime event in memory. The runtime posts
// one event per request, so it also bounds what one delivery can retain.
const DefaultMaxEventBytes = 64 << 10

// DefaultEventBufferBytes is the retained thread byte budget per session.
// It applies in addition to sessions.event_buffer, the event count cap. The
// default is small because sessions.max_concurrent multiplies it; see the
// memory sizing note in docs/configuration.md.
const DefaultEventBufferBytes = 8 << 20

// DefaultWebhookMaxBodyBytes bounds a webhook body when neither the trigger
// nor the server sets a limit.
const DefaultWebhookMaxBodyBytes = 1 << 20

// DefaultBodyReadTimeout bounds how long a handler waits for a request body.
// A server-wide ReadTimeout cannot be used: it would also bound WebSocket
// attach and streamed responses.
const DefaultBodyReadTimeout = 30 * time.Second

// MaxWebhookBodyBytes is the largest webhook body limit a configuration may
// set. A webhook body is a signed event, not a session bundle.
const MaxWebhookBodyBytes = 16 << 20

// WebhookBodyLimit returns the body limit for one trigger. The trigger-level
// max_body_bytes overrides server.webhook_max_body_bytes.
func (c *Config) WebhookBodyLimit(t TriggerConfig) int64 {
	if t.MaxBodyBytes > 0 {
		return t.MaxBodyBytes
	}
	if c.Server.WebhookMaxBodyBytes > 0 {
		return c.Server.WebhookMaxBodyBytes
	}
	return DefaultWebhookMaxBodyBytes
}

// Shutdown budget shares, in percent. The stages run in order, so each one
// gets its own window inside the budget.
const (
	shutdownHTTPShare    = 20
	shutdownWorkerShare  = 40
	shutdownManagerShare = 40
)

// ShutdownShares returns how long each stop stage may take. The session
// manager gets the remainder, so the three shares always add up to the budget.
func (c ServerConfig) ShutdownShares() (httpShare, workerShare, managerShare time.Duration) {
	budget := c.ShutdownBudget.Std()
	if budget <= 0 {
		budget = DefaultShutdownBudget
	}
	httpShare = budget * shutdownHTTPShare / 100
	workerShare = budget * shutdownWorkerShare / 100
	managerShare = budget - httpShare - workerShare
	return httpShare, workerShare, managerShare
}

// SessionsConfig controls session defaults and runtime limits.
type SessionsConfig struct {
	Namespace        string   `yaml:"namespace"`
	DefaultTTL       Duration `yaml:"default_ttl"`
	MaxTTL           Duration `yaml:"max_ttl"`
	IdleTimeout      Duration `yaml:"idle_timeout"`
	CloseGrace       Duration `yaml:"close_grace"`
	EventBuffer      int      `yaml:"event_buffer"`
	EventBufferBytes int      `yaml:"event_buffer_bytes"`
	MaxEventBytes    int      `yaml:"max_event_bytes"`
	// InboxBufferBytes bounds the retained human messages per session, in
	// addition to the event_buffer count cap. Zero or less follows
	// event_buffer_bytes.
	InboxBufferBytes   int      `yaml:"inbox_buffer_bytes"`
	MaxSteps           int      `yaml:"max_steps"`
	MaxHistoryMessages int      `yaml:"max_history_messages"`
	MaxConcurrent      int      `yaml:"max_concurrent"`
	Retention          Duration `yaml:"retention"`
	ConnectTimeout     Duration `yaml:"connect_timeout"`
	// MaxRestarts bounds replacement containers for a session whose pod
	// fails while the session is still active. Zero disables replacement.
	MaxRestarts int `yaml:"max_restarts"`
	// PersistentWorkspace mounts one per-session PersistentVolumeClaim at the
	// workspace and artifact paths so a replacement container keeps its files.
	// Nil means true.
	PersistentWorkspace *bool    `yaml:"persistent_workspace"`
	AllowShell          *bool    `yaml:"allow_shell"`
	ShellTimeout        Duration `yaml:"shell_timeout"`
	WorkspaceDir        string   `yaml:"workspace_dir"`
	ArtifactsDir        string   `yaml:"artifacts_dir"`
	// Records selects where session records and the artifact index are kept.
	Records RecordConfig `yaml:"records"`
}

// RecordConfig selects the store that keeps session records and the artifact
// index across restarts.
type RecordConfig struct {
	// Backend is memory or bbolt. An empty value follows journal.backend.
	Backend string `yaml:"backend"`
	// Path is the bbolt file. An empty value puts sessions.db beside the
	// journal database.
	Path string `yaml:"path"`
}

// JournalConfig selects where agent turn steps are recorded for replay.
type JournalConfig struct {
	Backend string            `yaml:"backend"` // memory or bbolt
	Bolt    BoltJournalConfig `yaml:"bbolt"`
	// MaxEntryBytes bounds one journaled step as the runtime posts it. A
	// model step can carry a whole write_file argument (up to 4 MiB of
	// content, JSON-escaped), so the default is 8 MiB.
	MaxEntryBytes int64 `yaml:"max_entry_bytes"`
	// MaxSessionBytes bounds everything one session journals during one
	// control-plane lifetime, so a runaway or hostile runtime cannot fill the
	// state volume the queue and schedule watermark share.
	MaxSessionBytes int64 `yaml:"max_session_bytes"`
}

// BoltJournalConfig points at the journal database file.
type BoltJournalConfig struct {
	Path string `yaml:"path"`
}

// PluginsConfig controls service plugins and the tool gateway.
type PluginsConfig struct {
	// Namespace holds service plugin objects. An empty value follows
	// sessions.namespace.
	Namespace string `yaml:"namespace"`
	// ReconcileInterval is how often service objects are reconciled.
	ReconcileInterval Duration `yaml:"reconcile_interval"`
	// Prune removes service objects whose manifest is gone. Nil means true.
	Prune *bool `yaml:"prune"`
	// AuditPath appends one JSONL record per tool call. An empty path logs
	// to the process log only.
	AuditPath string `yaml:"audit_path"`
	// MaxInflightCalls bounds dispatched tool calls across every session.
	MaxInflightCalls int `yaml:"max_inflight_calls"`
}

// RuntimeConfig selects and configures the session execution backend.
type RuntimeConfig struct {
	Backend           string            `yaml:"backend"` // kubernetes or local
	Image             string            `yaml:"image"`
	ImagePullPolicy   string            `yaml:"image_pull_policy"`
	ImagePullSecrets  []string          `yaml:"image_pull_secrets"`
	AgentBinary       string            `yaml:"agent_binary"`
	StateDir          string            `yaml:"state_dir"`
	ServiceAccount    string            `yaml:"service_account"`
	PlatformNamespace string            `yaml:"platform_namespace"`
	PlatformLabels    map[string]string `yaml:"platform_labels"`
	NetworkPolicy     *bool             `yaml:"network_policy"`
	DNSNamespace      string            `yaml:"dns_namespace"`
	Egress            []EgressRule      `yaml:"egress"`
	WorkspaceLimit    string            `yaml:"workspace_limit"`
	ArtifactLimit     string            `yaml:"artifact_volume_limit"`
	// PluginVolumeLimit bounds the copied files of all inline plugins in a
	// session pod.
	PluginVolumeLimit string `yaml:"plugin_volume_limit"`
	// StorageClass names the StorageClass of the per-session workspace claim.
	// Empty uses the cluster default; a cluster without one leaves the claim
	// pending until the session times out.
	StorageClass string            `yaml:"storage_class"`
	Resources    ResourceConfig    `yaml:"resources"`
	Env          map[string]string `yaml:"env"`
	PollInterval Duration          `yaml:"poll_interval"`
	PodLabels    map[string]string `yaml:"pod_labels"`
}

// EgressRule permits direct egress from a session pod to a CIDR. HTTP MCP
// servers and the LLM are proxied by the control plane, so this is only needed
// for stdio servers that call external networks.
type EgressRule struct {
	CIDR  string  `yaml:"cidr"`
	Ports []int32 `yaml:"ports"`
}

// ResourceConfig holds Kubernetes resource quantities.
type ResourceConfig struct {
	Requests map[string]string `yaml:"requests"`
	Limits   map[string]string `yaml:"limits"`
}

// LLMConfig configures the OpenAI-compatible upstream used through the
// credential broker. The API key stays in the control plane.
type LLMConfig struct {
	BaseURL      string            `yaml:"base_url"`
	APIKeyEnv    string            `yaml:"api_key_env"`
	Model        string            `yaml:"model"`
	AllowModels  []string          `yaml:"allow_models"`
	Timeout      Duration          `yaml:"timeout"`
	Headers      map[string]string `yaml:"headers"`
	APIKey       string            `yaml:"-"`
	MaxBodyBytes int64             `yaml:"max_body_bytes"`
	// ToolResultImages reports that the assigned provider accepts image
	// content parts in tool messages. nil means true. An explicit false
	// replaces each image with a labeled text placeholder.
	ToolResultImages *bool `yaml:"tool_result_images"`
}

// ArtifactsConfig configures durable artifact export.
type ArtifactsConfig struct {
	Store    string           `yaml:"store"` // local or s3
	Local    LocalStoreConfig `yaml:"local"`
	S3       S3StoreConfig    `yaml:"s3"`
	MaxBytes int64            `yaml:"max_bytes"`
	// IndexRetention bounds how long the control plane keeps a stored
	// bundle's download metadata after the session record is gone. Zero keeps
	// the metadata, because the bundle itself is the durable output.
	IndexRetention Duration `yaml:"index_retention"`
}

// LocalStoreConfig writes artifacts to a directory.
type LocalStoreConfig struct {
	Dir string `yaml:"dir"`
}

// S3StoreConfig writes artifacts to an S3-compatible object store.
// Credentials are resolved from the environment and never given to sessions.
type S3StoreConfig struct {
	Endpoint        string `yaml:"endpoint"`
	Region          string `yaml:"region"`
	Bucket          string `yaml:"bucket"`
	AccessKeyEnv    string `yaml:"access_key_env"`
	SecretKeyEnv    string `yaml:"secret_key_env"`
	SessionTokenEnv string `yaml:"session_token_env"`
	Prefix          string `yaml:"prefix"`
	PathStyle       bool   `yaml:"path_style"`
}

// SkillsConfig points at skill directories on the control plane host. Skill
// bodies are copied into each selected session.
type SkillsConfig struct {
	Dirs    []string      `yaml:"dirs"`
	Sources []SkillSource `yaml:"sources"`
	Default []string      `yaml:"default"`
	// CacheDir stores verified source archives by SHA-256 so a restart does
	// not need the archive host. An empty value derives
	// "<journal directory>/skill-cache" when the journal backend keeps a file.
	CacheDir string `yaml:"cache_dir"`
}

// SkillSource is a pinned tar.gz archive of Markdown skills, usually a code
// hosting archive URL. The control plane fetches and verifies it at startup.
type SkillSource struct {
	Name            string            `yaml:"name"`
	Repo            string            `yaml:"repo"`
	Ref             string            `yaml:"ref"`
	URL             string            `yaml:"url"`
	SHA256          string            `yaml:"sha256"`
	Subdir          string            `yaml:"subdir"`
	StripComponents *int              `yaml:"strip_components"`
	Headers         map[string]string `yaml:"headers"`
	Timeout         Duration          `yaml:"timeout"`

	ResolvedURL     string            `yaml:"-"`
	ResolvedHeaders map[string]string `yaml:"-"`
}

// AgentsConfig points at agent plugin manifests.
type AgentsConfig struct {
	Dirs          []string `yaml:"dirs"`
	AllowedImages []string `yaml:"allowed_images"`
}

// SourcesConfig controls scheduled source runs. Project bundles declare the
// connections and sources; this section only covers process-wide limits and
// where the schedule watermark is kept.
type SourcesConfig struct {
	// StatePath is the bbolt file that stores the last completed schedule
	// slot. An empty path keeps the watermark in memory.
	StatePath string `yaml:"state_path"`
	// MaxArtifactBytes bounds the payload one source run may write.
	MaxArtifactBytes int64 `yaml:"max_artifact_bytes"`
	// Concurrency bounds in-flight HTTP requests within one run.
	Concurrency int `yaml:"concurrency"`
	// Timeout bounds one source step: every request it sends, with retries.
	// A fanout step that reaches it keeps the pages it already fetched and
	// reports the rest as item errors. Each request is also bounded by its
	// connection's timeout.
	Timeout Duration `yaml:"timeout"`
	// JobTimeout bounds one whole source job: every fetch, the run artifact,
	// and the session it creates. The queue lease is renewed while the job
	// runs, so this may exceed queue.visibility_timeout.
	JobTimeout Duration `yaml:"job_timeout"`
}

// MCPConfig lists MCP servers that sessions can use and bounds the image
// content their tool results may carry.
type MCPConfig struct {
	Servers []MCPServerConfig `yaml:"servers"`
	// MaxImageBytes bounds one image in a tool result.
	MaxImageBytes int64 `yaml:"max_image_bytes"`
	// MaxResultImageBytes bounds the images in one tool result. A result that
	// exceeds it is recorded as an error, never truncated mid-image.
	MaxResultImageBytes int64 `yaml:"max_result_image_bytes"`
	// MaxSessionImageBytes bounds the images one session may deliver. It also
	// keeps a model request under the default broker body bound after base64
	// expansion.
	MaxSessionImageBytes int64 `yaml:"max_session_image_bytes"`
	// MaxRequestBytes bounds one tool gateway request body. A complete report
	// carries the image parts the runtime stored.
	MaxRequestBytes int64 `yaml:"max_request_bytes"`
}

// ContentLimits returns the image bounds for the gateway and the runtime.
func (c MCPConfig) ContentLimits() content.Limits {
	return content.Limits{
		MaxImageBytes:        c.MaxImageBytes,
		MaxResultImageBytes:  c.MaxResultImageBytes,
		MaxSessionImageBytes: c.MaxSessionImageBytes,
	}.WithDefaults()
}

// RequestBytes returns the tool gateway request body bound.
func (c MCPConfig) RequestBytes() int64 {
	if c.MaxRequestBytes > 0 {
		return c.MaxRequestBytes
	}
	return defaultMCPRequestBytes
}

const defaultMCPRequestBytes = 16 << 20

// MCPServerConfig describes one MCP server. HTTP servers are proxied by the
// control plane; header values may use "env:NAME" to inject a secret from the
// control plane environment.
type MCPServerConfig struct {
	Name        string            `yaml:"name"`
	Transport   string            `yaml:"transport"` // stdio or http
	Command     string            `yaml:"command"`
	Args        []string          `yaml:"args"`
	Env         map[string]string `yaml:"env"`
	URL         string            `yaml:"url"`
	Headers     map[string]string `yaml:"headers"`
	OptIn       bool              `yaml:"opt_in"`
	InitTimeout Duration          `yaml:"init_timeout"`
	// Scope is "shared" or "agent:<name>". A private server is available only
	// to its owner, and never included in defaults.
	Scope string `yaml:"scope"`
	// Tools is the allowlist of a server. An empty list offers every tool the
	// server reports.
	Tools []string `yaml:"tools"`
	// IdempotentTools names the tools that are safe to repeat after a
	// container restart. Every other tool counts as non-idempotent.
	IdempotentTools []string `yaml:"idempotent_tools"`

	ResolvedHeaders map[string]string `yaml:"-"`
}

// QueueConfig selects where accepted webhook work is stored and how it is
// retried. The memory backend is for development; bbolt and redis survive a
// control-plane restart.
type QueueConfig struct {
	Backend           string           `yaml:"backend"` // memory, bbolt, redis
	MaxJobs           int              `yaml:"max_jobs"`
	MaxAttempts       int              `yaml:"max_attempts"`
	RetryBase         Duration         `yaml:"retry_base"`
	VisibilityTimeout Duration         `yaml:"visibility_timeout"`
	DedupeTTL         Duration         `yaml:"dedupe_ttl"`
	TombstoneTTL      Duration         `yaml:"tombstone_ttl"`
	PollInterval      Duration         `yaml:"poll_interval"`
	Bolt              BoltQueueConfig  `yaml:"bbolt"`
	Redis             RedisQueueConfig `yaml:"redis"`
}

// BoltQueueConfig points at the bbolt database file.
type BoltQueueConfig struct {
	Path string `yaml:"path"`
}

// RedisQueueConfig configures a Redis-protocol store. Redis, Valkey, KeyDB,
// Dragonfly, ElastiCache, Memorystore, and Azure Managed Redis all speak it.
type RedisQueueConfig struct {
	URL         string `yaml:"url"`
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
	KeyPrefix   string `yaml:"key_prefix"`
	Cluster     bool   `yaml:"cluster"`
}

// TriggerConfig defines one webhook endpoint at /hooks/<name>.
type TriggerConfig struct {
	Name          string   `yaml:"name"`
	Type          string   `yaml:"type"` // generic, github, slack, datadog
	SecretEnv     string   `yaml:"secret_env"`
	AllowUnsigned bool     `yaml:"allow_unsigned"`
	Template      string   `yaml:"template"`
	TTL           Duration `yaml:"ttl"`
	Model         string   `yaml:"model"`
	Agent         string   `yaml:"agent"`
	Skills        []string `yaml:"skills"`
	MCPServers    []string `yaml:"mcp_servers"`
	// MaxBodyBytes overrides server.webhook_max_body_bytes for this trigger.
	// Zero uses the server default.
	MaxBodyBytes int64            `yaml:"max_body_bytes"`
	RateLimit    *RateLimitConfig `yaml:"rate_limit"`
	Secret       string           `yaml:"-"`
}

// RateLimitConfig bounds how many sessions one trigger can request. Requests
// refill over Interval up to Burst. Requests: 0 disables the limit.
type RateLimitConfig struct {
	Requests int      `yaml:"requests"`
	Interval Duration `yaml:"interval"`
	Burst    int      `yaml:"burst"`
}

// Default returns a configuration with safe defaults applied.
func Default() *Config {
	return &Config{
		Version: CurrentVersion,
		Server: ServerConfig{
			Addr:            ":8080",
			PublicURL:       "http://localhost:8080",
			TokenEnv:        "MAYFLY_API_TOKEN",
			AttachTicketTTL: Duration(60 * time.Second),
			MaxBodyBytes:    4 << 20,
			// A webhook body carries one signed event, so its default is
			// smaller than the admin JSON limit.
			WebhookMaxBodyBytes: DefaultWebhookMaxBodyBytes,
			BodyReadTimeout:     Duration(DefaultBodyReadTimeout),
			ShutdownBudget:      Duration(DefaultShutdownBudget),
		},
		Sessions: SessionsConfig{
			Namespace:           "mayfly",
			DefaultTTL:          Duration(time.Hour),
			MaxTTL:              Duration(4 * time.Hour),
			IdleTimeout:         Duration(20 * time.Minute),
			CloseGrace:          Duration(90 * time.Second),
			EventBuffer:         5000,
			EventBufferBytes:    DefaultEventBufferBytes,
			MaxEventBytes:       DefaultMaxEventBytes,
			InboxBufferBytes:    DefaultEventBufferBytes,
			MaxSteps:            24,
			MaxHistoryMessages:  120,
			MaxConcurrent:       50,
			Retention:           Duration(30 * time.Minute),
			ConnectTimeout:      Duration(2 * time.Minute),
			MaxRestarts:         2,
			PersistentWorkspace: new(true),
			AllowShell:          new(false),
			ShellTimeout:        Duration(120 * time.Second),
			WorkspaceDir:        "/workspace",
			ArtifactsDir:        "/var/lib/mayfly/artifacts",
		},
		Runtime: RuntimeConfig{
			Backend:           "kubernetes",
			ImagePullPolicy:   "IfNotPresent",
			StateDir:          ".mayfly",
			ServiceAccount:    "mayfly-agent",
			PlatformNamespace: "mayfly",
			PlatformLabels:    map[string]string{"app.kubernetes.io/name": "mayflyd"},
			NetworkPolicy:     new(true),
			DNSNamespace:      "kube-system",
			WorkspaceLimit:    "1Gi",
			ArtifactLimit:     "1Gi",
			PluginVolumeLimit: "512Mi",
			PollInterval:      Duration(2 * time.Second),
			Egress:            []EgressRule{},
			Env:               map[string]string{},
		},
		LLM: LLMConfig{
			Timeout:      Duration(180 * time.Second),
			MaxBodyBytes: 16 << 20,
		},
		Artifacts: ArtifactsConfig{
			Store:    "local",
			Local:    LocalStoreConfig{Dir: "./artifacts"},
			MaxBytes: 256 << 20,
		},
		Skills: SkillsConfig{},
		MCP: MCPConfig{
			MaxImageBytes:        content.DefaultMaxImageBytes,
			MaxResultImageBytes:  content.DefaultMaxResultImageBytes,
			MaxSessionImageBytes: content.DefaultMaxSessionImageBytes,
			MaxRequestBytes:      defaultMCPRequestBytes,
		},
		Sources: SourcesConfig{
			MaxArtifactBytes: 16 << 20,
			Concurrency:      4,
			Timeout:          Duration(15 * time.Minute),
			JobTimeout:       Duration(30 * time.Minute),
		},
		Queue: QueueConfig{
			Backend:           "memory",
			MaxJobs:           1000,
			MaxAttempts:       5,
			RetryBase:         Duration(5 * time.Second),
			VisibilityTimeout: Duration(5 * time.Minute),
			DedupeTTL:         Duration(72 * time.Hour),
			TombstoneTTL:      Duration(time.Hour),
			PollInterval:      Duration(2 * time.Second),
			Redis:             RedisQueueConfig{KeyPrefix: "mayfly"},
		},
		Journal: JournalConfig{
			Backend:         "bbolt",
			Bolt:            BoltJournalConfig{Path: ".mayfly/journal.db"},
			MaxEntryBytes:   8 << 20,
			MaxSessionBytes: 64 << 20,
		},
		Plugins: PluginsConfig{
			ReconcileInterval: Duration(60 * time.Second),
			Prune:             new(true),
			MaxInflightCalls:  256,
		},
	}
}

// Load reads a YAML configuration file, applies defaults, resolves secrets
// from the environment, and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := Default()
	// Zero the version so a file that omits the field is distinguishable from
	// one that sets it to the current version.
	cfg.Version = 0
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	// A decoder reads one document at a time; a trailing document would be
	// ignored silently. One config file means one document.
	var extra yaml.Node
	if err := decoder.Decode(&extra); err == nil {
		return nil, fmt.Errorf("parse config %s: multiple YAML documents are not supported", path)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	switch cfg.Version {
	case 0:
		cfg.Version = CurrentVersion
		slog.Warn("config has no version field; assuming the current version",
			"path", path, "version", CurrentVersion)
	case CurrentVersion:
	default:
		return nil, fmt.Errorf("config %s: version %d is not supported by this build, which reads version %d", path, cfg.Version, CurrentVersion)
	}
	cfg.applyDefaults()
	if err := cfg.resolveSecrets(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.InternalURL == "" {
		c.Server.InternalURL = c.Server.PublicURL
	}
	if c.Runtime.PlatformNamespace == "" {
		c.Runtime.PlatformNamespace = c.Sessions.Namespace
	}
	if c.Runtime.DNSNamespace == "" {
		c.Runtime.DNSNamespace = "kube-system"
	}
	if c.Runtime.PlatformLabels == nil {
		c.Runtime.PlatformLabels = map[string]string{"app.kubernetes.io/name": "mayflyd"}
	}
	if c.Runtime.PollInterval.Std() <= 0 {
		c.Runtime.PollInterval = Duration(2 * time.Second)
	}
	if c.Server.AttachTicketTTL.Std() <= 0 {
		c.Server.AttachTicketTTL = Duration(60 * time.Second)
	}
	if c.Server.ShutdownBudget.Std() <= 0 {
		c.Server.ShutdownBudget = Duration(DefaultShutdownBudget)
	}
	if c.Server.BodyReadTimeout.Std() <= 0 {
		c.Server.BodyReadTimeout = Duration(DefaultBodyReadTimeout)
	}
	if c.LLM.MaxBodyBytes <= 0 {
		c.LLM.MaxBodyBytes = 16 << 20
	}
	if c.MCP.MaxImageBytes <= 0 {
		c.MCP.MaxImageBytes = content.DefaultMaxImageBytes
	}
	if c.MCP.MaxResultImageBytes <= 0 {
		c.MCP.MaxResultImageBytes = content.DefaultMaxResultImageBytes
	}
	if c.MCP.MaxSessionImageBytes <= 0 {
		c.MCP.MaxSessionImageBytes = content.DefaultMaxSessionImageBytes
	}
	if c.MCP.MaxRequestBytes <= 0 {
		c.MCP.MaxRequestBytes = defaultMCPRequestBytes
	}
	if c.Artifacts.MaxBytes <= 0 {
		c.Artifacts.MaxBytes = 256 << 20
	}
	if c.Sessions.MaxSteps <= 0 {
		c.Sessions.MaxSteps = 24
	}
	if c.Sessions.EventBuffer <= 0 {
		c.Sessions.EventBuffer = 5000
	}
	if c.Sessions.EventBufferBytes <= 0 {
		c.Sessions.EventBufferBytes = DefaultEventBufferBytes
	}
	if c.Sessions.InboxBufferBytes <= 0 {
		c.Sessions.InboxBufferBytes = c.Sessions.EventBufferBytes
	}
	if c.Sessions.MaxEventBytes <= 0 {
		c.Sessions.MaxEventBytes = DefaultMaxEventBytes
	}
	if c.Queue.MaxJobs <= 0 {
		c.Queue.MaxJobs = 1000
	}
	if c.Queue.MaxAttempts <= 0 {
		c.Queue.MaxAttempts = 5
	}
	if c.Queue.RetryBase.Std() <= 0 {
		c.Queue.RetryBase = Duration(5 * time.Second)
	}
	if c.Queue.VisibilityTimeout.Std() <= 0 {
		c.Queue.VisibilityTimeout = Duration(5 * time.Minute)
	}
	if c.Queue.DedupeTTL.Std() <= 0 {
		c.Queue.DedupeTTL = Duration(72 * time.Hour)
	}
	if c.Queue.TombstoneTTL.Std() < 0 {
		c.Queue.TombstoneTTL = 0
	}
	if c.Queue.PollInterval.Std() <= 0 {
		c.Queue.PollInterval = Duration(2 * time.Second)
	}
	if c.Queue.Redis.KeyPrefix == "" {
		c.Queue.Redis.KeyPrefix = "mayfly"
	}
	if c.Sessions.WorkspaceDir == "" {
		c.Sessions.WorkspaceDir = "/workspace"
	}
	if c.Sessions.ArtifactsDir == "" {
		c.Sessions.ArtifactsDir = "/var/lib/mayfly/artifacts"
	}
	if c.Sessions.PersistentWorkspace == nil {
		c.Sessions.PersistentWorkspace = new(true)
	}
	if c.Journal.Backend == "" {
		c.Journal.Backend = "bbolt"
	}
	if c.Journal.Backend == "bbolt" && c.Journal.Bolt.Path == "" {
		c.Journal.Bolt.Path = ".mayfly/journal.db"
	}
	if c.Sessions.Records.Backend == "" {
		c.Sessions.Records.Backend = c.Journal.Backend
	}
	if c.Sessions.Records.Backend == "bbolt" && c.Sessions.Records.Path == "" {
		c.Sessions.Records.Path = filepath.Join(filepath.Dir(c.Journal.Bolt.Path), "sessions.db")
	}
	if c.Skills.CacheDir == "" && c.Journal.Backend == "bbolt" && c.Journal.Bolt.Path != "" {
		c.Skills.CacheDir = filepath.Join(filepath.Dir(c.Journal.Bolt.Path), "skill-cache")
	}
	if c.Runtime.WorkspaceLimit == "" {
		c.Runtime.WorkspaceLimit = "1Gi"
	}
	if c.Runtime.ArtifactLimit == "" {
		c.Runtime.ArtifactLimit = "1Gi"
	}
	if c.Runtime.PluginVolumeLimit == "" {
		c.Runtime.PluginVolumeLimit = "512Mi"
	}
	if c.Plugins.Namespace == "" {
		c.Plugins.Namespace = c.Sessions.Namespace
	}
	if c.Plugins.ReconcileInterval.Std() <= 0 {
		c.Plugins.ReconcileInterval = Duration(60 * time.Second)
	}
	if c.Plugins.Prune == nil {
		c.Plugins.Prune = new(true)
	}
	if c.Plugins.MaxInflightCalls <= 0 {
		c.Plugins.MaxInflightCalls = 256
	}
	if c.Runtime.PollInterval.Std() <= 0 {
		c.Runtime.PollInterval = Duration(2 * time.Second)
	}
	for i := range c.Triggers {
		rl := c.Triggers[i].RateLimit
		if rl == nil {
			c.Triggers[i].RateLimit = &RateLimitConfig{
				Requests: defaultTriggerRateRequests,
				Interval: Duration(defaultTriggerRateInterval),
				Burst:    defaultTriggerRateBurst,
			}
			continue
		}
		if rl.Interval.Std() <= 0 {
			rl.Interval = Duration(defaultTriggerRateInterval)
		}
		if rl.Burst <= 0 {
			rl.Burst = rl.Requests
		}
		if rl.Burst <= 0 {
			rl.Burst = 1
		}
	}
}

// Default webhook rate limit: 30 sessions per minute with a burst of 10.
const (
	defaultTriggerRateRequests = 30
	defaultTriggerRateInterval = time.Minute
	defaultTriggerRateBurst    = 10
)

func (c *Config) resolveSecrets() error {
	if err := c.Server.validateTokens(); err != nil {
		return err
	}
	if len(c.Server.Tokens) == 0 {
		c.AdminToken = os.Getenv(c.Server.TokenEnv)
		if err := validateTokenSecret("server.token_env ("+c.Server.TokenEnv+")", c.AdminToken); err != nil {
			return err
		}
	} else {
		for i := range c.Server.Tokens {
			token := &c.Server.Tokens[i]
			token.Secret = os.Getenv(token.SecretEnv)
			if token.Secret == "" {
				return fmt.Errorf("server.tokens[%d] (%s): environment variable %s is not set", i, token.Name, token.SecretEnv)
			}
			if err := validateTokenSecret(fmt.Sprintf("server.tokens[%d] (%s) secret_env %s", i, token.Name, token.SecretEnv), token.Secret); err != nil {
				return err
			}
		}
	}
	if c.LLM.APIKeyEnv != "" {
		c.LLM.APIKey = os.Getenv(c.LLM.APIKeyEnv)
	}
	for i := range c.Triggers {
		t := &c.Triggers[i]
		if t.SecretEnv != "" {
			t.Secret = os.Getenv(t.SecretEnv)
		}
	}
	for i := range c.Skills.Sources {
		src := &c.Skills.Sources[i]
		if src.Repo != "" {
			built, err := repoArchiveURL(src.Repo, src.Ref)
			if err != nil {
				return fmt.Errorf("skill source %q: %w", src.Name, err)
			}
			src.ResolvedURL = built
		}
		src.ResolvedHeaders = map[string]string{}
		for k, v := range src.Headers {
			if strings.HasPrefix(v, "env:") {
				name := strings.TrimPrefix(v, "env:")
				val := os.Getenv(name)
				if val == "" {
					return fmt.Errorf("skill source %q: header %s needs environment variable %s", src.Name, k, name)
				}
				v = val
			}
			src.ResolvedHeaders[k] = v
		}
		if src.Timeout.Std() <= 0 {
			src.Timeout = Duration(30 * time.Second)
		}
	}
	for i := range c.MCP.Servers {
		s := &c.MCP.Servers[i]
		if s.Transport != "http" {
			for k, v := range s.Env {
				if strings.HasPrefix(v, "env:") {
					return fmt.Errorf("mcp server %q: stdio env %s references a platform secret; use an http MCP server so the platform can proxy credentials", s.Name, k)
				}
			}
			continue
		}
		s.ResolvedHeaders = map[string]string{}
		for k, v := range s.Headers {
			if strings.HasPrefix(v, "env:") {
				name := strings.TrimPrefix(v, "env:")
				val := os.Getenv(name)
				if val == "" {
					return fmt.Errorf("mcp server %q: header %s needs environment variable %s", s.Name, k, name)
				}
				v = val
			}
			s.ResolvedHeaders[k] = v
		}
	}
	return nil
}

// Validate checks the configuration for inconsistencies.
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr is required")
	}
	if c.Server.PublicURL == "" {
		return fmt.Errorf("server.public_url is required")
	}
	if c.Server.InternalURL == "" {
		return fmt.Errorf("server.internal_url is required")
	}
	if c.Server.ShutdownBudget.Std() < 3*time.Second {
		return fmt.Errorf("server.shutdown_budget must be at least 3s")
	}
	if c.Server.WebhookMaxBodyBytes <= 0 {
		return fmt.Errorf("server.webhook_max_body_bytes must be positive")
	}
	if c.Server.WebhookMaxBodyBytes > MaxWebhookBodyBytes {
		return fmt.Errorf("server.webhook_max_body_bytes must not exceed %d bytes", MaxWebhookBodyBytes)
	}
	if err := c.Server.validateTokens(); err != nil {
		return err
	}
	if c.Server.MetricsEnabled() && c.Server.Metrics.Address != "" && c.Server.Metrics.Address == c.Server.Addr {
		return fmt.Errorf("server.metrics.address must differ from server.addr")
	}
	if c.Sessions.DefaultTTL.Std() <= 0 {
		return fmt.Errorf("sessions.default_ttl must be positive")
	}
	if c.Sessions.MaxTTL.Std() < c.Sessions.DefaultTTL.Std() {
		return fmt.Errorf("sessions.max_ttl must be >= sessions.default_ttl")
	}
	if c.Sessions.CloseGrace.Std() <= 0 {
		return fmt.Errorf("sessions.close_grace must be positive")
	}
	if c.Sessions.MaxConcurrent < 0 {
		return fmt.Errorf("sessions.max_concurrent must not be negative")
	}
	if c.Sessions.Retention.Std() < 0 {
		return fmt.Errorf("sessions.retention must not be negative")
	}
	if c.Sessions.ConnectTimeout.Std() < 0 {
		return fmt.Errorf("sessions.connect_timeout must not be negative")
	}
	if c.Sessions.MaxRestarts < 0 {
		return fmt.Errorf("sessions.max_restarts must not be negative")
	}
	switch c.Sessions.Records.Backend {
	case "memory":
	case "bbolt":
		if c.Sessions.Records.Path == "" {
			return fmt.Errorf("sessions.records.path is required for the bbolt backend")
		}
	default:
		return fmt.Errorf("sessions.records.backend must be memory or bbolt")
	}
	if c.Artifacts.IndexRetention.Std() < 0 {
		return fmt.Errorf("artifacts.index_retention must not be negative")
	}
	if c.Sources.MaxArtifactBytes <= 0 {
		return fmt.Errorf("sources.max_artifact_bytes must be positive")
	}
	if c.Sources.Concurrency <= 0 {
		return fmt.Errorf("sources.concurrency must be positive")
	}
	if c.Sources.Timeout.Std() <= 0 {
		return fmt.Errorf("sources.timeout must be positive")
	}
	if c.Sources.JobTimeout.Std() <= 0 {
		return fmt.Errorf("sources.job_timeout must be positive")
	}
	if c.Sources.Timeout.Std() > c.Sources.JobTimeout.Std() {
		return fmt.Errorf("sources.timeout must not exceed sources.job_timeout")
	}
	for name := range c.Runtime.Env {
		if sessioncfg.IsReservedEnv(name) {
			return fmt.Errorf("runtime.env must not set %s; the control plane injects it into every session", name)
		}
	}
	switch c.Runtime.Backend {
	case "kubernetes":
		if c.Runtime.Image == "" {
			return fmt.Errorf("runtime.image is required for the kubernetes backend")
		}
		if c.Sessions.Namespace == "" {
			return fmt.Errorf("sessions.namespace is required for the kubernetes backend")
		}
	case "local":
		if c.Runtime.AgentBinary == "" {
			return fmt.Errorf("runtime.agent_binary is required for the local backend")
		}
	default:
		return fmt.Errorf("runtime.backend must be kubernetes or local, got %q", c.Runtime.Backend)
	}
	if _, err := resource.ParseQuantity(c.Runtime.PluginVolumeLimit); err != nil {
		return fmt.Errorf("runtime.plugin_volume_limit: %w", err)
	}
	if c.LLM.BaseURL == "" {
		return fmt.Errorf("llm.base_url is required")
	}
	if c.LLM.Model == "" {
		return fmt.Errorf("llm.model is required")
	}
	if len(c.LLM.AllowModels) > 0 {
		allowed := false
		for _, m := range c.LLM.AllowModels {
			if m == c.LLM.Model {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("llm.model %q is not in llm.allow_models", c.LLM.Model)
		}
	}
	switch c.Queue.Backend {
	case "", "memory":
	case "bbolt":
		if c.Queue.Bolt.Path == "" {
			return fmt.Errorf("queue.bbolt.path is required")
		}
	case "redis":
		u, err := url.Parse(c.Queue.Redis.URL)
		if err != nil || u.Host == "" || (u.Scheme != "redis" && u.Scheme != "rediss") {
			return fmt.Errorf("queue.redis.url must be redis:// or rediss:// with a host")
		}
	default:
		return fmt.Errorf("queue.backend must be memory, bbolt, or redis, got %q", c.Queue.Backend)
	}
	if c.Queue.MaxJobs < 0 || c.Queue.MaxAttempts < 0 {
		return fmt.Errorf("queue.max_jobs and queue.max_attempts must not be negative")
	}
	if c.Queue.TombstoneTTL.Std() < 0 {
		return fmt.Errorf("queue.tombstone_ttl must not be negative")
	}
	switch c.Journal.Backend {
	case "memory":
	case "bbolt":
		if c.Journal.Bolt.Path == "" {
			return fmt.Errorf("journal.bbolt.path is required")
		}
	default:
		return fmt.Errorf("journal.backend must be memory or bbolt, got %q", c.Journal.Backend)
	}
	if c.Journal.MaxEntryBytes <= 0 {
		return fmt.Errorf("journal.max_entry_bytes must be positive")
	}
	if c.Journal.MaxSessionBytes < c.Journal.MaxEntryBytes {
		return fmt.Errorf("journal.max_session_bytes must be at least journal.max_entry_bytes")
	}
	if c.MCP.MaxResultImageBytes < c.MCP.MaxImageBytes {
		return fmt.Errorf("mcp.max_result_image_bytes must be at least mcp.max_image_bytes")
	}
	if c.MCP.MaxSessionImageBytes < c.MCP.MaxImageBytes {
		return fmt.Errorf("mcp.max_session_image_bytes must be at least mcp.max_image_bytes")
	}
	// A tool result reaches the journal as one step, and the complete report
	// reaches the gateway as one JSON body. Base64 expands 4 bytes to 3, so
	// the image bound must leave room for text and JSON in both.
	encodedResult := (c.MCP.MaxResultImageBytes*4 + 2) / 3
	if c.MCP.MaxRequestBytes < encodedResult+(64<<10) {
		return fmt.Errorf("mcp.max_request_bytes must fit one result of mcp.max_result_image_bytes after base64 expansion")
	}
	if c.Journal.MaxEntryBytes < encodedResult+(512<<10) {
		return fmt.Errorf("journal.max_entry_bytes must fit one image result of mcp.max_result_image_bytes after base64 expansion")
	}
	if c.Plugins.ReconcileInterval.Std() < 10*time.Second {
		return fmt.Errorf("plugins.reconcile_interval must be at least 10s")
	}
	if c.Plugins.MaxInflightCalls < 1 {
		return fmt.Errorf("plugins.max_inflight_calls must be positive")
	}
	// A session start can take up to 90s (runner start timeout); a shorter
	// lease would expire while the first worker is still creating the session.
	if c.Queue.VisibilityTimeout.Std() < 2*time.Minute {
		return fmt.Errorf("queue.visibility_timeout must be at least 2m")
	}
	// Redis stores dedupe keys with whole-second expiry.
	if c.Queue.DedupeTTL.Std() < time.Second {
		return fmt.Errorf("queue.dedupe_ttl must be at least 1s")
	}
	switch c.Artifacts.Store {
	case "local":
		if c.Artifacts.Local.Dir == "" {
			return fmt.Errorf("artifacts.local.dir is required")
		}
	case "s3":
		if c.Artifacts.S3.Bucket == "" || c.Artifacts.S3.Region == "" {
			return fmt.Errorf("artifacts.s3.bucket and artifacts.s3.region are required")
		}
		if (c.Artifacts.S3.AccessKeyEnv == "") != (c.Artifacts.S3.SecretKeyEnv == "") {
			return fmt.Errorf("artifacts.s3.access_key_env and artifacts.s3.secret_key_env must be set together; leave both empty to use the default AWS credential chain")
		}
	default:
		return fmt.Errorf("artifacts.store must be local or s3, got %q", c.Artifacts.Store)
	}

	seenMCP := map[string]bool{}
	for _, s := range c.MCP.Servers {
		if s.Name == "" {
			return fmt.Errorf("mcp server name is required")
		}
		if seenMCP[s.Name] {
			return fmt.Errorf("duplicate mcp server name %q", s.Name)
		}
		seenMCP[s.Name] = true
		switch s.Transport {
		case "stdio":
			if s.Command == "" {
				return fmt.Errorf("mcp server %q: command is required", s.Name)
			}
		case "http":
			if s.URL == "" {
				return fmt.Errorf("mcp server %q: url is required", s.Name)
			}
		default:
			return fmt.Errorf("mcp server %q: transport must be stdio or http", s.Name)
		}
		if err := validateMCPToolLists(s); err != nil {
			return err
		}
	}

	if c.Skills.CacheDir != "" {
		if filepath.Clean(c.Skills.CacheDir) != c.Skills.CacheDir {
			return fmt.Errorf("skills.cache_dir %q must be a clean path", c.Skills.CacheDir)
		}
		if strings.HasSuffix(strings.ToLower(c.Skills.CacheDir), ".db") {
			return fmt.Errorf("skills.cache_dir %q must be a directory, not a database file", c.Skills.CacheDir)
		}
	}

	for _, src := range c.Skills.Sources {
		if src.Repo != "" && src.URL != "" {
			return fmt.Errorf("skill source %q: set repo or url, not both", src.Name)
		}
		if src.Repo != "" {
			if strings.TrimSpace(src.Ref) == "" {
				return fmt.Errorf("skill source %q: ref is required with repo", src.Name)
			}
			if src.ResolvedURL == "" {
				built, err := repoArchiveURL(src.Repo, src.Ref)
				if err != nil {
					return fmt.Errorf("skill source %q: %w", src.Name, err)
				}
				src.ResolvedURL = built
			}
		}
		if src.Repo == "" && strings.TrimSpace(src.URL) == "" {
			return fmt.Errorf("skill source %q: url or repo is required", src.Name)
		}
		archiveURL := src.URL
		if src.Repo != "" {
			archiveURL = src.ResolvedURL
		}
		parsed, err := url.Parse(archiveURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("skill source %q: url must be http or https", src.Name)
		}
		if len(src.SHA256) != 64 {
			return fmt.Errorf("skill source %q: sha256 must be a 64 character hex digest", src.Name)
		}
		if _, err := hex.DecodeString(src.SHA256); err != nil {
			return fmt.Errorf("skill source %q: sha256 is not valid hex", src.Name)
		}
		if src.Subdir != "" {
			clean := path.Clean(src.Subdir)
			if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
				return fmt.Errorf("skill source %q: subdir must stay inside the archive", src.Name)
			}
		}
		if src.StripComponents != nil && *src.StripComponents < 0 {
			return fmt.Errorf("skill source %q: strip_components must not be negative", src.Name)
		}
	}

	seenTrigger := map[string]bool{}
	for _, t := range c.Triggers {
		if t.Name == "" {
			return fmt.Errorf("trigger name is required")
		}
		if seenTrigger[t.Name] {
			return fmt.Errorf("duplicate trigger name %q", t.Name)
		}
		seenTrigger[t.Name] = true
		switch t.Type {
		case "generic", "github", "slack", "datadog":
		default:
			return fmt.Errorf("trigger %q: unsupported type %q", t.Name, t.Type)
		}
		if t.Secret == "" && !t.AllowUnsigned {
			return fmt.Errorf("trigger %q: secret_env %s is not set and allow_unsigned is false", t.Name, t.SecretEnv)
		}
		if t.MaxBodyBytes < 0 || t.MaxBodyBytes > MaxWebhookBodyBytes {
			return fmt.Errorf("trigger %q: max_body_bytes must be between 0 and %d bytes", t.Name, MaxWebhookBodyBytes)
		}
		if t.RateLimit != nil {
			if t.RateLimit.Requests < 0 {
				return fmt.Errorf("trigger %q: rate_limit.requests must not be negative", t.Name)
			}
			if t.RateLimit.Requests > 0 && t.RateLimit.Interval.Std() <= 0 {
				return fmt.Errorf("trigger %q: rate_limit.interval must be positive", t.Name)
			}
			if t.RateLimit.Burst < 0 {
				return fmt.Errorf("trigger %q: rate_limit.burst must not be negative", t.Name)
			}
		}
	}
	return nil
}

// validateMCPToolLists checks one server's tools and idempotent_tools: both
// hold unique names, and an idempotent tool must also be allowed when the
// server has an allowlist.
func validateMCPToolLists(s MCPServerConfig) error {
	allowed := make(map[string]bool, len(s.Tools))
	for _, name := range s.Tools {
		if name == "" {
			return fmt.Errorf("mcp server %q: tools hold an empty name", s.Name)
		}
		if allowed[name] {
			return fmt.Errorf("mcp server %q: duplicate tool %q", s.Name, name)
		}
		allowed[name] = true
	}
	idempotent := make(map[string]bool, len(s.IdempotentTools))
	for _, name := range s.IdempotentTools {
		if name == "" {
			return fmt.Errorf("mcp server %q: idempotent_tools hold an empty name", s.Name)
		}
		if idempotent[name] {
			return fmt.Errorf("mcp server %q: duplicate idempotent tool %q", s.Name, name)
		}
		idempotent[name] = true
		if len(allowed) > 0 && !allowed[name] {
			return fmt.Errorf("mcp server %q: idempotent tool %q is not in tools", s.Name, name)
		}
	}
	return nil
}

// ModelAllowed reports whether name is the default model or an explicitly
// allowed model. An empty allow list permits only the default model.
func (c LLMConfig) ModelAllowed(name string) bool {
	if name == c.Model {
		return true
	}
	for _, allowed := range c.AllowModels {
		if name == allowed {
			return true
		}
	}
	return false
}

// Bool returns the value of p or def when p is nil.
func Bool(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// SecretEnvNames lists the environment variables this configuration reads
// secrets from: the admin token, the LLM key, artifact store credentials, the
// queue password, trigger secrets, and env: header references. Project bundle
// templates may never read them. MAYFLY_* is reserved separately.
func (c *Config) SecretEnvNames() []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	addHeaders := func(headers map[string]string) {
		for _, value := range headers {
			if name, ok := strings.CutPrefix(value, "env:"); ok {
				add(name)
			}
		}
	}
	add(c.Server.TokenEnv)
	for _, token := range c.Server.Tokens {
		add(token.SecretEnv)
	}
	add(c.LLM.APIKeyEnv)
	addHeaders(c.LLM.Headers)
	add(c.Artifacts.S3.AccessKeyEnv)
	add(c.Artifacts.S3.SecretKeyEnv)
	add(c.Artifacts.S3.SessionTokenEnv)
	add(c.Queue.Redis.PasswordEnv)
	for _, trigger := range c.Triggers {
		add(trigger.SecretEnv)
	}
	for _, source := range c.Skills.Sources {
		addHeaders(source.Headers)
	}
	for _, server := range c.MCP.Servers {
		addHeaders(server.Headers)
	}
	sort.Strings(out)
	return out
}
