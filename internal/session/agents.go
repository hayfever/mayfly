package session

import (
	"fmt"
	"path"
	"sort"
	"time"

	"github.com/hayfever/mayfly/internal/agents"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/model"
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

	loaded, err := agents.LoadDirs(m.cfg.Agents.Dirs)
	if err != nil {
		return err
	}
	for _, agent := range loaded {
		if agent.Name == "default" {
			return fmt.Errorf("%s: agent name %q is reserved for the built-in agent", agent.Source, agent.Name)
		}
		if agent.Model != "" && !m.cfg.LLM.ModelAllowed(agent.Model) {
			return fmt.Errorf("%s: agent %q uses model %q, which is not allowed", agent.Source, agent.Name, agent.Model)
		}
		for _, name := range agent.Skills {
			if _, ok := m.skillByName[name]; !ok {
				return fmt.Errorf("%s: agent %q references unknown skill %q", agent.Source, agent.Name, name)
			}
		}
		for _, name := range agent.MCPServers {
			if _, ok := m.mcpByName[name]; !ok {
				return fmt.Errorf("%s: agent %q references unknown MCP server %q", agent.Source, agent.Name, name)
			}
		}
		if agent.Runtime.Image != "" && !imageAllowed(agent.Runtime.Image, m.cfg.Agents.AllowedImages) {
			return fmt.Errorf("%s: agent %q image %q is not in agents.allowed_images", agent.Source, agent.Name, agent.Runtime.Image)
		}
		if agent.Default {
			m.defaultAgent = agent.Name
		}
		m.agentByName[agent.Name] = agent
		m.agents = append(m.agents, agent)
	}
	return nil
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
	skillNames := agent.Skills
	if len(skillNames) == 0 {
		skillNames = m.defaultSkill
	}
	mcpNames := agent.MCPServers
	if len(mcpNames) == 0 {
		mcpNames = m.defaultMCP
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
	return config.Bool(cfg.Sessions.AllowShell, true)
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
