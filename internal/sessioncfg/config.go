// Package sessioncfg defines the configuration document handed from the
// control plane to a session runtime. It is serialized as session.json and
// mounted into the session container.
package sessioncfg

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const Version = 1

// Skill is a named instruction block injected into the agent system prompt.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Body        string `json:"body"`
}

// MCPServer describes one MCP server made available to the session.
// HTTP servers are always reached through the platform MCP proxy; URL points
// at the proxy, and credentials stay in the control plane.
type MCPServer struct {
	Name               string            `json:"name"`
	Transport          string            `json:"transport"` // stdio or http
	Command            string            `json:"command,omitempty"`
	Args               []string          `json:"args,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	URL                string            `json:"url,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	InitTimeoutSeconds int               `json:"init_timeout_seconds,omitempty"`
}

// Config is the full runtime configuration for one session.
type Config struct {
	Version            int            `json:"version"`
	SessionID          string         `json:"session_id"`
	Title              string         `json:"title"`
	Prompt             string         `json:"prompt"`
	Agent              string         `json:"agent"`
	Harness            string         `json:"harness"`
	SystemPrompt       string         `json:"system_prompt"`
	RuntimeProtocol    int            `json:"runtime_protocol"`
	AgentConfig        map[string]any `json:"agent_config,omitempty"`
	Model              string         `json:"model"`
	PlatformURL        string         `json:"platform_url"`
	LLMBaseURL         string         `json:"llm_base_url"`
	LLMTimeoutSeconds  int            `json:"llm_timeout_seconds"`
	MaxSteps           int            `json:"max_steps"`
	MaxHistoryMessages int            `json:"max_history_messages"`
	TTLSeconds         int            `json:"ttl_seconds"`
	IdleTimeoutSeconds int            `json:"idle_timeout_seconds"`
	WorkspaceDir       string         `json:"workspace_dir"`
	ArtifactsDir       string         `json:"artifacts_dir"`
	AllowShell         bool           `json:"allow_shell"`
	ShellTimeoutSecs   int            `json:"shell_timeout_seconds"`
	ArtifactMaxBytes   int64          `json:"artifact_max_bytes"`
	Skills             []Skill        `json:"skills"`
	MCPServers         []MCPServer    `json:"mcp_servers"`
	CreatedAt          time.Time      `json:"created_at"`
	AgentVersion       string         `json:"agent_version,omitempty"`
}

// Validate checks invariants that both the control plane and the runtime rely on.
func (c *Config) Validate() error {
	if c.Version != Version {
		return fmt.Errorf("unsupported session config version %d", c.Version)
	}
	if c.SessionID == "" {
		return errors.New("session_id is required")
	}
	if c.Model == "" {
		return errors.New("model is required")
	}
	if c.RuntimeProtocol == 0 {
		c.RuntimeProtocol = 1
	}
	if c.RuntimeProtocol != 1 {
		return fmt.Errorf("unsupported runtime protocol %d", c.RuntimeProtocol)
	}
	if c.PlatformURL == "" {
		return errors.New("platform_url is required")
	}
	if c.LLMBaseURL == "" {
		return errors.New("llm_base_url is required")
	}
	if c.WorkspaceDir == "" {
		return errors.New("workspace_dir is required")
	}
	if c.ArtifactsDir == "" {
		return errors.New("artifacts_dir is required")
	}
	if c.MaxSteps <= 0 {
		return errors.New("max_steps must be positive")
	}
	for i, s := range c.Skills {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("skill %d has an empty name", i)
		}
	}
	for _, s := range c.MCPServers {
		switch s.Transport {
		case "stdio":
			if s.Command == "" {
				return fmt.Errorf("mcp server %q: command is required for stdio", s.Name)
			}
		case "http":
			if s.URL == "" {
				return fmt.Errorf("mcp server %q: url is required for http", s.Name)
			}
		default:
			return fmt.Errorf("mcp server %q: unsupported transport %q", s.Name, s.Transport)
		}
	}
	return nil
}
