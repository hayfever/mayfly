// Package agents loads agent plugin manifests. An agent plugin is declarative:
// it names an agent, its prompt, model, skills, MCP servers, limits, and an
// optional runtime image. The control plane never executes plugin code.
package agents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the manifest schema this package understands.
const SchemaVersion = 1

// ProtocolVersion is the session runtime protocol an image must implement.
const ProtocolVersion = 1

var (
	namePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Manifest is one agent plugin file. A manifest can declare several agents.
type Manifest struct {
	SchemaVersion int          `yaml:"schemaVersion" json:"schemaVersion"`
	Name          string       `yaml:"name" json:"name"`
	Version       string       `yaml:"version" json:"version"`
	Agents        []Definition `yaml:"agents" json:"agents"`
}

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
	Name               string   `yaml:"name" json:"name"`
	Description        string   `yaml:"description" json:"description"`
	Harness            string   `yaml:"harness" json:"harness"`
	Model              string   `yaml:"model" json:"model"`
	SystemPrompt       string   `yaml:"systemPrompt" json:"systemPrompt"`
	SystemPromptFile   string   `yaml:"systemPromptFile" json:"systemPromptFile"`
	Skills             []string `yaml:"skills" json:"skills"`
	MCPServers         []string `yaml:"mcpServers" json:"mcpServers"`
	AllowShell         *bool    `yaml:"allowShell" json:"allowShell"`
	MaxSteps           int      `yaml:"maxSteps" json:"maxSteps"`
	MaxHistoryMessages int      `yaml:"maxHistoryMessages" json:"maxHistoryMessages"`
	Runtime            Runtime  `yaml:"runtime" json:"runtime"`
	Default            bool     `yaml:"default" json:"default"`
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

// LoadDirs reads every YAML or JSON manifest under the given directories.
// Hidden directories (Kubernetes ConfigMap atomic-write directories) are
// skipped. Duplicate agent names are an error.
func LoadDirs(dirs []string) ([]Agent, error) {
	var out []Agent
	seen := map[string]string{}
	defaults := 0
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		info, err := os.Stat(dir)
		if err != nil {
			return nil, fmt.Errorf("agent directory %s: %w", dir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("agent path %s is not a directory", dir)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return nil, fmt.Errorf("agent directory %s: %w", dir, err)
		}
		walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path != dir && strings.HasPrefix(entry.Name(), ".") {
					return fs.SkipDir
				}
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".yaml", ".yml", ".json":
			default:
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			loaded, err := loadManifest(root, rel, path)
			if err != nil {
				return err
			}
			for _, agent := range loaded {
				if prev, ok := seen[agent.Name]; ok {
					return fmt.Errorf("duplicate agent name %q in %s and %s", agent.Name, prev, agent.Source)
				}
				if agent.Default {
					defaults++
					if defaults > 1 {
						return fmt.Errorf("more than one agent is marked as default; second is %q", agent.Name)
					}
				}
				seen[agent.Name] = agent.Source
				out = append(out, agent)
			}
			return nil
		})
		_ = root.Close()
		if walkErr != nil {
			return nil, walkErr
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func loadManifest(root *os.Root, rel, path string) ([]Agent, error) {
	raw, err := root.ReadFile(rel)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if manifest.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%s: unsupported schemaVersion %d", path, manifest.SchemaVersion)
	}
	if strings.TrimSpace(manifest.Name) == "" {
		return nil, fmt.Errorf("%s: manifest name is required", path)
	}
	if len(manifest.Agents) == 0 {
		return nil, fmt.Errorf("%s: no agents declared", path)
	}

	out := make([]Agent, 0, len(manifest.Agents))
	for _, def := range manifest.Agents {
		agent, err := buildAgent(path, rel, root, def)
		if err != nil {
			return nil, err
		}
		out = append(out, agent)
	}
	return out, nil
}

func buildAgent(path, manifestRel string, root *os.Root, def Definition) (Agent, error) {
	if !namePattern.MatchString(def.Name) {
		return Agent{}, fmt.Errorf("%s: invalid agent name %q (use lowercase letters, digits, dot, underscore, or hyphen)", path, def.Name)
	}
	if def.Harness != "" && !namePattern.MatchString(def.Harness) {
		return Agent{}, fmt.Errorf("%s: agent %q has invalid harness %q", path, def.Name, def.Harness)
	}
	if strings.TrimSpace(def.SystemPrompt) != "" && strings.TrimSpace(def.SystemPromptFile) != "" {
		return Agent{}, fmt.Errorf("%s: agent %q sets both systemPrompt and systemPromptFile", path, def.Name)
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
	if def.SystemPromptFile != "" {
		rel := filepath.Clean(def.SystemPromptFile)
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return Agent{}, fmt.Errorf("%s: agent %q prompt file must stay inside the manifest directory", path, def.Name)
		}
		promptRel := filepath.Join(filepath.Dir(manifestRel), rel)
		data, err := root.ReadFile(promptRel)
		if err != nil {
			return Agent{}, fmt.Errorf("%s: agent %q prompt file: %w", path, def.Name, err)
		}
		prompt = strings.TrimSpace(string(data))
	}
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
