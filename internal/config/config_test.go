package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestLoadExampleLocal(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "config.local.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Runtime.Backend != "local" {
		t.Fatalf("backend = %q", cfg.Runtime.Backend)
	}
	if !Bool(cfg.Sessions.AllowShell, false) {
		t.Fatalf("allow_shell was not enabled")
	}
	if cfg.Sessions.DefaultTTL.Std() != 30*time.Minute {
		t.Fatalf("default ttl = %s", cfg.Sessions.DefaultTTL.Std())
	}
	if len(cfg.Triggers) == 0 || !cfg.Triggers[0].AllowUnsigned {
		t.Fatalf("expected an unsigned generic trigger")
	}
}

func TestLoadRejectsBadDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := []byte("sessions:\n  default_ttl: forever\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for an invalid duration")
	}
}

func TestValidateRejectsMaxTTLBelowDefault(t *testing.T) {
	cfg := Default()
	cfg.Sessions.DefaultTTL = Duration(time.Hour)
	cfg.Sessions.MaxTTL = Duration(time.Minute)
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected max_ttl validation error")
	}
}

func TestModelAllowlist(t *testing.T) {
	cfg := LLMConfig{Model: "default", AllowModels: []string{"extra"}}
	if !cfg.ModelAllowed("default") || !cfg.ModelAllowed("extra") {
		t.Fatal("allowed models were rejected")
	}
	if cfg.ModelAllowed("other") {
		t.Fatal("unlisted model was allowed")
	}
	empty := LLMConfig{Model: "default"}
	if empty.ModelAllowed("extra") {
		t.Fatal("empty allowlist must reject non-default models")
	}
}

func TestValidateSkillSources(t *testing.T) {
	base := func() *Config {
		cfg := Default()
		cfg.Runtime.Backend = "local"
		cfg.Runtime.AgentBinary = "agent"
		cfg.LLM.BaseURL = "https://example.com/v1"
		cfg.LLM.Model = "model"
		cfg.applyDefaults()
		return cfg
	}
	valid := base()
	valid.Skills.Sources = []SkillSource{{
		Name:   "skills",
		URL:    "https://example.com/skills.tar.gz",
		SHA256: strings.Repeat("a", 64),
		Subdir: "skills",
	}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}

	badURL := base()
	badURL.Skills.Sources = []SkillSource{{Name: "bad", URL: "file:///etc/passwd", SHA256: strings.Repeat("a", 64)}}
	if err := badURL.Validate(); err == nil {
		t.Fatal("expected URL validation error")
	}

	badHash := base()
	badHash.Skills.Sources = []SkillSource{{Name: "bad", URL: "https://example.com/x", SHA256: "abc"}}
	if err := badHash.Validate(); err == nil {
		t.Fatal("expected sha256 validation error")
	}

	negative := base()
	strip := -1
	negative.Skills.Sources = []SkillSource{{Name: "bad", URL: "https://example.com/x", SHA256: strings.Repeat("a", 64), StripComponents: &strip}}
	if err := negative.Validate(); err == nil {
		t.Fatal("expected strip_components validation error")
	}
}

func TestSkillSourceHeaderEnvironment(t *testing.T) {
	t.Setenv("TEST_SKILL_TOKEN", "secret")
	cfg := Default()
	cfg.Skills.Sources = []SkillSource{{
		Name:    "skills",
		URL:     "https://example.com/skills.tar.gz",
		SHA256:  strings.Repeat("a", 64),
		Headers: map[string]string{"Authorization": "env:TEST_SKILL_TOKEN"},
	}}
	if err := cfg.resolveSecrets(); err != nil {
		t.Fatal(err)
	}
	if cfg.Skills.Sources[0].ResolvedHeaders["Authorization"] != "secret" {
		t.Fatalf("resolved = %v", cfg.Skills.Sources[0].ResolvedHeaders)
	}
}

func TestRepoSkillSourceResolution(t *testing.T) {
	cfg := Default()
	cfg.Runtime.Backend = "local"
	cfg.Runtime.AgentBinary = "agent"
	cfg.LLM.BaseURL = "https://example.com/v1"
	cfg.LLM.Model = "model"
	cfg.applyDefaults()
	cfg.Skills.Sources = []SkillSource{{
		Name:   "repo",
		Repo:   "https://github.com/acme/skills",
		Ref:    "abc123",
		SHA256: strings.Repeat("a", 64),
	}}
	if err := cfg.resolveSecrets(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	want := "https://github.com/acme/skills/archive/abc123.tar.gz"
	if cfg.Skills.Sources[0].ResolvedURL != want {
		t.Fatalf("resolved = %q", cfg.Skills.Sources[0].ResolvedURL)
	}
}

func TestSkillsCacheDir(t *testing.T) {
	derived := validBase()
	want := filepath.Join(".mayfly", "skill-cache")
	if derived.Skills.CacheDir != want {
		t.Fatalf("derived cache_dir = %q, want %q", derived.Skills.CacheDir, want)
	}

	besideJournal := Default()
	besideJournal.Journal.Bolt.Path = "/var/lib/mayfly/journal.db"
	besideJournal.applyDefaults()
	if besideJournal.Skills.CacheDir != "/var/lib/mayfly/skill-cache" {
		t.Fatalf("derived cache_dir = %q", besideJournal.Skills.CacheDir)
	}

	memory := Default()
	memory.Journal.Backend = "memory"
	memory.applyDefaults()
	if memory.Skills.CacheDir != "" {
		t.Fatalf("memory journal derived cache_dir = %q", memory.Skills.CacheDir)
	}

	explicit := validBase()
	explicit.Skills.CacheDir = "/var/lib/mayfly/skill-cache"
	explicit.applyDefaults()
	if explicit.Skills.CacheDir != "/var/lib/mayfly/skill-cache" {
		t.Fatalf("explicit cache_dir = %q", explicit.Skills.CacheDir)
	}
	if err := explicit.Validate(); err != nil {
		t.Fatal(err)
	}

	unclean := validBase()
	unclean.Skills.CacheDir = "state/skill-cache/"
	if err := unclean.Validate(); err == nil {
		t.Fatal("expected clean path validation error")
	}

	database := validBase()
	database.Skills.CacheDir = "/var/lib/mayfly/skills.db"
	if err := database.Validate(); err == nil {
		t.Fatal("expected database file validation error")
	}
}

func TestLoadAcceptsSkillsCacheDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := loadableConfig + "skills: {cache_dir: /var/lib/mayfly/skill-cache}\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Skills.CacheDir != "/var/lib/mayfly/skill-cache" {
		t.Fatalf("cache_dir = %q", cfg.Skills.CacheDir)
	}
}

func TestLoadLiteLLMExample(t *testing.T) {
	t.Setenv("MAYFLY_WEBHOOK_SECRET", "test")
	cfg, err := Load(filepath.Join("..", "..", "examples", "config.litellm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Model != "mayfly-default" {
		t.Fatalf("model = %q", cfg.LLM.Model)
	}
	for _, model := range []string{"claude-sonnet", "bedrock-claude", "openrouter-qwen", "agent-backend"} {
		if !cfg.LLM.ModelAllowed(model) {
			t.Fatalf("model %q not allowed", model)
		}
	}
}

func validBase() *Config {
	cfg := Default()
	cfg.Runtime.Backend = "local"
	cfg.Runtime.AgentBinary = "agent"
	cfg.LLM.BaseURL = "https://example.com/v1"
	cfg.LLM.Model = "model"
	cfg.applyDefaults()
	return cfg
}

func TestSessionDefaultsDisableShellAndBoundSessions(t *testing.T) {
	cfg := Default()
	if Bool(cfg.Sessions.AllowShell, true) {
		t.Fatal("allow_shell must be off by default")
	}
	if cfg.Sessions.MaxConcurrent <= 0 {
		t.Fatal("max_concurrent must default to a positive cap")
	}
	if cfg.Sessions.Retention.Std() <= 0 {
		t.Fatal("retention must default to a positive duration")
	}
}

func TestValidateRejectsReservedRuntimeEnv(t *testing.T) {
	cfg := validBase()
	cfg.Runtime.Env = map[string]string{"MAYFLY_SESSION_TOKEN": "spoofed"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected reserved env validation error")
	}
	cfg.Runtime.Env = map[string]string{"HF_HOME": "/opt/cache"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("literal env must be allowed: %v", err)
	}
}

func TestValidateS3CredentialSources(t *testing.T) {
	s3Base := func() *Config {
		cfg := validBase()
		cfg.Artifacts.Store = "s3"
		cfg.Artifacts.S3.Bucket = "bucket"
		cfg.Artifacts.S3.Region = "us-east-1"
		return cfg
	}
	if err := s3Base().Validate(); err != nil {
		t.Fatalf("default credential chain must be allowed: %v", err)
	}
	partial := s3Base()
	partial.Artifacts.S3.AccessKeyEnv = "AWS_ACCESS_KEY_ID"
	if err := partial.Validate(); err == nil {
		t.Fatal("expected a partial static credential error")
	}
	both := s3Base()
	both.Artifacts.S3.AccessKeyEnv = "AWS_ACCESS_KEY_ID"
	both.Artifacts.S3.SecretKeyEnv = "AWS_SECRET_ACCESS_KEY"
	if err := both.Validate(); err != nil {
		t.Fatalf("static credentials must be allowed: %v", err)
	}
}

func TestValidateQueueBounds(t *testing.T) {
	if err := validBase().Validate(); err != nil {
		t.Fatalf("default queue bounds must be allowed: %v", err)
	}
	shortLease := validBase()
	shortLease.Queue.VisibilityTimeout = Duration(30 * time.Second)
	if err := shortLease.Validate(); err == nil {
		t.Fatal("expected a visibility_timeout lower bound error")
	}
	shortDedupe := validBase()
	shortDedupe.Queue.DedupeTTL = Duration(500 * time.Millisecond)
	if err := shortDedupe.Validate(); err == nil {
		t.Fatal("expected a dedupe_ttl lower bound error")
	}
	negativeTombstone := validBase()
	negativeTombstone.Queue.TombstoneTTL = Duration(-time.Second)
	if err := negativeTombstone.Validate(); err == nil {
		t.Fatal("expected a tombstone_ttl error")
	}
}

func TestTriggerRateLimitDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := []byte(`runtime:
  backend: local
  agent_binary: ./agent
llm:
  base_url: http://localhost:9099/v1
  model: stub
triggers:
  - name: default-limit
    type: generic
    allow_unsigned: true
  - name: unlimited
    type: generic
    allow_unsigned: true
    rate_limit:
      requests: 0
  - name: custom
    type: generic
    allow_unsigned: true
    rate_limit:
      requests: 6
      interval: 30s
      burst: 2
`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Triggers[0].RateLimit; got.Requests != 30 || got.Burst != 10 || got.Interval.Std() != time.Minute {
		t.Fatalf("default rate limit = %+v", got)
	}
	if got := cfg.Triggers[1].RateLimit; got.Requests != 0 || got.Interval.Std() != time.Minute {
		t.Fatalf("disabled rate limit = %+v", got)
	}
	if got := cfg.Triggers[2].RateLimit; got.Requests != 6 || got.Burst != 2 || got.Interval.Std() != 30*time.Second {
		t.Fatalf("custom rate limit = %+v", got)
	}
}

func TestWebhookBodyLimitConfig(t *testing.T) {
	if got := Default().Server.WebhookMaxBodyBytes; got != DefaultWebhookMaxBodyBytes {
		t.Fatalf("default webhook limit = %d", got)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := []byte(`runtime:
  backend: local
  agent_binary: ./agent
llm:
  base_url: http://localhost:9099/v1
  model: stub
server:
  webhook_max_body_bytes: 2048
triggers:
  - name: server-default
    type: generic
    allow_unsigned: true
  - name: custom
    type: generic
    allow_unsigned: true
    max_body_bytes: 512
`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.WebhookBodyLimit(cfg.Triggers[0]); got != 2048 {
		t.Fatalf("server default limit = %d", got)
	}
	if got := cfg.WebhookBodyLimit(cfg.Triggers[1]); got != 512 {
		t.Fatalf("trigger limit = %d", got)
	}

	zero := validBase()
	zero.Server.WebhookMaxBodyBytes = 0
	if err := zero.Validate(); err == nil {
		t.Fatal("expected a positive server.webhook_max_body_bytes error")
	}

	tooLarge := validBase()
	tooLarge.Server.WebhookMaxBodyBytes = MaxWebhookBodyBytes + 1
	if err := tooLarge.Validate(); err == nil {
		t.Fatal("expected a server.webhook_max_body_bytes upper bound error")
	}

	negative := validBase()
	negative.Triggers = []TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true, MaxBodyBytes: -1}}
	if err := negative.Validate(); err == nil {
		t.Fatal("expected a negative trigger max_body_bytes error")
	}

	triggerTooLarge := validBase()
	triggerTooLarge.Triggers = []TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true, MaxBodyBytes: MaxWebhookBodyBytes + 1}}
	if err := triggerTooLarge.Validate(); err == nil {
		t.Fatal("expected a trigger max_body_bytes upper bound error")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("queue:\n  backnd: bbolt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	if !strings.Contains(err.Error(), "backnd") {
		t.Fatalf("error does not name the unknown key: %v", err)
	}
}

func TestLoadAcceptsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err != nil && strings.Contains(err.Error(), "parse config") {
		t.Fatalf("empty file should parse as an empty document: %v", err)
	}
}

// loadableConfig is the shortest document that passes Validate: the local
// runtime needs a binary, and every backend needs an LLM base URL.
const loadableConfig = "runtime: {backend: local, agent_binary: ./mayfly-agent}\n" +
	"llm: {base_url: \"http://localhost:9099/v1\", model: stub, allow_models: [stub]}\n"

func TestLoadAcceptsCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\n"+loadableConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Version != CurrentVersion {
		t.Fatalf("version = %d, want %d", cfg.Version, CurrentVersion)
	}
}

func TestLoadAssumesVersionWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(loadableConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Version != CurrentVersion {
		t.Fatalf("version = %d, want %d", cfg.Version, CurrentVersion)
	}
}

func TestLoadRejectsUnknownVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an unsupported version")
	}
	if !strings.Contains(err.Error(), "version 2") || !strings.Contains(err.Error(), "version 1") {
		t.Fatalf("error must name the rejected and the supported version: %v", err)
	}
}

func TestLoadRejectsMultipleDocuments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server: {addr: \":8081\"}\n---\nserver: {addr: \":8082\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("Load error = %v, want a multiple-document error", err)
	}
}

func TestShippedConfigsHaveNoUnknownFields(t *testing.T) {
	files := []string{
		filepath.Join("..", "..", "examples", "config.local.yaml"),
		filepath.Join("..", "..", "examples", "config.litellm.yaml"),
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg := Default()
		cfg.Version = 0
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if err := decoder.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("%s: %v", path, err)
		}
		if cfg.Version != CurrentVersion {
			t.Fatalf("%s: version = %d, want %d", path, cfg.Version, CurrentVersion)
		}
	}

	path := filepath.Join("..", "..", "deploy", "helm", "mayfly", "values.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Config yaml.Node `yaml:"config"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Config.Kind == 0 {
		t.Fatalf("%s: no config subtree", path)
	}
	sub, err := yaml.Marshal(&doc.Config)
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(sub))
	decoder.KnownFields(true)
	decoded := Default()
	decoded.Version = 0
	if err := decoder.Decode(decoded); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("%s config subtree: %v", path, err)
	}
	if decoded.Version != CurrentVersion {
		t.Fatalf("%s config subtree: version = %d, want %d", path, decoded.Version, CurrentVersion)
	}
}

func TestSecretEnvNames(t *testing.T) {
	cfg := Default()
	cfg.Server.TokenEnv = "MAYFLY_API_TOKEN"
	cfg.LLM.APIKeyEnv = "OPENAI_API_KEY"
	cfg.Queue.Redis.PasswordEnv = "REDIS_PASSWORD"
	cfg.Triggers = []TriggerConfig{{Name: "gh", SecretEnv: "GITHUB_WEBHOOK_SECRET"}}
	cfg.MCP.Servers = []MCPServerConfig{{Name: "slack", Headers: map[string]string{"Authorization": "env:SLACK_MCP_TOKEN", "X": "plain"}}}
	got := strings.Join(cfg.SecretEnvNames(), ",")
	for _, want := range []string{"MAYFLY_API_TOKEN", "OPENAI_API_KEY", "REDIS_PASSWORD", "GITHUB_WEBHOOK_SECRET", "SLACK_MCP_TOKEN"} {
		if !strings.Contains(got, want) {
			t.Fatalf("SecretEnvNames = %s, missing %s", got, want)
		}
	}
	if strings.Contains(got, "plain") {
		t.Fatalf("literal header value listed: %s", got)
	}
}

func TestSourcesTimeoutWithinJobTimeout(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "examples", "config.local.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Sources.Timeout = Duration(time.Hour)
	cfg.Sources.JobTimeout = Duration(time.Minute)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "sources.timeout") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigPluginsDefaults(t *testing.T) {
	cfg := Default()
	cfg.Sessions.Namespace = "agents"
	cfg.Runtime.Image = "example/agent:1"
	cfg.LLM.BaseURL = "http://localhost:11434/v1"
	cfg.LLM.Model = "test"
	cfg.applyDefaults()
	if cfg.Plugins.Namespace != "agents" {
		t.Fatalf("plugins.namespace = %q, want the sessions namespace", cfg.Plugins.Namespace)
	}
	if cfg.Plugins.ReconcileInterval.Std() != time.Minute {
		t.Fatalf("plugins.reconcile_interval = %s, want 1m", cfg.Plugins.ReconcileInterval.Std())
	}
	if !Bool(cfg.Plugins.Prune, false) {
		t.Fatal("plugins.prune did not default to true")
	}
	if cfg.Plugins.MaxInflightCalls != 256 {
		t.Fatalf("plugins.max_inflight_calls = %d, want 256", cfg.Plugins.MaxInflightCalls)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults did not validate: %v", err)
	}

	cfg.Plugins.ReconcileInterval = Duration(5 * time.Second)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "plugins.reconcile_interval") {
		t.Fatalf("short reconcile interval error = %v", err)
	}

	cfg = Default()
	cfg.Runtime.Image = "example/agent:1"
	cfg.LLM.BaseURL = "http://localhost:11434/v1"
	cfg.LLM.Model = "test"
	cfg.applyDefaults()
	cfg.Plugins.MaxInflightCalls = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "plugins.max_inflight_calls") {
		t.Fatalf("zero in-flight cap error = %v", err)
	}
}

func TestConfigMCPToolLists(t *testing.T) {
	base := func() *Config {
		cfg := validBase()
		cfg.MCP.Servers = []MCPServerConfig{{
			Name:            "search",
			Transport:       "http",
			URL:             "https://example.com/mcp",
			Tools:           []string{"query", "fetch"},
			IdempotentTools: []string{"query"},
		}}
		return cfg
	}

	cfg := base()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("tool lists did not validate: %v", err)
	}
	if got := cfg.MCP.Servers[0].IdempotentTools; len(got) != 1 || got[0] != "query" {
		t.Fatalf("idempotent tools = %v", got)
	}

	cases := []struct {
		name   string
		server MCPServerConfig
		want   string
	}{
		{
			name:   "idempotent tool outside the allowlist",
			server: MCPServerConfig{Tools: []string{"query"}, IdempotentTools: []string{"send"}},
			want:   "not in tools",
		},
		{
			name:   "duplicate tool",
			server: MCPServerConfig{Tools: []string{"query", "query"}},
			want:   "duplicate tool",
		},
		{
			name:   "duplicate idempotent tool",
			server: MCPServerConfig{IdempotentTools: []string{"query", "query"}},
			want:   "duplicate idempotent tool",
		},
		{
			name:   "empty tool name",
			server: MCPServerConfig{Tools: []string{""}},
			want:   "empty name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validBase()
			server := tc.server
			server.Name = "search"
			server.Transport = "http"
			server.URL = "https://example.com/mcp"
			cfg.MCP.Servers = []MCPServerConfig{server}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}

	// Without an allowlist, idempotent_tools may name any tool.
	cfg = validBase()
	cfg.MCP.Servers = []MCPServerConfig{{
		Name:            "search",
		Transport:       "stdio",
		Command:         "mcp-search",
		IdempotentTools: []string{"query"},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("idempotent tools without an allowlist did not validate: %v", err)
	}
}

func TestConfigPluginVolumeLimit(t *testing.T) {
	cfg := validBase()
	if cfg.Runtime.PluginVolumeLimit != "512Mi" {
		t.Fatalf("runtime.plugin_volume_limit = %q, want 512Mi", cfg.Runtime.PluginVolumeLimit)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default plugin volume limit did not validate: %v", err)
	}

	cfg = validBase()
	cfg.Runtime.PluginVolumeLimit = "not-a-quantity"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "runtime.plugin_volume_limit") {
		t.Fatalf("invalid plugin volume limit error = %v", err)
	}

	cfg = validBase()
	cfg.Runtime.PluginVolumeLimit = "2Gi"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit plugin volume limit did not validate: %v", err)
	}
}

func TestShutdownSharesSplitTheBudget(t *testing.T) {
	cfg := Default()
	httpShare, workerShare, managerShare := cfg.Server.ShutdownShares()
	if httpShare != 5*time.Second || workerShare != 10*time.Second || managerShare != 10*time.Second {
		t.Fatalf("default shares = %v/%v/%v", httpShare, workerShare, managerShare)
	}
	cfg.Server.ShutdownBudget = Duration(0)
	httpShare, workerShare, managerShare = cfg.Server.ShutdownShares()
	if httpShare+workerShare+managerShare != DefaultShutdownBudget {
		t.Fatalf("unset budget shares = %v/%v/%v", httpShare, workerShare, managerShare)
	}
	cfg.Server.ShutdownBudget = Duration(10 * time.Second)
	httpShare, workerShare, managerShare = cfg.Server.ShutdownShares()
	if httpShare+workerShare+managerShare != 10*time.Second {
		t.Fatalf("shares do not add up to the budget: %v/%v/%v", httpShare, workerShare, managerShare)
	}
	if httpShare <= 0 || workerShare <= 0 || managerShare <= 0 {
		t.Fatalf("a share is empty: %v/%v/%v", httpShare, workerShare, managerShare)
	}
}

func TestValidateRejectsTinyShutdownBudget(t *testing.T) {
	cfg := Default()
	cfg.Server.ShutdownBudget = Duration(time.Second)
	if err := cfg.Validate(); err == nil {
		t.Fatal("a one-second shutdown budget was accepted")
	}
}

func TestMetricsDefaults(t *testing.T) {
	cfg := Default()
	if !cfg.Server.MetricsEnabled() || !cfg.Server.MetricsOnMainPort() || !cfg.Server.MetricsRequireAuth() {
		t.Fatal("metrics must default to enabled, on the main port, with the admin token")
	}
	cfg.Server.Metrics.Address = ":9090"
	if cfg.Server.MetricsOnMainPort() {
		t.Fatal("a metrics address must move the endpoint off the main port")
	}
	if cfg.Server.MetricsRequireAuth() {
		t.Fatal("a separate metrics address must not require the admin token by default")
	}
	cfg.Server.Metrics.RequireAuth = new(true)
	if !cfg.Server.MetricsRequireAuth() {
		t.Fatal("require_auth = true must apply on a separate address")
	}
	cfg.Server.Metrics.Enabled = new(false)
	if cfg.Server.MetricsEnabled() {
		t.Fatal("enabled = false must turn the endpoint off")
	}
}

func TestLoadMetricsSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := []byte("runtime:\n  backend: local\n  agent_binary: /usr/local/bin/mayfly-agent\nllm:\n  base_url: http://127.0.0.1:1/v1\n  model: test-model\nserver:\n  metrics:\n    address: \":9090\"\n    require_auth: true\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Metrics.Address != ":9090" || cfg.Server.MetricsOnMainPort() {
		t.Fatalf("metrics address = %q", cfg.Server.Metrics.Address)
	}
	if !cfg.Server.MetricsRequireAuth() {
		t.Fatal("require_auth was not read")
	}
}

func TestValidateRejectsMetricsOnMainAddress(t *testing.T) {
	cfg := Default()
	cfg.Server.Metrics.Address = cfg.Server.Addr
	if err := cfg.Validate(); err == nil {
		t.Fatal("the main address was accepted as the metrics address")
	}
}

func TestSessionByteLimits(t *testing.T) {
	cfg := Default()
	if cfg.Sessions.MaxEventBytes != DefaultMaxEventBytes || cfg.Sessions.EventBufferBytes != DefaultEventBufferBytes {
		t.Fatalf("defaults = %d / %d", cfg.Sessions.MaxEventBytes, cfg.Sessions.EventBufferBytes)
	}
	decoder := yaml.NewDecoder(strings.NewReader("sessions:\n  max_event_bytes: 1024\n  event_buffer_bytes: 4096\n"))
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Sessions.MaxEventBytes != 1024 || cfg.Sessions.EventBufferBytes != 4096 {
		t.Fatalf("loaded limits = %d / %d", cfg.Sessions.MaxEventBytes, cfg.Sessions.EventBufferBytes)
	}
	cfg.Sessions.MaxEventBytes = 0
	cfg.Sessions.EventBufferBytes = 0
	cfg.applyDefaults()
	if cfg.Sessions.MaxEventBytes != DefaultMaxEventBytes || cfg.Sessions.EventBufferBytes != DefaultEventBufferBytes {
		t.Fatalf("zero limits did not fall back to defaults")
	}
}

func TestBodyReadTimeoutDefaults(t *testing.T) {
	cfg := Default()
	if got := cfg.Server.BodyReadTimeout.Std(); got != DefaultBodyReadTimeout {
		t.Fatalf("default body read timeout = %s", got)
	}
	cfg.Server.BodyReadTimeout = 0
	cfg.applyDefaults()
	if got := cfg.Server.BodyReadTimeout.Std(); got != DefaultBodyReadTimeout {
		t.Fatalf("zero body read timeout = %s, want the default", got)
	}
}
