package config

import (
	"fmt"
	"strings"
	"time"
)

// API token scopes. Every /api/v1 route requires one of these, except the
// attach WebSocket, which uses a one-time ticket instead.
const (
	ScopeAdmin          = "admin"
	ScopeSessionsRead   = "sessions:read"
	ScopeSessionsWrite  = "sessions:write"
	ScopeQueueRead      = "queue:read"
	ScopeQueueWrite     = "queue:write"
	ScopeSchedulesRead  = "schedules:read"
	ScopeSchedulesWrite = "schedules:write"
)

// DefaultTokenName names the token from server.token_env. That token has the
// admin scope, so an existing deployment keeps one working credential.
const DefaultTokenName = "default"

// MinTokenSecretLength is the shortest accepted API token secret. A shorter
// secret is rejected at load: constant-time comparison does not help a value
// that can be guessed.
const MinTokenSecretLength = 32

// validateTokenSecret checks one resolved secret. The error names the config
// location, never the value.
func validateTokenSecret(where, secret string) error {
	if secret != "" && len(secret) < MinTokenSecretLength {
		return fmt.Errorf("%s: token secret must be at least %d characters", where, MinTokenSecretLength)
	}
	return nil
}

// apiScopes lists every scope in documentation order.
var apiScopes = []string{
	ScopeAdmin,
	ScopeSessionsRead,
	ScopeSessionsWrite,
	ScopeQueueRead,
	ScopeQueueWrite,
	ScopeSchedulesRead,
	ScopeSchedulesWrite,
}

// ScopeNames returns every valid scope.
func ScopeNames() []string {
	return append([]string(nil), apiScopes...)
}

// ValidScope reports whether name is a scope the control plane enforces.
func ValidScope(name string) bool {
	for _, scope := range apiScopes {
		if scope == name {
			return true
		}
	}
	return false
}

// APITokenConfig is one named bearer token for the /api/v1 surface. The secret
// is resolved from SecretEnv at load time; it is never serialized and never
// logged.
type APITokenConfig struct {
	Name      string     `yaml:"name"`
	Scopes    []string   `yaml:"scopes"`
	SecretEnv string     `yaml:"secret_env"`
	ExpiresAt *time.Time `yaml:"expires_at"`
	Secret    string     `yaml:"-"`
}

// Expired reports whether the token is past its expiry. A token without
// expires_at never expires.
func (t APITokenConfig) Expired(now time.Time) bool {
	return t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)
}

// HasScope reports whether the token grants scope. The admin scope grants
// every scope.
func (t APITokenConfig) HasScope(scope string) bool {
	for _, granted := range t.Scopes {
		if granted == ScopeAdmin || granted == scope {
			return true
		}
	}
	return false
}

// APITokens returns the resolved token set. A configuration with
// server.tokens uses exactly those tokens. Otherwise the token from
// server.token_env is exposed as one token named "default" with the admin
// scope.
func (c *Config) APITokens() []APITokenConfig {
	if len(c.Server.Tokens) > 0 {
		return c.Server.Tokens
	}
	if c.AdminToken == "" {
		return nil
	}
	return []APITokenConfig{{
		Name:   DefaultTokenName,
		Scopes: []string{ScopeAdmin},
		Secret: c.AdminToken,
	}}
}

// validateTokens checks the declared tokens before their secrets are read.
func (s ServerConfig) validateTokens() error {
	names := map[string]bool{}
	envs := map[string]bool{}
	for i, token := range s.Tokens {
		where := fmt.Sprintf("server.tokens[%d]", i)
		if token.Name == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		if names[token.Name] {
			return fmt.Errorf("%s: duplicate token name %q", where, token.Name)
		}
		names[token.Name] = true
		if token.SecretEnv == "" {
			return fmt.Errorf("%s (%s): secret_env is required", where, token.Name)
		}
		if envs[token.SecretEnv] {
			return fmt.Errorf("%s (%s): secret_env %s is already used by another token", where, token.Name, token.SecretEnv)
		}
		envs[token.SecretEnv] = true
		if len(token.Scopes) == 0 {
			return fmt.Errorf("%s (%s): scopes must name at least one of %s", where, token.Name, strings.Join(apiScopes, ", "))
		}
		for _, scope := range token.Scopes {
			if !ValidScope(scope) {
				return fmt.Errorf("%s (%s): unknown scope %q; valid scopes are %s", where, token.Name, scope, strings.Join(apiScopes, ", "))
			}
		}
	}
	return nil
}
