package sessioncfg

import (
	"encoding/json"
	"reflect"
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

func inlinePlugin() Plugin {
	return Plugin{
		Name:      "word-stats",
		Placement: "inline",
		Image:     "example.com/mayfly-plugin-word-stats:1",
		Dir:       "/opt/mayfly/plugins/word-stats",
		Command:   "server/server",
		Args:      []string{"--serve"},
		Workdir:   "plugin",
		Env:       map[string]string{"DEMO_MODE": "1"},
		Secrets: []PluginSecret{{
			Env:    "DEMO_TOKEN",
			Secret: "mayfly-plugin-word-stats",
			Key:    "token",
		}},
		SecretsDir:         "/etc/mayfly/plugin-secrets/word-stats",
		Tools:              []PluginTool{{Name: "count_words", Idempotent: true, TimeoutSeconds: 120}},
		InitTimeoutSeconds: 30,
		MaxRestarts:        2,
		MaxOutputBytes:     65536,
		StderrBytes:        1 << 20,
	}
}

func sidecarPlugin() Plugin {
	return Plugin{
		Name:      "notify",
		Placement: "sidecar",
		Image:     "example.com/mayfly-plugin-notify:1",
		Command:   "bin/notify",
		Socket:    "/run/mayfly/plugins/notify/mcp.sock",
		Secrets: []PluginSecret{{
			Env:    "NOTIFY_WEBHOOK_URL",
			Secret: "mayfly-plugin-notify",
			Key:    "webhook-url",
		}},
		Tools:              []PluginTool{{Name: "post", TimeoutSeconds: 30}},
		InitTimeoutSeconds: 30,
		MaxOutputBytes:     65536,
		Sidecar: &PluginSidecar{
			Requests:  map[string]string{"cpu": "10m", "memory": "32Mi"},
			Limits:    map[string]string{"memory": "64Mi"},
			RunAsUser: 65532,
			Workspace: "none",
		},
	}
}

func servicePlugin() Plugin {
	return Plugin{
		Name:               "tickets",
		Placement:          "service",
		Required:           true,
		Tools:              []PluginTool{{Name: "get_ticket", Idempotent: true, TimeoutSeconds: 120}, {Name: "add_comment", TimeoutSeconds: 120}},
		InitTimeoutSeconds: 30,
		MaxOutputBytes:     65536,
	}
}

func TestPluginPlacementsRoundTrip(t *testing.T) {
	for _, plugin := range []Plugin{inlinePlugin(), sidecarPlugin(), servicePlugin()} {
		t.Run(plugin.Placement, func(t *testing.T) {
			cfg := validConfig()
			cfg.Plugins = []Plugin{plugin}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var back Config
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Plugins, back.Plugins) {
				t.Fatalf("plugins changed:\n got %+v\nwant %+v", back.Plugins, cfg.Plugins)
			}
		})
	}
}

func TestConfigValidatePlugins(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"empty name", func(c *Config) { c.Plugins[0].Name = "" }, "empty name"},
		{"duplicate name", func(c *Config) {
			c.Plugins = append(c.Plugins, c.Plugins[0])
		}, "duplicate plugin"},
		{"placement", func(c *Config) { c.Plugins[0].Placement = "remote" }, "unsupported placement"},
		{"inline dir", func(c *Config) { c.Plugins[0].Dir = "" }, "dir"},
		{"inline command", func(c *Config) { c.Plugins[0].Command = "" }, "command"},
		{"inline workdir", func(c *Config) { c.Plugins[0].Workdir = "" }, "workdir"},
		{"inline secrets dir", func(c *Config) { c.Plugins[0].SecretsDir = "" }, "secrets_dir"},
		{"sidecar socket", func(c *Config) {
			c.Plugins = []Plugin{sidecarPlugin()}
			c.Plugins[0].Socket = ""
		}, "socket"},
		{"sidecar command", func(c *Config) {
			c.Plugins = []Plugin{sidecarPlugin()}
			c.Plugins[0].Command = ""
		}, "command"},
		{"sidecar block", func(c *Config) {
			c.Plugins = []Plugin{sidecarPlugin()}
			c.Plugins[0].Sidecar = nil
		}, "sidecar"},
		{"no tools", func(c *Config) { c.Plugins[0].Tools = nil }, "at least one tool"},
		{"duplicate tool", func(c *Config) {
			c.Plugins[0].Tools = []PluginTool{{Name: "run", TimeoutSeconds: 1}, {Name: "run", TimeoutSeconds: 1}}
		}, "duplicate tool"},
		{"empty tool name", func(c *Config) {
			c.Plugins[0].Tools = []PluginTool{{TimeoutSeconds: 1}}
		}, "empty name"},
		{"tool timeout", func(c *Config) { c.Plugins[0].Tools[0].TimeoutSeconds = 0 }, "timeout_seconds"},
		{"init timeout", func(c *Config) { c.Plugins[0].InitTimeoutSeconds = 0 }, "init_timeout_seconds"},
		{"max output zero", func(c *Config) { c.Plugins[0].MaxOutputBytes = 0 }, "max_output_bytes"},
		{"max output cap", func(c *Config) { c.Plugins[0].MaxOutputBytes = 70000 }, "max_output_bytes"},
		{"reserved secret", func(c *Config) { c.Plugins[0].Secrets[0].Env = EnvToken }, "reserved"},
		{"secret name", func(c *Config) { c.Plugins[0].Secrets[0].Env = "DEMO-TOKEN" }, "invalid environment variable name"},
		{"duplicate secret", func(c *Config) {
			c.Plugins[0].Secrets = append(c.Plugins[0].Secrets, c.Plugins[0].Secrets[0])
		}, "duplicate secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Plugins = []Plugin{inlinePlugin()}
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
