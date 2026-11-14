package session

import (
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/hayfever/mayfly/internal/agents"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/project"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// AgentInfo is the public description of a selectable agent.
type AgentInfo struct {
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	Harness            string   `json:"harness"`
	Source             string   `json:"source"`
	Model              string   `json:"model"`
	Image              string   `json:"image"`
	Skills             []string `json:"skills"`
	MCPServers         []string `json:"mcp_servers"`
	AllowShell         bool     `json:"allow_shell"`
	MaxSteps           int      `json:"max_steps"`
	MaxHistoryMessages int      `json:"max_history_messages"`
	Protocol           int      `json:"protocol"`
	Command            []string `json:"command,omitempty"`
	Args               []string `json:"args,omitempty"`
	EnvKeys            []string `json:"env_keys,omitempty"`
	HasConfig          bool     `json:"has_config"`
	Default            bool     `json:"default"`
	Builtin            bool     `json:"builtin"`
}

func (m *Manager) loadAgents() error {
	builtin := agents.Agent{
		Definition: agents.Definition{
			Name:        "default",
			Description: "Built-in Mayfly agent",
			Runtime:     agents.Runtime{Protocol: agents.ProtocolVersion},
		},
		Source: "builtin",
	}
	m.agents = []agents.Agent{builtin}
	m.agentByName = map[string]agents.Agent{"default": builtin}
	m.defaultAgent = "default"

	if m.bundle == nil {
		return nil
	}
	for _, agent := range m.bundle.Agents {
		if agent.Name == "default" {
			return fmt.Errorf("%s: agent name %q is reserved for the built-in agent", agent.Source, agent.Name)
		}
		if agent.Model != "" && !m.cfg.LLM.ModelAllowed(agent.Model) {
			return fmt.Errorf("%s: agent %q uses model %q, which is not allowed", agent.Source, agent.Name, agent.Model)
		}
		if agent.Skills != nil {
			for _, name := range *agent.Skills {
				skill, ok := m.skillByName[name]
				if !ok {
					return fmt.Errorf("%s: agent %q references unknown skill %q", agent.Source, agent.Name, name)
				}
				if !scopeVisible(skill.Scope, agent.Name) {
					return fmt.Errorf("%s: agent %q references skill %q from another scope", agent.Source, agent.Name, name)
				}
			}
		}
		if agent.MCPServers != nil {
			for _, name := range *agent.MCPServers {
				server, ok := m.mcpByName[name]
				if !ok {
					return fmt.Errorf("%s: agent %q references unknown MCP server %q", agent.Source, agent.Name, name)
				}
				if !scopeVisible(server.Scope, agent.Name) {
					return fmt.Errorf("%s: agent %q references MCP server %q from another scope", agent.Source, agent.Name, name)
				}
			}
		}
		if agent.Runtime.Image != "" && !imageAllowed(agent.Runtime.Image, m.cfg.Agents.AllowedImages) {
			return fmt.Errorf("%s: agent %q image %q is not in agents.allowed_images", agent.Source, agent.Name, agent.Runtime.Image)
		}
		for name := range agent.Runtime.Env {
			if sessioncfg.IsReservedEnv(name) {
				return fmt.Errorf("%s: agent %q runtime.env must not set %s; the control plane injects it into every session", agent.Source, agent.Name, name)
			}
		}
		m.agentByName[agent.Name] = agent
		m.agents = append(m.agents, agent)
	}
	if m.bundle.Default != "" {
		m.defaultAgent = m.bundle.Default
	}
	for _, skill := range m.skills {
		if strings.HasPrefix(skill.Scope, "agent:") {
			owner := strings.TrimPrefix(skill.Scope, "agent:")
			if _, ok := m.agentByName[owner]; !ok {
				return fmt.Errorf("skill %q is private to unknown agent %q", skill.Name, owner)
			}
		}
	}
	for _, name := range m.cfg.Skills.Default {
		skill := m.skillByName[name]
		if strings.HasPrefix(skill.Scope, "agent:") {
			return fmt.Errorf("skills.default names private skill %q", name)
		}
	}
	return nil
}

// scopeVisible reports whether an agent may use a definition with this scope.
func scopeVisible(scope, agent string) bool {
	return scope == "" || scope == "shared" || scope == "agent:"+agent
}

// Agents returns every selectable agent with effective defaults.
func (m *Manager) Agents() []AgentInfo {
	out := make([]AgentInfo, 0, len(m.agents))
	for _, agent := range m.agents {
		out = append(out, m.agentInfo(agent))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Builtin != out[j].Builtin {
			return out[i].Builtin
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (m *Manager) agentInfo(agent agents.Agent) AgentInfo {
	modelName := agent.Model
	if modelName == "" {
		modelName = m.cfg.LLM.Model
	}
	skillNames := m.defaultSkill
	if agent.Skills != nil {
		skillNames = *agent.Skills
	}
	mcpNames := m.defaultMCP
	if agent.MCPServers != nil {
		mcpNames = *agent.MCPServers
	}
	image := agent.Runtime.Image
	if image == "" {
		image = m.cfg.Runtime.Image
	}
	maxSteps := agent.MaxSteps
	if maxSteps <= 0 {
		maxSteps = m.cfg.Sessions.MaxSteps
	}
	maxHistory := agent.MaxHistoryMessages
	if maxHistory <= 0 {
		maxHistory = m.cfg.Sessions.MaxHistoryMessages
	}
	return AgentInfo{
		Name:               agent.Name,
		Description:        agent.Description,
		Harness:            harnessName(agent),
		Source:             agent.Source,
		Model:              modelName,
		Image:              image,
		Skills:             append([]string(nil), skillNames...),
		MCPServers:         append([]string(nil), mcpNames...),
		AllowShell:         agentAllowShell(agent, m.cfg),
		MaxSteps:           maxSteps,
		MaxHistoryMessages: maxHistory,
		Protocol:           agent.Runtime.Protocol,
		Command:            append([]string(nil), agent.Runtime.Command...),
		Args:               append([]string(nil), agent.Runtime.Args...),
		EnvKeys:            envKeys(agent.Runtime.Env),
		HasConfig:          len(agent.Runtime.Config) > 0,
		Default:            agent.Name == m.defaultAgent,
		Builtin:            agent.Name == "default",
	}
}

func (m *Manager) resolveAgent(name string) (agents.Agent, error) {
	if name == "" {
		name = m.defaultAgent
	}
	agent, ok := m.agentByName[name]
	if !ok {
		return agents.Agent{}, fmt.Errorf("unknown agent %q", name)
	}
	return agent, nil
}

func agentAllowShell(agent agents.Agent, cfg *config.Config) bool {
	if agent.AllowShell != nil {
		return *agent.AllowShell
	}
	return config.Bool(cfg.Sessions.AllowShell, false)
}

func harnessName(agent agents.Agent) string {
	if agent.Harness == "" {
		return "mayfly"
	}
	return agent.Harness
}

func envKeys(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for key := range env {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func mergeEnv(base, override map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(override))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range override {
		out[key] = value
	}
	return out
}

func imageAllowed(image string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if ok, err := path.Match(pattern, image); err == nil && ok {
			return true
		}
	}
	return false
}

type promptData struct {
	SessionID string
	Title     string
	Agent     string
	Harness   string
	Model     string
	Trigger   model.TriggerInfo
	Now       time.Time
}

// Validate runs the startup checks New runs on the configuration and bundle,
// without starting anything: skills load and resolve, every agent's model is
// allowed, and every skill, MCP server, image, and runtime.env it names
// exists and is visible to it. mayflyd -check calls it so a bundle that would
// stop the control plane fails the check too.
func Validate(cfg *config.Config, bundle *project.Bundle) error {
	_, err := New(cfg, bundle, nil, hub.New(1), journal.NewMemory(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return err
	}
	if bundle == nil {
		return nil
	}
	for _, schedule := range bundle.Schedules {
		if _, ok := bundle.Agent(schedule.Agent); !ok {
			return fmt.Errorf("schedule %q: agent %q is not defined", schedule.Name, schedule.Agent)
		}
	}
	return nil
}
