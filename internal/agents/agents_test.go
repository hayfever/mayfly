package agents

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBuildRendersPrompt(t *testing.T) {
	agent, err := Build(Definition{
		Name:         "triage",
		SystemPrompt: "You are {{.Agent}} for {{.SessionID}}.",
		MaxSteps:     40,
		Default:      true,
		Runtime:      Runtime{Protocol: 1},
	}, "agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Name != "triage" || !agent.Default || agent.MaxSteps != 40 {
		t.Fatalf("agent = %+v", agent)
	}
	prompt, err := agent.RenderPrompt(map[string]string{"Agent": "triage", "SessionID": "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "You are triage for s1." {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestBuildRejectsBadDefinitions(t *testing.T) {
	cases := map[string]Definition{
		"name":         {Name: "BadName"},
		"prompt file":  {Name: "ok", SystemPromptFile: "prompt.md"},
		"bad template": {Name: "ok", SystemPrompt: "hello {{"},
	}
	for name, def := range cases {
		if _, err := Build(def, "agent.yaml"); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

func TestBuildKeepsLLMBudgetOverrides(t *testing.T) {
	agent, err := Build(Definition{Name: "ok", MaxLLMTokens: 500, MaxLLMRequests: 3}, "agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if agent.MaxLLMTokens != 500 || agent.MaxLLMRequests != 3 {
		t.Fatalf("budget = %d / %d", agent.MaxLLMTokens, agent.MaxLLMRequests)
	}
	// The manifest uses camelCase keys in both formats, like maxSteps.
	var loaded Definition
	if err := yaml.Unmarshal([]byte("name: ok\nmaxLLMTokens: 700\nmaxLLMRequests: 7\n"), &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.MaxLLMTokens != 700 || loaded.MaxLLMRequests != 7 {
		t.Fatalf("yaml budget = %d / %d", loaded.MaxLLMTokens, loaded.MaxLLMRequests)
	}
	loaded = Definition{}
	if err := json.Unmarshal([]byte(`{"name":"ok","maxLLMTokens":800,"maxLLMRequests":8}`), &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.MaxLLMTokens != 800 || loaded.MaxLLMRequests != 8 {
		t.Fatalf("json budget = %d / %d", loaded.MaxLLMTokens, loaded.MaxLLMRequests)
	}
	for name, def := range map[string]Definition{
		"tokens":   {Name: "ok", MaxLLMTokens: -1},
		"requests": {Name: "ok", MaxLLMRequests: -1},
	} {
		if _, err := Build(def, "agent.yaml"); err == nil {
			t.Fatalf("%s: a negative budget was accepted", name)
		}
	}
}

func TestBuildKeepsNilAndEmptySkillListsDistinct(t *testing.T) {
	none := []string{}
	agent, err := Build(Definition{Name: "ok", Skills: &none}, "agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Skills == nil || len(*agent.Skills) != 0 {
		t.Fatalf("skills = %v, want an empty, non-nil list", agent.Skills)
	}
	inherited, err := Build(Definition{Name: "ok2"}, "agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if inherited.Skills != nil {
		t.Fatalf("skills = %v, want nil", inherited.Skills)
	}
}

func TestEmptyPromptRendersEmpty(t *testing.T) {
	agent, err := Build(Definition{Name: "ok"}, "agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out, err := agent.RenderPrompt(struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("prompt = %q", out)
	}
}

func TestBuildValidatesPlugins(t *testing.T) {
	invalid := []string{"notify", "Bad Name"}
	if _, err := Build(Definition{Name: "writer", Plugins: &invalid}, "agent.yaml"); err == nil || !strings.Contains(err.Error(), `invalid plugin name "Bad Name"`) {
		t.Fatalf("err = %v", err)
	}
	duplicate := []string{"notify", "notify"}
	if _, err := Build(Definition{Name: "writer", Plugins: &duplicate}, "agent.yaml"); err == nil || !strings.Contains(err.Error(), `lists plugin "notify" twice`) {
		t.Fatalf("err = %v", err)
	}
	none := []string{}
	agent, err := Build(Definition{Name: "writer", Plugins: &none}, "agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if names := agent.PluginNames(); len(names) != 0 {
		t.Fatalf("plugins = %v", names)
	}
	selected := []string{"notify", "crm"}
	agent, err = Build(Definition{Name: "writer", Plugins: &selected}, "agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if names := agent.PluginNames(); len(names) != 2 || names[1] != "crm" {
		t.Fatalf("plugins = %v", names)
	}
}
