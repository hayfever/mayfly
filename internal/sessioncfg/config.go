// Package sessioncfg defines the configuration document handed from the
// control plane to a session runtime. It is serialized as session.json and
// mounted into the session container.
package sessioncfg

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hayfever/mayfly/internal/content"
)

const Version = 1

// envNamePattern is the name rule for a variable a plugin reads.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Environment variables that every session runtime receives from the control
// plane. Configuration must not set them, so a session cannot be pointed at a
// different config file or handed a token the control plane did not issue.
const (
	EnvConfig = "MAYFLY_CONFIG"
	EnvToken  = "MAYFLY_SESSION_TOKEN"
)

// IsReservedEnv reports whether name is owned by the control plane.
func IsReservedEnv(name string) bool {
	return name == EnvConfig || name == EnvToken
}

// Skill is a named instruction block injected into the agent system prompt.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Body        string `json:"body"`
	// Scope is "shared" or "agent:<name>". A private skill is available only
	// to its owner, and never included in defaults.
	Scope string `json:"scope,omitempty"`
}

// Artifact is one read-only input made available to the session.
type Artifact struct {
	Key         string `json:"key"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"content_type"`
}

// MCPServer describes one MCP server the session may use. The runtime starts a
// stdio server itself. An HTTP server carries no endpoint here: the gateway
// discovers and calls it through the broker, and its tools arrive in the tool
// grant.
type MCPServer struct {
	Name               string            `json:"name"`
	Transport          string            `json:"transport"` // stdio or http
	Command            string            `json:"command,omitempty"`
	Args               []string          `json:"args,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	InitTimeoutSeconds int               `json:"init_timeout_seconds,omitempty"`
	// Tools is the allowlist of a remote server. An empty list offers every
	// tool the server reports.
	Tools []string `json:"tools,omitempty"`
	// IdempotentTools names the tools that are safe to repeat after a
	// container restart. Every other tool counts as non-idempotent.
	IdempotentTools []string `json:"idempotent_tools,omitempty"`
}

// PluginMaxOutputBytes caps one plugin tool result, as agent.maxToolOutput
// caps a built-in tool result.
const PluginMaxOutputBytes = 64 << 10

// Plugin describes one plugin made available to the session. It is the
// runtime's copy of the manifest: paths, names, and limits, never a secret
// value. Service plugins carry only the fields a grant can be rebuilt from.
type Plugin struct {
	Name               string            `json:"name"`
	Placement          string            `json:"placement"`         // inline, sidecar, service
	Image              string            `json:"image,omitempty"`   // pod rebuilds; the runtime ignores it
	Dir                string            `json:"dir,omitempty"`     // inline
	Command            string            `json:"command,omitempty"` // inline, sidecar
	Args               []string          `json:"args,omitempty"`
	Workdir            string            `json:"workdir,omitempty"` // inline
	Socket             string            `json:"socket,omitempty"`  // sidecar
	Required           bool              `json:"required,omitempty"`
	Env                map[string]string `json:"env,omitempty"`         // inline, sidecar
	Secrets            []PluginSecret    `json:"secrets,omitempty"`     // inline, sidecar; names only
	SecretsDir         string            `json:"secrets_dir,omitempty"` // inline
	Tools              []PluginTool      `json:"tools"`
	InitTimeoutSeconds int               `json:"init_timeout_seconds"`
	MaxRestarts        int               `json:"max_restarts,omitempty"` // inline
	MaxOutputBytes     int               `json:"max_output_bytes"`
	StderrBytes        int64             `json:"stderr_bytes,omitempty"` // inline
	Sidecar            *PluginSidecar    `json:"sidecar,omitempty"`      // pod rebuilds
}

// PluginSidecar is the sidecar container's size and identity.
type PluginSidecar struct {
	Requests  map[string]string `json:"requests,omitempty"`
	Limits    map[string]string `json:"limits,omitempty"`
	RunAsUser int64             `json:"run_as_user"`
	Workspace string            `json:"workspace"`
}

// PluginSecret names the environment variable a plugin reads and where the
// control plane finds its value. Secret is empty on the local backend.
type PluginSecret struct {
	Env    string `json:"env"`
	Secret string `json:"secret,omitempty"`
	Key    string `json:"key,omitempty"`
}

// PluginTool is one tool the grant allows, by its manifest name.
type PluginTool struct {
	Name           string `json:"name"` // remote name
	Idempotent     bool   `json:"idempotent,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds"`
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
	// MaxLLMTokens and MaxLLMRequests are the resolved per-session model
	// budget. The control plane resolves the agent override before it writes
	// this document, so adoption recovers the same budget. Zero means no
	// limit.
	MaxLLMTokens       int    `json:"max_llm_tokens"`
	MaxLLMRequests     int    `json:"max_llm_requests"`
	TTLSeconds         int    `json:"ttl_seconds"`
	IdleTimeoutSeconds int    `json:"idle_timeout_seconds"`
	WorkspaceDir       string `json:"workspace_dir"`
	ArtifactsDir       string `json:"artifacts_dir"`
	AllowShell         bool   `json:"allow_shell"`
	ShellTimeoutSecs   int    `json:"shell_timeout_seconds"`
	ArtifactMaxBytes   int64  `json:"artifact_max_bytes"`
	// MaxImageBytes, MaxResultImageBytes, and MaxSessionImageBytes bound
	// image content from MCP tool results. Zero applies the content package
	// defaults, never an unlimited value.
	MaxImageBytes        int64 `json:"max_image_bytes,omitempty"`
	MaxResultImageBytes  int64 `json:"max_result_image_bytes,omitempty"`
	MaxSessionImageBytes int64 `json:"max_session_image_bytes,omitempty"`
	// ToolResultImages is nil or true when the assigned model accepts image
	// content parts. An explicit false makes the runtime replace each image
	// with a labeled text placeholder.
	ToolResultImages *bool       `json:"tool_result_images,omitempty"`
	Skills           []Skill     `json:"skills"`
	MCPServers       []MCPServer `json:"mcp_servers"`
	// Plugins are the tools the gateway grants this session. A service plugin
	// records its manifest so the grant can be rebuilt on adoption.
	Plugins []Plugin `json:"plugins,omitempty"`
	// Artifacts are read-only inputs the control plane stored before the
	// session started. Their keys are the only ones read_artifact accepts.
	Artifacts    []Artifact `json:"artifacts,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	AgentVersion string     `json:"agent_version,omitempty"`
	// IdempotencyKey is the caller's CreateOptions.IdempotencyKey. It is kept
	// with the session so an adopted session still answers a repeated Create.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Resumable reports that a replacement container will resume this session
	// from the journal and the same workspace. A runtime that is not resumable
	// finalizes on SIGTERM instead of leaving the turn to a replacement.
	Resumable bool `json:"resumable,omitempty"`
}

// ContentLimits returns the image bounds, with defaults for unset fields.
func (c Config) ContentLimits() content.Limits {
	return content.Limits{
		MaxImageBytes:        c.MaxImageBytes,
		MaxResultImageBytes:  c.MaxResultImageBytes,
		MaxSessionImageBytes: c.MaxSessionImageBytes,
	}.WithDefaults()
}

// ModelAcceptsImages reports whether the assigned model may receive image
// content parts. An unset field accepts them.
func (c Config) ModelAcceptsImages() bool {
	return c.ToolResultImages == nil || *c.ToolResultImages
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
	if c.MaxLLMTokens < 0 || c.MaxLLMRequests < 0 {
		return errors.New("max_llm_tokens and max_llm_requests must not be negative")
	}
	limits := c.ContentLimits()
	c.MaxImageBytes = limits.MaxImageBytes
	c.MaxResultImageBytes = limits.MaxResultImageBytes
	c.MaxSessionImageBytes = limits.MaxSessionImageBytes
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
			// The gateway holds the endpoint and credentials. The runtime
			// never dials an HTTP server, so its config carries no URL.
		default:
			return fmt.Errorf("mcp server %q: unsupported transport %q", s.Name, s.Transport)
		}
	}
	return c.validatePlugins()
}

// validatePlugins checks the plugin grants the runtime trusts. It repeats the
// manifest rules it depends on instead of importing the manifest package: the
// runtime must not carry the loader.
func (c *Config) validatePlugins() error {
	seen := make(map[string]bool, len(c.Plugins))
	for i := range c.Plugins {
		p := &c.Plugins[i]
		if p.Name == "" {
			return fmt.Errorf("plugin %d has an empty name", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate plugin %q", p.Name)
		}
		seen[p.Name] = true
		if err := p.validate(); err != nil {
			return fmt.Errorf("plugin %q: %w", p.Name, err)
		}
	}
	return nil
}

// validate checks one plugin entry against its placement.
func (p *Plugin) validate() error {
	switch p.Placement {
	case "inline":
		if p.Dir == "" {
			return errors.New("dir is required for an inline plugin")
		}
		if p.Command == "" {
			return errors.New("command is required for an inline plugin")
		}
		if p.Workdir == "" {
			return errors.New("workdir is required for an inline plugin")
		}
		if len(p.Secrets) > 0 && p.SecretsDir == "" {
			return errors.New("secrets_dir is required for an inline plugin with secrets")
		}
	case "sidecar":
		if p.Socket == "" {
			return errors.New("socket is required for a sidecar plugin")
		}
		if p.Command == "" {
			return errors.New("command is required for a sidecar plugin")
		}
		if p.Sidecar == nil {
			return errors.New("sidecar is required for a sidecar plugin")
		}
	case "service":
		// A service plugin carries only what the grant needs.
	default:
		return fmt.Errorf("unsupported placement %q", p.Placement)
	}
	if p.InitTimeoutSeconds <= 0 {
		return errors.New("init_timeout_seconds must be positive")
	}
	if p.MaxOutputBytes < 1 || p.MaxOutputBytes > PluginMaxOutputBytes {
		return fmt.Errorf("max_output_bytes must be in 1..%d", PluginMaxOutputBytes)
	}
	if len(p.Tools) == 0 {
		return errors.New("at least one tool is required")
	}
	tools := make(map[string]bool, len(p.Tools))
	for i, tool := range p.Tools {
		if tool.Name == "" {
			return fmt.Errorf("tool %d has an empty name", i)
		}
		if tools[tool.Name] {
			return fmt.Errorf("duplicate tool %q", tool.Name)
		}
		tools[tool.Name] = true
		if tool.TimeoutSeconds <= 0 {
			return fmt.Errorf("tool %q: timeout_seconds must be positive", tool.Name)
		}
	}
	secrets := make(map[string]bool, len(p.Secrets))
	for i, secret := range p.Secrets {
		if IsReservedEnv(secret.Env) {
			return fmt.Errorf("secret %d: %s is reserved for the control plane", i, secret.Env)
		}
		if !envNamePattern.MatchString(secret.Env) {
			return fmt.Errorf("secret %d: invalid environment variable name %q", i, secret.Env)
		}
		if secrets[secret.Env] {
			return fmt.Errorf("duplicate secret %q", secret.Env)
		}
		secrets[secret.Env] = true
	}
	return nil
}
