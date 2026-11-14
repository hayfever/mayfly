package prompt

import (
	"strings"
	"testing"
)

func TestEnvPolicy(t *testing.T) {
	t.Setenv("ALLOWED", "yes")
	t.Setenv("MAYFLY_API_TOKEN", "admin")
	t.Setenv("OPENAI_API_KEY", "llm")
	t.Setenv("UNDECLARED", "no")
	policy := EnvPolicy{Allow: []string{"ALLOWED", "OPENAI_API_KEY", "MAYFLY_API_TOKEN"}, Deny: []string{"OPENAI_API_KEY"}}

	tmpl, err := Parse("ok", `{{ env "ALLOWED" }}`, policy)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := tmpl.Render(nil); err != nil || got != "yes" {
		t.Fatalf("render = %q, %v", got, err)
	}
	for raw, want := range map[string]string{
		`{{ env "UNDECLARED" }}`:                          "not declared",
		`{{ env "MAYFLY_API_TOKEN" }}`:                    "reserved",
		`{{ env "OPENAI_API_KEY" }}`:                      "control-plane secret",
		`{{ env .Name }}`:                                 "string literal",
		`{{ printf "%s" (env "UNDECLARED") }}`:            "not declared",
		`{{ define "x" }}{{ env "UNDECLARED" }}{{ end }}`: "not declared",
	} {
		if _, err := Parse("bad", raw, policy); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want %q", raw, err, want)
		}
	}
	if _, err := Parse("none", `{{ env "ALLOWED" }}`, EnvPolicy{}); err == nil {
		t.Fatal("a template without declarations read the environment")
	}
}
