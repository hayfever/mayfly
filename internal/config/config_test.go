package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
