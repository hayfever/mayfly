package config

import (
	"strings"
	"testing"
)

// FuzzTokenSecretValidation checks the length rule and the error text for
// arbitrary bytes. A rejected secret must never appear in the error.
func FuzzTokenSecretValidation(f *testing.F) {
	f.Add("ci", "queue:read", "")
	f.Add("ci", "queue:read", strings.Repeat("a", MinTokenSecretLength-1))
	f.Add("ci", "queue:read", strings.Repeat("a", MinTokenSecretLength))
	f.Add("", "not-a-scope", strings.Repeat("z", 4096))

	f.Fuzz(func(t *testing.T, name, scope, secret string) {
		cfg := Default()
		cfg.Server.Tokens = []APITokenConfig{{
			Name:      name,
			Scopes:    []string{scope},
			SecretEnv: "MAYFLY_FUZZ_TOKEN",
			Secret:    secret,
		}}
		_ = cfg.Server.validateTokens()

		err := validateTokenSecret("server.tokens[0]", secret)
		if secret == "" {
			if err != nil {
				t.Fatalf("empty secret: %v", err)
			}
			return
		}
		if len(secret) < MinTokenSecretLength {
			if err == nil {
				t.Fatalf("secret of %d characters was accepted", len(secret))
			}
			const want = "server.tokens[0]: token secret must be at least 32 characters"
			if err.Error() != want {
				t.Fatalf("error = %q, want the fixed message %q", err, want)
			}
			return
		}
		if err != nil {
			t.Fatalf("secret of %d characters was rejected: %v", len(secret), err)
		}
	})
}
