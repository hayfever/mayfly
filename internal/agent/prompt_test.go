package agent

import (
	"strings"
	"testing"

	"github.com/hayden/mayfly/internal/sessioncfg"
)

func TestSystemPromptCustomAgent(t *testing.T) {
	cfg := sessioncfg.Config{
		Agent:        "triage",
		SystemPrompt: "You are the triage agent.",
		SessionID:    "s1",
		Title:        "Triage",
		Model:        "m",
		WorkspaceDir: "/workspace",
		Skills: []sessioncfg.Skill{{
			Name: "incident-triage",
			Body: "Check evidence.",
		}},
		MCPServers: []sessioncfg.MCPServer{{Name: "echo"}},
	}
	out := systemPrompt(cfg)
	for _, want := range []string{"You are the triage agent.", "Agent: triage", "Session: s1", "MCP servers configured: echo", "## Skill: incident-triage", "Check evidence."} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt missing %q:\n%s", want, out)
		}
	}
}

func TestSystemPromptBuiltIn(t *testing.T) {
	cfg := sessioncfg.Config{SessionID: "s1", Model: "m", WorkspaceDir: "/workspace"}
	out := systemPrompt(cfg)
	if !strings.HasPrefix(out, "You are Mayfly") {
		t.Fatalf("unexpected built-in prompt: %s", out)
	}
	if strings.Contains(out, "Agent:") {
		t.Fatal("built-in prompt should not name a custom agent")
	}
}
