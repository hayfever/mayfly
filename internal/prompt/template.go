// Package prompt parses the Go templates used by agent prompts and schedules.
// A missing key is an error: a silently empty prompt field hides a broken
// schedule.
package prompt

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"
)

// ReservedEnvPrefix names the control plane's own environment. A template may
// never read a variable with this prefix, whatever a bundle declares.
const ReservedEnvPrefix = "MAYFLY_"

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvPolicy decides which environment variables the template function env may
// read. Allow lists the names the bundle declared for this template; Deny
// lists control-plane secrets (the LLM key, trigger secrets, MCP header
// variables, and so on) that stay unreadable even when declared.
type EnvPolicy struct {
	Allow []string
	Deny  []string
}

// Check reports why name may not be declared for template use, or nil.
func (p EnvPolicy) Check(name string) error {
	if !envNamePattern.MatchString(name) {
		return fmt.Errorf("invalid environment variable name %q", name)
	}
	if strings.HasPrefix(name, ReservedEnvPrefix) {
		return fmt.Errorf("environment variable %s is reserved for the control plane", name)
	}
	for _, denied := range p.Deny {
		if name == denied {
			return fmt.Errorf("environment variable %s holds a control-plane secret and cannot be read by a template", name)
		}
	}
	return nil
}

func (p EnvPolicy) allows(name string) error {
	if err := p.Check(name); err != nil {
		return err
	}
	for _, allowed := range p.Allow {
		if name == allowed {
			return nil
		}
	}
	if len(p.Allow) == 0 {
		return fmt.Errorf("environment variable %s is not declared; this template may not read the environment", name)
	}
	sorted := append([]string(nil), p.Allow...)
	sort.Strings(sorted)
	return fmt.Errorf("environment variable %s is not declared (declared: %s)", name, strings.Join(sorted, ", "))
}

// Template is a parsed template.
type Template struct {
	Raw string

	template *template.Template
}

// Parse compiles one template with the shared function set. Every env call
// must name a string literal that env allows, so an undeclared or reserved
// variable fails at load time instead of at render time.
func Parse(name, raw string, env EnvPolicy) (*Template, error) {
	parsed, err := template.New(name).
		Option("missingkey=error").
		Funcs(template.FuncMap{"env": func(key string) (string, error) {
			if err := env.allows(key); err != nil {
				return "", err
			}
			return os.Getenv(key), nil
		}}).
		Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	for _, tree := range parsed.Templates() {
		if tree.Tree == nil || tree.Root == nil {
			continue
		}
		if err := checkEnvCalls(tree.Root, env); err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
	}
	return &Template{Raw: raw, template: parsed}, nil
}

// checkEnvCalls walks the parse tree and validates every env call.
func checkEnvCalls(node parse.Node, env EnvPolicy) error {
	switch n := node.(type) {
	case nil:
		return nil
	case *parse.ListNode:
		if n == nil {
			return nil
		}
		for _, child := range n.Nodes {
			if err := checkEnvCalls(child, env); err != nil {
				return err
			}
		}
	case *parse.ActionNode:
		return checkEnvCalls(n.Pipe, env)
	case *parse.IfNode:
		return checkBranch(&n.BranchNode, env)
	case *parse.RangeNode:
		return checkBranch(&n.BranchNode, env)
	case *parse.WithNode:
		return checkBranch(&n.BranchNode, env)
	case *parse.TemplateNode:
		return checkEnvCalls(n.Pipe, env)
	case *parse.PipeNode:
		if n == nil {
			return nil
		}
		for _, cmd := range n.Cmds {
			if err := checkEnvCalls(cmd, env); err != nil {
				return err
			}
		}
	case *parse.ChainNode:
		return checkEnvCalls(n.Node, env)
	case *parse.CommandNode:
		for i, arg := range n.Args {
			if ident, ok := arg.(*parse.IdentifierNode); ok && ident.Ident == "env" {
				if i != 0 || len(n.Args) != 2 {
					return fmt.Errorf("env takes exactly one string literal argument")
				}
				literal, ok := n.Args[1].(*parse.StringNode)
				if !ok {
					return fmt.Errorf("env takes a string literal, not %s", n.Args[1])
				}
				if err := env.allows(literal.Text); err != nil {
					return err
				}
				continue
			}
			if err := checkEnvCalls(arg, env); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkBranch(n *parse.BranchNode, env EnvPolicy) error {
	if err := checkEnvCalls(n.Pipe, env); err != nil {
		return err
	}
	if err := checkEnvCalls(n.List, env); err != nil {
		return err
	}
	return checkEnvCalls(n.ElseList, env)
}

// Render executes the template with data.
func (t *Template) Render(data any) (string, error) {
	var out bytes.Buffer
	if err := t.template.Execute(&out, data); err != nil {
		return "", fmt.Errorf("template %q: %w", t.Raw, err)
	}
	return out.String(), nil
}
