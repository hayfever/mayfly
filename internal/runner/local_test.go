package runner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// TestLocalStartsWithMinimalEnvironment checks a local session does not
// inherit control-plane or provider secrets from mayflyd's environment.
func TestLocalStartsWithMinimalEnvironment(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "agent.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > \"$PWD/env.out\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAYFLY_API_TOKEN", "admin-secret")
	t.Setenv("OPENAI_API_KEY", "llm-secret")
	t.Setenv("NOTION_TOKEN", "notion-secret")
	t.Setenv("FIRECRAWL_API_KEY", "firecrawl-secret")
	t.Setenv("LANG", "C.UTF-8")
	local, err := NewLocal(script, filepath.Join(dir, "state"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := local.Start(context.Background(), Spec{
		SessionID: "s1",
		Token:     "session-token",
		Env:       map[string]string{"EXTRA": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-handle.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("agent script did not exit")
	}
	raw, err := os.ReadFile(filepath.Join(handle.StateDir, "env.out"))
	if err != nil {
		t.Fatal(err)
	}
	env := string(raw)
	for _, secret := range []string{"admin-secret", "llm-secret", "notion-secret", "firecrawl-secret"} {
		if strings.Contains(env, secret) {
			t.Fatalf("session environment contains %q:\n%s", secret, env)
		}
	}
	for _, want := range []string{"MAYFLY_SESSION_TOKEN=session-token", "MAYFLY_CONFIG=", "EXTRA=yes", "LANG=C.UTF-8", "PATH="} {
		if !strings.Contains(env, want) {
			t.Fatalf("session environment is missing %q:\n%s", want, env)
		}
	}
}

// TestLocalWritesPluginSecrets checks the local backend writes each plugin
// secret to an owner-only file before the agent starts.
func TestLocalWritesPluginSecrets(t *testing.T) {
	dir := t.TempDir()
	secretsDir := filepath.Join(dir, "sessions", "s1", "plugin-secrets", "notify")
	script := filepath.Join(dir, "agent.sh")
	body := "#!/bin/sh\n" +
		"cat " + secretsDir + "/SLACK_TOKEN > token.out\n" +
		"stat -c '%a' " + secretsDir + " > dir.mode\n" +
		"stat -c '%a' " + secretsDir + "/SLACK_TOKEN > file.mode\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	local, err := NewLocal(script, filepath.Join(dir, "state"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := local.Start(context.Background(), Spec{
		SessionID: "s1",
		Token:     "session-token",
		Runtime: sessioncfg.Config{
			Plugins: []sessioncfg.Plugin{{
				Name:       "notify",
				Placement:  "inline",
				SecretsDir: secretsDir,
				Secrets:    []sessioncfg.PluginSecret{{Env: "SLACK_TOKEN"}},
			}},
		},
		PluginSecrets: map[string]map[string]string{
			"notify": {"SLACK_TOKEN": "secret-value"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-handle.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("agent script did not exit")
	}
	read := func(name string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(handle.StateDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(raw))
	}
	if got := read("token.out"); got != "secret-value" {
		t.Fatalf("secret file = %q", got)
	}
	if got := read("dir.mode"); got != "700" {
		t.Fatalf("secrets directory mode = %q, want 700", got)
	}
	if got := read("file.mode"); got != "600" {
		t.Fatalf("secret file mode = %q, want 600", got)
	}
}
