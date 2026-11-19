package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadTokenConfig(t *testing.T, raw string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLegacyTokenBecomesDefaultAdminToken(t *testing.T) {
	cfg := Default()
	cfg.AdminToken = "legacy-secret"
	tokens := cfg.APITokens()
	if len(tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(tokens))
	}
	if tokens[0].Name != DefaultTokenName {
		t.Fatalf("name = %q, want %q", tokens[0].Name, DefaultTokenName)
	}
	if tokens[0].Secret != "legacy-secret" {
		t.Fatalf("secret was not taken from the legacy token")
	}
	for _, scope := range ScopeNames() {
		if !tokens[0].HasScope(scope) {
			t.Fatalf("default token does not grant %s", scope)
		}
	}
}

func TestNoTokenResolvesToEmptySet(t *testing.T) {
	cfg := Default()
	if got := cfg.APITokens(); got != nil {
		t.Fatalf("tokens = %+v, want nil", got)
	}
}

func TestLoadNamedTokens(t *testing.T) {
	t.Setenv("MAYFLY_CI_TOKEN", "ci-secret")
	t.Setenv("MAYFLY_READ_TOKEN", "read-secret")
	cfg, err := loadTokenConfig(t, `
runtime:
  image: example/agent:1
llm:
  base_url: http://localhost:4000/v1
  model: gpt-4.1-mini
server:
  tokens:
    - name: ci
      scopes: [sessions:read, sessions:write]
      secret_env: MAYFLY_CI_TOKEN
      expires_at: "2030-01-02T03:04:05Z"
    - name: reader
      scopes: [sessions:read]
      secret_env: MAYFLY_READ_TOKEN
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tokens := cfg.APITokens()
	if len(tokens) != 2 {
		t.Fatalf("tokens = %d, want 2", len(tokens))
	}
	if tokens[0].Name != "ci" || tokens[0].Secret != "ci-secret" {
		t.Fatalf("first token = %+v", tokens[0])
	}
	if tokens[0].ExpiresAt == nil || !tokens[0].ExpiresAt.Equal(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("expires_at = %v", tokens[0].ExpiresAt)
	}
	if tokens[1].ExpiresAt != nil {
		t.Fatalf("token without expires_at has an expiry: %v", tokens[1].ExpiresAt)
	}
	if !tokens[0].HasScope(ScopeSessionsWrite) || tokens[1].HasScope(ScopeSessionsWrite) {
		t.Fatalf("scopes were not loaded: %+v", tokens)
	}
}

func TestNamedTokensReplaceTokenEnv(t *testing.T) {
	t.Setenv("MAYFLY_API_TOKEN", "legacy-secret")
	t.Setenv("MAYFLY_CI_TOKEN", "ci-secret")
	cfg, err := loadTokenConfig(t, `
runtime:
  image: example/agent:1
llm:
  base_url: http://localhost:4000/v1
  model: gpt-4.1-mini
server:
  tokens:
    - name: ci
      scopes: [queue:read]
      secret_env: MAYFLY_CI_TOKEN
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminToken != "" {
		t.Fatalf("legacy token was resolved even though server.tokens is set: %q", cfg.AdminToken)
	}
	tokens := cfg.APITokens()
	if len(tokens) != 1 || tokens[0].Name != "ci" {
		t.Fatalf("tokens = %+v", tokens)
	}
}

func TestLoadRejectsBadTokenConfig(t *testing.T) {
	t.Setenv("MAYFLY_CI_TOKEN", "ci-secret")
	t.Setenv("MAYFLY_OTHER_TOKEN", "other-secret")
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "duplicate name",
			raw: `
server:
  tokens:
    - {name: ci, scopes: [queue:read], secret_env: MAYFLY_CI_TOKEN}
    - {name: ci, scopes: [queue:write], secret_env: MAYFLY_OTHER_TOKEN}
`,
			want: "duplicate token name",
		},
		{
			name: "unknown scope",
			raw: `
server:
  tokens:
    - {name: ci, scopes: [queue:destroy], secret_env: MAYFLY_CI_TOKEN}
`,
			want: "unknown scope",
		},
		{
			name: "no scopes",
			raw: `
server:
  tokens:
    - {name: ci, scopes: [], secret_env: MAYFLY_CI_TOKEN}
`,
			want: "scopes must name at least one",
		},
		{
			name: "no secret env",
			raw: `
server:
  tokens:
    - {name: ci, scopes: [queue:read]}
`,
			want: "secret_env is required",
		},
		{
			name: "shared secret env",
			raw: `
server:
  tokens:
    - {name: ci, scopes: [queue:read], secret_env: MAYFLY_CI_TOKEN}
    - {name: other, scopes: [queue:write], secret_env: MAYFLY_CI_TOKEN}
`,
			want: "already used by another token",
		},
		{
			name: "unset secret env",
			raw: `
server:
  tokens:
    - {name: ci, scopes: [queue:read], secret_env: MAYFLY_MISSING_TOKEN}
`,
			want: "MAYFLY_MISSING_TOKEN is not set",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadTokenConfig(t, tc.raw); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAPITokenScopeAndExpiry(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)
	token := APITokenConfig{Name: "ci", Scopes: []string{ScopeQueueRead}}
	if !token.HasScope(ScopeQueueRead) || token.HasScope(ScopeQueueWrite) {
		t.Fatalf("scope check = %+v", token.Scopes)
	}
	if token.Expired(now) {
		t.Fatal("token without expires_at reported as expired")
	}
	token.ExpiresAt = &past
	if !token.Expired(now) {
		t.Fatal("past expiry not reported as expired")
	}
	token.ExpiresAt = &future
	if token.Expired(now) {
		t.Fatal("future expiry reported as expired")
	}
	admin := APITokenConfig{Name: "root", Scopes: []string{ScopeAdmin}}
	for _, scope := range append(ScopeNames(), "unknown:scope") {
		if !admin.HasScope(scope) {
			t.Fatalf("admin token does not grant %q", scope)
		}
	}
}

func TestTokenSecretEnvNamesAreReserved(t *testing.T) {
	cfg := Default()
	cfg.Server.TokenEnv = ""
	cfg.Server.Tokens = []APITokenConfig{
		{Name: "ci", Scopes: []string{ScopeQueueRead}, SecretEnv: "MAYFLY_CI_TOKEN"},
		{Name: "ops", Scopes: []string{ScopeAdmin}, SecretEnv: "MAYFLY_OPS_TOKEN"},
	}
	names := strings.Join(cfg.SecretEnvNames(), ",")
	if !strings.Contains(names, "MAYFLY_CI_TOKEN") || !strings.Contains(names, "MAYFLY_OPS_TOKEN") {
		t.Fatalf("SecretEnvNames = %s", names)
	}
}
