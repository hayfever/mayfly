// Package agents loads agent plugin manifests. An agent plugin is declarative:
// it names an agent, its prompt, model, skills, MCP servers, limits, and an
// optional runtime image. The control plane never executes plugin code.
package agents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// ProtocolVersion is the session runtime protocol an image must implement.
const ProtocolVersion = 1

var (
	namePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Runtime configures the harness that runs the agent.
type Runtime struct {
	Image    string            `yaml:"image" json:"image"`
	Protocol int               `yaml:"protocol" json:"protocol"`
	Command  []string          `yaml:"command" json:"command"`
	Args     []string          `yaml:"args" json:"args"`
	Env      map[string]string `yaml:"env" json:"env"`
	Config   map[string]any    `yaml:"config" json:"config"`
}

// Definition is one selectable agent.
type Definition struct {
	Name             string    `yaml:"name" json:"name"`
	Description      string    `yaml:"description" json:"description"`
	Harness          string    `yaml:"harness" json:"harness"`
	Model            string    `yaml:"model" json:"model"`
	SystemPrompt     string    `yaml:"systemPrompt" json:"systemPrompt"`
	SystemPromptFile string    `yaml:"systemPromptFile" json:"systemPromptFile"`
	Skills           *[]string `yaml:"skills" json:"skills"`
	MCPServers       *[]string `yaml:"mcpServers" json:"mcpServers"`
	// Plugins names the plugin manifests the agent may call. Nil and an empty
	// list both select none.
	Plugins            *[]string  `yaml:"plugins" json:"plugins"`
	AllowShell         *bool      `yaml:"allowShell" json:"allowShell"`
	MaxSteps           int        `yaml:"maxSteps" json:"maxSteps"`
	MaxHistoryMessages int        `yaml:"maxHistoryMessages" json:"maxHistoryMessages"`
	Runtime            Runtime    `yaml:"runtime" json:"runtime"`
	Schedules          []Schedule `yaml:"schedules" json:"schedules"`
	Default            bool       `yaml:"default" json:"default"`
}

// Schedule runs one source on a cron expression.
type Schedule struct {
	Name      string   `yaml:"name" json:"name"`
	Cron      string   `yaml:"cron" json:"cron"`
	Source    string   `yaml:"source" json:"source"`
	TTL       string   `yaml:"ttl" json:"ttl"`
	TriggerOn []string `yaml:"triggerOn" json:"triggerOn"`
	Context   string   `yaml:"context" json:"context"`
	Title     string   `yaml:"title" json:"title"`
	Prompt    string   `yaml:"prompt" json:"prompt"`
	// RequiredEnv and OptionalEnv are the only variables the context,
	// title, and prompt templates may read with env. A required variable
	// must be set at startup; an optional one renders empty when unset.
	RequiredEnv []string `yaml:"requiredEnv" json:"requiredEnv,omitempty"`
	OptionalEnv []string `yaml:"optionalEnv" json:"optionalEnv,omitempty"`
}

// Agent is a loaded definition with its prompt template and source.
type Agent struct {
	Definition
	Source     string
	PromptText string

	prompt *template.Template
}

// RenderPrompt executes the agent prompt template with data.
func (a Agent) RenderPrompt(data any) (string, error) {
	if a.prompt == nil {
		return "", nil
	}
	var out bytes.Buffer
	if err := a.prompt.Execute(&out, data); err != nil {
		return "", fmt.Errorf("agent %q prompt: %w", a.Name, err)
	}
	return strings.TrimSpace(out.String()), nil
}

// PluginNames returns the declared plugin names, or nil when the agent selects
// none. The result is a copy, so a caller cannot change the definition.
func (d Definition) PluginNames() []string {
	if d.Plugins == nil {
		return nil
	}
	return append([]string(nil), (*d.Plugins)...)
}

// Build validates one definition and parses its prompt. The project loader
// resolves systemPromptFile before it calls Build.
func Build(def Definition, source string) (Agent, error) {
	path := source
	if !namePattern.MatchString(def.Name) {
		return Agent{}, fmt.Errorf("%s: invalid agent name %q (use lowercase letters, digits, dot, underscore, or hyphen)", path, def.Name)
	}
	if def.Harness != "" && !namePattern.MatchString(def.Harness) {
		return Agent{}, fmt.Errorf("%s: agent %q has invalid harness %q", path, def.Name, def.Harness)
	}
	if def.Plugins != nil {
		seen := make(map[string]bool, len(*def.Plugins))
		for _, name := range *def.Plugins {
			if !namePattern.MatchString(name) {
				return Agent{}, fmt.Errorf("%s: agent %q has invalid plugin name %q", path, def.Name, name)
			}
			if seen[name] {
				return Agent{}, fmt.Errorf("%s: agent %q lists plugin %q twice", path, def.Name, name)
			}
			seen[name] = true
		}
	}
	if strings.TrimSpace(def.SystemPrompt) != "" && strings.TrimSpace(def.SystemPromptFile) != "" {
		return Agent{}, fmt.Errorf("%s: agent %q sets both systemPrompt and systemPromptFile", path, def.Name)
	}
	if strings.TrimSpace(def.SystemPromptFile) != "" {
		return Agent{}, fmt.Errorf("%s: agent %q: systemPromptFile must be resolved by the project loader", path, def.Name)
	}
	if def.MaxSteps < 0 || def.MaxHistoryMessages < 0 {
		return Agent{}, fmt.Errorf("%s: agent %q has a negative limit", path, def.Name)
	}
	if def.Runtime.Protocol == 0 {
		def.Runtime.Protocol = ProtocolVersion
	}
	if def.Runtime.Protocol != ProtocolVersion {
		return Agent{}, fmt.Errorf("%s: agent %q uses unsupported protocol %d", path, def.Name, def.Runtime.Protocol)
	}
	for key := range def.Runtime.Env {
		if !envNamePattern.MatchString(key) {
			return Agent{}, fmt.Errorf("%s: agent %q has invalid environment variable %q", path, def.Name, key)
		}
		switch key {
		case "MAYFLY_SESSION_TOKEN", "MAYFLY_CONFIG":
			return Agent{}, fmt.Errorf("%s: agent %q must not override %s", path, def.Name, key)
		}
	}
	if len(def.Runtime.Config) > 0 {
		if _, err := json.Marshal(def.Runtime.Config); err != nil {
			return Agent{}, fmt.Errorf("%s: agent %q runtime config is not JSON serializable: %w", path, def.Name, err)
		}
	}

	prompt := strings.TrimSpace(def.SystemPrompt)
	var tmpl *template.Template
	if prompt != "" {
		parsed, err := template.New("agent:" + def.Name).Option("missingkey=zero").Parse(prompt)
		if err != nil {
			return Agent{}, fmt.Errorf("%s: agent %q prompt template: %w", path, def.Name, err)
		}
		tmpl = parsed
	}
	return Agent{
		Definition: def,
		Source:     path,
		PromptText: prompt,
		prompt:     tmpl,
	}, nil
}
