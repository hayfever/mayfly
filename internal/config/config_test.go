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
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if err := decoder.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("%s: %v", path, err)
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
	if err := decoder.Decode(Default()); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("%s config subtree: %v", path, err)
	}
}
