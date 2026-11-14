package agents

import (
	"strings"
	"testing"
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
