// Package config loads and validates the control plane configuration.
package config

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

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

// Config is the root configuration document.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Sessions  SessionsConfig  `yaml:"sessions"`
	Runtime   RuntimeConfig   `yaml:"runtime"`
	LLM       LLMConfig       `yaml:"llm"`
	Artifacts ArtifactsConfig `yaml:"artifacts"`
	Skills    SkillsConfig    `yaml:"skills"`
	MCP       MCPConfig       `yaml:"mcp"`
	Agents    AgentsConfig    `yaml:"agents"`
	Queue     QueueConfig     `yaml:"queue"`
	Triggers  []TriggerConfig `yaml:"triggers"`

	// AdminToken is resolved from the environment at load time and never
	// serialized.
	AdminToken string `yaml:"-"`
}

// ServerConfig covers the public HTTP listener.
type ServerConfig struct {
	Addr            string   `yaml:"addr"`
	PublicURL       string   `yaml:"public_url"`
	InternalURL     string   `yaml:"internal_url"`
	TokenEnv        string   `yaml:"token_env"`
	AttachTicketTTL Duration `yaml:"attach_ticket_ttl"`
	MaxBodyBytes    int64    `yaml:"max_body_bytes"`
}

// SessionsConfig controls session defaults and runtime limits.
type SessionsConfig struct {
	Namespace          string   `yaml:"namespace"`
	DefaultTTL         Duration `yaml:"default_ttl"`
	MaxTTL             Duration `yaml:"max_ttl"`
	IdleTimeout        Duration `yaml:"idle_timeout"`
	CloseGrace         Duration `yaml:"close_grace"`
	EventBuffer        int      `yaml:"event_buffer"`
	MaxSteps           int      `yaml:"max_steps"`
	MaxHistoryMessages int      `yaml:"max_history_messages"`
	MaxConcurrent      int      `yaml:"max_concurrent"`
	Retention          Duration `yaml:"retention"`
	ConnectTimeout     Duration `yaml:"connect_timeout"`
	AllowShell         *bool    `yaml:"allow_shell"`
	ShellTimeout       Duration `yaml:"shell_timeout"`
	WorkspaceDir       string   `yaml:"workspace_dir"`
	ArtifactsDir       string   `yaml:"artifacts_dir"`
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
	Resources         ResourceConfig    `yaml:"resources"`
	Env               map[string]string `yaml:"env"`
	PollInterval      Duration          `yaml:"poll_interval"`
	PodLabels         map[string]string `yaml:"pod_labels"`
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
}

// ArtifactsConfig configures durable artifact export.
type ArtifactsConfig struct {
	Store    string           `yaml:"store"` // local or s3
	Local    LocalStoreConfig `yaml:"local"`
	S3       S3StoreConfig    `yaml:"s3"`
	MaxBytes int64            `yaml:"max_bytes"`
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

// MCPConfig lists MCP servers that sessions can use.
type MCPConfig struct {
	Servers []MCPServerConfig `yaml:"servers"`
}

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
	Name          string           `yaml:"name"`
	Type          string           `yaml:"type"` // generic, github, slack, datadog
	SecretEnv     string           `yaml:"secret_env"`
	AllowUnsigned bool             `yaml:"allow_unsigned"`
	Template      string           `yaml:"template"`
	TTL           Duration         `yaml:"ttl"`
	Model         string           `yaml:"model"`
	Agent         string           `yaml:"agent"`
	Skills        []string         `yaml:"skills"`
	MCPServers    []string         `yaml:"mcp_servers"`
	RateLimit     *RateLimitConfig `yaml:"rate_limit"`
	Secret        string           `yaml:"-"`
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
		Server: ServerConfig{
			Addr:            ":8080",
			PublicURL:       "http://localhost:8080",
			TokenEnv:        "MAYFLY_API_TOKEN",
			AttachTicketTTL: Duration(60 * time.Second),
			MaxBodyBytes:    4 << 20,
		},
		Sessions: SessionsConfig{
			Namespace:          "mayfly",
			DefaultTTL:         Duration(time.Hour),
			MaxTTL:             Duration(4 * time.Hour),
			IdleTimeout:        Duration(20 * time.Minute),
			CloseGrace:         Duration(90 * time.Second),
			EventBuffer:        5000,
			MaxSteps:           24,
			MaxHistoryMessages: 120,
			MaxConcurrent:      50,
			Retention:          Duration(30 * time.Minute),
			ConnectTimeout:     Duration(2 * time.Minute),
			AllowShell:         new(false),
			ShellTimeout:       Duration(120 * time.Second),
			WorkspaceDir:       "/workspace",
			ArtifactsDir:       "/var/lib/mayfly/artifacts",
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
		MCP:    MCPConfig{},
		Queue: QueueConfig{
			Backend:           "memory",
			MaxJobs:           1000,
			MaxAttempts:       5,
			RetryBase:         Duration(5 * time.Second),
			VisibilityTimeout: Duration(5 * time.Minute),
			DedupeTTL:         Duration(72 * time.Hour),
			PollInterval:      Duration(2 * time.Second),
			Redis:             RedisQueueConfig{KeyPrefix: "mayfly"},
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
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
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
	if c.LLM.MaxBodyBytes <= 0 {
		c.LLM.MaxBodyBytes = 16 << 20
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
	if c.Runtime.WorkspaceLimit == "" {
		c.Runtime.WorkspaceLimit = "1Gi"
	}
	if c.Runtime.ArtifactLimit == "" {
		c.Runtime.ArtifactLimit = "1Gi"
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
	c.AdminToken = os.Getenv(c.Server.TokenEnv)
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
