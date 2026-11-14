package session

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/agents"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/project"
)

func pluginTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Runtime.StateDir = t.TempDir()
	return cfg
}

func pluginTestBundle(manifests []plugin.Manifest, agentList ...agents.Agent) *project.Bundle {
	bundle := &project.Bundle{Agents: agentList, Plugins: map[string]plugin.Manifest{}}
	for _, manifest := range manifests {
		if manifest.Scope == "" {
			manifest.Scope = "shared"
		}
		bundle.Plugins[manifest.Name] = manifest
	}
	return bundle
}

func agentSelectingPlugins(name string, names ...string) agents.Agent {
	return agents.Agent{
		Source:     "agents.yaml",
		Definition: agents.Definition{Name: name, Plugins: &names},
	}
}

func testPlugin(name string, placement plugin.Placement, tools ...string) plugin.Manifest {
	manifest := plugin.Manifest{Name: name, Placement: placement, Origin: name + "/plugin.yaml"}
	for _, tool := range tools {
		manifest.Tools = append(manifest.Tools, plugin.Tool{Name: tool})
	}
	return manifest
}

func validatePlugins(t *testing.T, cfg *config.Config, bundle *project.Bundle) error {
	t.Helper()
	return Validate(cfg, bundle)
}

func TestAgentUnknownPlugin(t *testing.T) {
	cfg := pluginTestConfig(t)
	bundle := pluginTestBundle(nil, agentSelectingPlugins("writer", "ghost"))
	err := validatePlugins(t, cfg, bundle)
	if err == nil || !strings.Contains(err.Error(), `agent "writer" references unknown plugin "ghost"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentPluginScope(t *testing.T) {
	cfg := pluginTestConfig(t)
	private := testPlugin("notify", plugin.Sidecar, "post")
	private.Scope = "agent:other"
	bundle := pluginTestBundle([]plugin.Manifest{private}, agentSelectingPlugins("writer", "notify"))
	err := validatePlugins(t, cfg, bundle)
	if err == nil || !strings.Contains(err.Error(), `agent "writer" references plugin "notify" from another scope`) {
		t.Fatalf("err = %v", err)
	}

	private.Scope = "agent:writer"
	if err := validatePlugins(t, cfg, pluginTestBundle([]plugin.Manifest{private}, agentSelectingPlugins("writer", "notify"))); err != nil {
		t.Fatalf("owner: %v", err)
	}
}

func TestAgentPluginHarness(t *testing.T) {
	cfg := pluginTestConfig(t)
	manifest := testPlugin("notify", plugin.Sidecar, "post")
	agent := agentSelectingPlugins("writer", "notify")
	agent.Harness = "pi"
	err := validatePlugins(t, cfg, pluginTestBundle([]plugin.Manifest{manifest}, agent))
	if err == nil || !strings.Contains(err.Error(), `uses harness "pi"; plugins need the built-in harness`) {
		t.Fatalf("err = %v", err)
	}

	agent.Harness = "mayfly"
	if err := validatePlugins(t, cfg, pluginTestBundle([]plugin.Manifest{manifest}, agent)); err != nil {
		t.Fatalf("mayfly harness: %v", err)
	}
}

func TestAgentPluginSecretsWithShell(t *testing.T) {
	manifest := testPlugin("notify", plugin.Inline, "post")
	manifest.Secrets = []plugin.Secret{{Env: "NOTIFY_WEBHOOK_URL", Secret: "mayfly-plugin-notify", Key: "webhook-url"}}
	shell := true
	agent := agentSelectingPlugins("writer", "notify")
	agent.AllowShell = &shell

	for _, tc := range []struct {
		placement plugin.Placement
		backend   string
		want      string
	}{
		{plugin.Inline, "kubernetes", "stores its secrets in the agent container; use placement: sidecar"},
		{plugin.Inline, "local", "stores its secrets in the agent container; use placement: sidecar"},
		{plugin.Sidecar, "kubernetes", ""},
		{plugin.Sidecar, "local", "the local backend runs plugin \"notify\" as inline"},
		{plugin.Service, "kubernetes", ""},
		{plugin.Service, "local", ""},
	} {
		cfg := pluginTestConfig(t)
		cfg.Runtime.Backend = tc.backend
		candidate := manifest
		candidate.Placement = tc.placement
		err := validatePlugins(t, cfg, pluginTestBundle([]plugin.Manifest{candidate}, agent))
		if tc.want == "" {
			if err != nil {
				t.Fatalf("%s/%s: %v", tc.placement, tc.backend, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s/%s: err = %v, want %q", tc.placement, tc.backend, err, tc.want)
		}
	}
}

func TestAgentPluginImageAllowlist(t *testing.T) {
	cfg := pluginTestConfig(t)
	cfg.Agents.AllowedImages = []string{"ghcr.io/acme/*"}
	manifest := testPlugin("notify", plugin.Sidecar, "post")
	manifest.Image = "evil.example/notify:1"
	err := validatePlugins(t, cfg, pluginTestBundle([]plugin.Manifest{manifest}, agentSelectingPlugins("writer", "notify")))
	if err == nil || !strings.Contains(err.Error(), `image "evil.example/notify:1" is not in agents.allowed_images`) {
		t.Fatalf("err = %v", err)
	}

	manifest.Image = "ghcr.io/acme/notify:1"
	if err := validatePlugins(t, cfg, pluginTestBundle([]plugin.Manifest{manifest}, agentSelectingPlugins("writer", "notify"))); err != nil {
		t.Fatalf("allowed image: %v", err)
	}
}

func TestAgentPluginMCPServerRef(t *testing.T) {
	cfg := pluginTestConfig(t)
	cfg.MCP.Servers = []config.MCPServerConfig{
		{Name: "crm", Transport: "http", URL: "https://crm.test/mcp"},
		{Name: "local-tools", Transport: "stdio", Command: "tools"},
	}
	manifest := testPlugin("crm", plugin.Service, "find_account")
	manifest.Service = &plugin.ServiceSpec{MCPServer: "crm"}
	agent := agentSelectingPlugins("writer", "crm")
	if err := validatePlugins(t, cfg, pluginTestBundle([]plugin.Manifest{manifest}, agent)); err != nil {
		t.Fatalf("http server: %v", err)
	}

	for _, tc := range []struct {
		server string
		want   string
	}{
		{"ghost", `"ghost", which is not defined`},
		{"local-tools", "which is not an http MCP server"},
	} {
		candidate := manifest
		candidate.Service = &plugin.ServiceSpec{MCPServer: tc.server}
		err := validatePlugins(t, cfg, pluginTestBundle([]plugin.Manifest{candidate}, agent))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.server, err, tc.want)
		}
	}

	cfg.MCP.Servers[0].Scope = "agent:other"
	err := validatePlugins(t, cfg, pluginTestBundle([]plugin.Manifest{manifest}, agent))
	if err == nil || !strings.Contains(err.Error(), `"crm", which is not visible to the agent`) {
		t.Fatalf("private server: %v", err)
	}
	cfg.MCP.Servers[0].Scope = "agent:writer"
	if err := validatePlugins(t, cfg, pluginTestBundle([]plugin.Manifest{manifest}, agent)); err != nil {
		t.Fatalf("owning agent: %v", err)
	}
}

func TestAgentPluginToolCollision(t *testing.T) {
	cfg := pluginTestConfig(t)
	bundle := pluginTestBundle(
		[]plugin.Manifest{
			testPlugin("a.b", plugin.Inline, "post"),
			testPlugin("a_b", plugin.Inline, "post"),
		},
		agentSelectingPlugins("writer", "a.b", "a_b"),
	)
	err := validatePlugins(t, cfg, bundle)
	if err == nil || !strings.Contains(err.Error(), `plugins "a.b" and "a_b" expose the same tool name "plugin_a_b_post"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentInfoListsPlugins(t *testing.T) {
	cfg := pluginTestConfig(t)
	bundle := pluginTestBundle(
		[]plugin.Manifest{testPlugin("notify", plugin.Sidecar, "post")},
		agentSelectingPlugins("writer", "notify"),
	)
	m, err := New(cfg, bundle, nil, hub.New(1), journal.NewMemory(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range m.Agents() {
		if info.Name != "writer" {
			continue
		}
		if len(info.Plugins) != 1 || info.Plugins[0] != "notify" {
			t.Fatalf("plugins = %v", info.Plugins)
		}
		return
	}
	t.Fatal("agent writer is missing")
}
