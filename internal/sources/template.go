package sources

import (
	"fmt"

	"github.com/hayfever/mayfly/internal/prompt"
)

// textTemplate is a parsed Go template over the run data.
type textTemplate = prompt.Template

func parseTemplate(name, raw string, env prompt.EnvPolicy) (*textTemplate, error) {
	return prompt.Parse(name, raw, env)
}

// valueNode is a request body value with its strings parsed as templates.
type valueNode struct {
	text   *textTemplate
	items  []valueNode
	fields map[string]valueNode
	raw    any
}

func compileValue(where string, value any, env prompt.EnvPolicy) (valueNode, error) {
	switch v := value.(type) {
	case string:
		tmpl, err := parseTemplate(where, v, env)
		if err != nil {
			return valueNode{}, err
		}
		return valueNode{text: tmpl}, nil
	case map[string]any:
		out := valueNode{fields: make(map[string]valueNode, len(v))}
		for key, item := range v {
			child, err := compileValue(where+"."+key, item, env)
			if err != nil {
				return valueNode{}, err
			}
			out.fields[key] = child
		}
		return out, nil
	case []any:
		out := valueNode{items: make([]valueNode, 0, len(v))}
		for i, item := range v {
			child, err := compileValue(fmt.Sprintf("%s[%d]", where, i), item, env)
			if err != nil {
				return valueNode{}, err
			}
			out.items = append(out.items, child)
		}
		return out, nil
	default:
		return valueNode{raw: value}, nil
	}
}

func (n valueNode) render(data any) (any, error) {
	switch {
	case n.text != nil:
		return n.text.Render(data)
	case n.fields != nil:
		out := make(map[string]any, len(n.fields))
		for key, child := range n.fields {
			value, err := child.render(data)
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
		return out, nil
	case n.items != nil:
		out := make([]any, 0, len(n.items))
		for _, child := range n.items {
			value, err := child.render(data)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	default:
		return n.raw, nil
	}
}

// compileTemplates parses a string map such as query or headers.
func compileTemplates(where string, values map[string]string, env prompt.EnvPolicy) (map[string]*textTemplate, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]*textTemplate, len(values))
	for key, raw := range values {
		tmpl, err := parseTemplate(where+"."+key, raw, env)
		if err != nil {
			return nil, err
		}
		out[key] = tmpl
	}
	return out, nil
}
