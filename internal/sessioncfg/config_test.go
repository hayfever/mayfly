package sessioncfg

import (
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		Version:         Version,
		SessionID:       "s1",
		Model:           "test-model",
		PlatformURL:     "http://mayflyd:8080",
		LLMBaseURL:      "http://mayflyd:8080/v1",
		WorkspaceDir:    "/workspace",
		ArtifactsDir:    "/artifacts",
		MaxSteps:        20,
		MCPServers:      nil,
		Skills:          nil,
		RuntimeProtocol: 1,
	}
}

func TestValidateAcceptsRuntimeConfig(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeProtocol != 1 {
		t.Fatalf("protocol = %d", cfg.RuntimeProtocol)
	}
}

func TestValidateDefaultsProtocol(t *testing.T) {
	cfg := validConfig()
	cfg.RuntimeProtocol = 0
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeProtocol != 1 {
		t.Fatalf("protocol = %d", cfg.RuntimeProtocol)
	}
}

func TestValidateRejectsBrokenConfig(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"version", func(c *Config) { c.Version = 2 }, "version"},
		{"session", func(c *Config) { c.SessionID = "" }, "session_id"},
		{"model", func(c *Config) { c.Model = "" }, "model"},
		{"protocol", func(c *Config) { c.RuntimeProtocol = 3 }, "protocol"},
		{"platform", func(c *Config) { c.PlatformURL = "" }, "platform_url"},
		{"llm", func(c *Config) { c.LLMBaseURL = "" }, "llm_base_url"},
		{"workspace", func(c *Config) { c.WorkspaceDir = "" }, "workspace_dir"},
		{"artifacts dir", func(c *Config) { c.ArtifactsDir = "" }, "artifacts_dir"},
		{"max steps", func(c *Config) { c.MaxSteps = 0 }, "max_steps"},
		{"skill name", func(c *Config) { c.Skills = []Skill{{Body: "x"}} }, "empty name"},
		{"stdio command", func(c *Config) { c.MCPServers = []MCPServer{{Name: "s", Transport: "stdio"}} }, "command"},
		{"http url", func(c *Config) { c.MCPServers = []MCPServer{{Name: "s", Transport: "http"}} }, "url"},
		{"transport", func(c *Config) { c.MCPServers = []MCPServer{{Name: "s", Transport: "grpc"}} }, "unsupported transport"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReservedEnv(t *testing.T) {
	for _, name := range []string{EnvConfig, EnvToken} {
		if !IsReservedEnv(name) {
			t.Fatalf("%s not reserved", name)
		}
	}
	if IsReservedEnv("PATH") {
		t.Fatal("PATH reported reserved")
	}
}
