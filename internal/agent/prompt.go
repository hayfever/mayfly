package agent

import (
	"fmt"
	"strings"

	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// systemPrompt builds the model instructions from the agent prompt, runtime
// facts, and skills. A custom agent prompt replaces the built-in persona but
// still gets the environment and safety rules.
func systemPrompt(cfg sessioncfg.Config) string {
	var b strings.Builder
	if strings.TrimSpace(cfg.SystemPrompt) != "" {
		b.WriteString(strings.TrimSpace(cfg.SystemPrompt))
		b.WriteString("\n\n")
		writeEnvironment(&b, cfg)
	} else {
		b.WriteString("You are Mayfly, an autonomous agent running in an ephemeral, isolated session.\n\n")
		writeEnvironment(&b, cfg)
	}
	for _, skill := range cfg.Skills {
		b.WriteString("\n## Skill: ")
		b.WriteString(skill.Name)
		b.WriteString("\n")
		if skill.Description != "" {
			b.WriteString(skill.Description)
			b.WriteString("\n\n")
		}
		b.WriteString(skill.Body)
		b.WriteString("\n")
	}
	return b.String()
}

func writeEnvironment(b *strings.Builder, cfg sessioncfg.Config) {
	if cfg.Agent != "" {
		fmt.Fprintf(b, "Agent: %s\n", cfg.Agent)
	}
	if cfg.Harness != "" {
		fmt.Fprintf(b, "Harness: %s\n", cfg.Harness)
	}
	fmt.Fprintf(b, "Session: %s\n", cfg.SessionID)
	if cfg.Title != "" {
		fmt.Fprintf(b, "Title: %s\n", cfg.Title)
	}
	fmt.Fprintf(b, "Model: %s\n", cfg.Model)
	fmt.Fprintf(b, "Workspace: %s\n", cfg.WorkspaceDir)
	b.WriteString("\nRules:\n")
	b.WriteString("- Work only inside the workspace. Use the provided tools to read and write files.\n")
	b.WriteString("- Multiple people may send messages. Treat each message as a request and keep replies concise.\n")
	b.WriteString("- Use tools when they make the answer more accurate. Do not invent tool results.\n")
	b.WriteString("- When the task is complete, reply with a short summary and stop.\n")
	if len(cfg.MCPServers) > 0 {
		names := make([]string, 0, len(cfg.MCPServers))
		for _, s := range cfg.MCPServers {
			names = append(names, s.Name)
		}
		fmt.Fprintf(b, "- MCP servers configured: %s. Their tools are prefixed mcp_<server>_.\n", strings.Join(names, ", "))
	}
	b.WriteString("- The session is destroyed when it closes. Put deliverables in the workspace; they are exported as artifacts.\n")
}
