package main

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, text string) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return doc
}

func TestCompare(t *testing.T) {
	const base = `{
		"type": "object",
		"required": ["version", "kind"],
		"properties": {
			"version": {"const": 1},
			"kind": {"type": "string", "enum": ["a", "b"]},
			"prompt": {"type": "string", "minLength": 1},
			"metadata": {"type": "object", "additionalProperties": true}
		},
		"additionalProperties": true
	}`

	tests := []struct {
		name     string
		old      string
		new      string
		breaking bool
	}{
		{
			name: "identical",
			old:  base,
			new:  base,
		},
		{
			name: "added optional top-level property",
			old:  base,
			new: `{
				"type": "object",
				"required": ["version", "kind"],
				"properties": {
					"version": {"const": 1},
					"kind": {"type": "string", "enum": ["a", "b"]},
					"prompt": {"type": "string", "minLength": 1},
					"metadata": {"type": "object", "additionalProperties": true},
					"trace_id": {"type": "string"}
				},
				"additionalProperties": true
			}`,
		},
		{
			name: "added optional nested property",
			old:  `{"type": "object", "properties": {"session": {"type": "object", "properties": {"title": {"type": "string"}}}}}`,
			new:  `{"type": "object", "properties": {"session": {"type": "object", "properties": {"title": {"type": "string"}, "labels": {"type": "object"}}}}}`,
		},
		{
			name: "changed description",
			old:  `{"type": "object", "description": "old", "properties": {"a": {"type": "string", "description": "old"}}}`,
			new:  `{"type": "object", "description": "new", "properties": {"a": {"type": "string", "description": "new"}}}`,
		},
		{
			name:     "removed property",
			old:      base,
			new:      `{"type": "object", "required": ["version", "kind"], "properties": {"version": {"const": 1}, "kind": {"type": "string", "enum": ["a", "b"]}}, "additionalProperties": true}`,
			breaking: true,
		},
		{
			name:     "renamed property",
			old:      base,
			new:      `{"type": "object", "required": ["version", "kind"], "properties": {"version": {"const": 1}, "kind": {"type": "string", "enum": ["a", "b"]}, "message": {"type": "string", "minLength": 1}, "metadata": {"type": "object", "additionalProperties": true}}, "additionalProperties": true}`,
			breaking: true,
		},
		{
			name:     "added required property",
			old:      base,
			new:      `{"type": "object", "required": ["version", "kind", "trace_id"], "properties": {"version": {"const": 1}, "kind": {"type": "string", "enum": ["a", "b"]}, "prompt": {"type": "string", "minLength": 1}, "metadata": {"type": "object", "additionalProperties": true}, "trace_id": {"type": "string"}}, "additionalProperties": true}`,
			breaking: true,
		},
		{
			name:     "removed required entry",
			old:      base,
			new:      `{"type": "object", "required": ["version"], "properties": {"version": {"const": 1}, "kind": {"type": "string", "enum": ["a", "b"]}, "prompt": {"type": "string", "minLength": 1}, "metadata": {"type": "object", "additionalProperties": true}}, "additionalProperties": true}`,
			breaking: true,
		},
		{
			name:     "changed type",
			old:      base,
			new:      `{"type": "object", "required": ["version", "kind"], "properties": {"version": {"const": 1}, "kind": {"type": "string", "enum": ["a", "b"]}, "prompt": {"type": "number", "minLength": 1}, "metadata": {"type": "object", "additionalProperties": true}}, "additionalProperties": true}`,
			breaking: true,
		},
		{
			name:     "added constraint to existing property",
			old:      base,
			new:      `{"type": "object", "required": ["version", "kind"], "properties": {"version": {"const": 1}, "kind": {"type": "string", "enum": ["a", "b"]}, "prompt": {"type": "string", "minLength": 2}, "metadata": {"type": "object", "additionalProperties": true}}, "additionalProperties": true}`,
			breaking: true,
		},
		{
			name:     "narrowed enum",
			old:      base,
			new:      `{"type": "object", "required": ["version", "kind"], "properties": {"version": {"const": 1}, "kind": {"type": "string", "enum": ["a"]}, "prompt": {"type": "string", "minLength": 1}, "metadata": {"type": "object", "additionalProperties": true}}, "additionalProperties": true}`,
			breaking: true,
		},
		{
			name:     "closed object",
			old:      base,
			new:      `{"type": "object", "required": ["version", "kind"], "properties": {"version": {"const": 1}, "kind": {"type": "string", "enum": ["a", "b"]}, "prompt": {"type": "string", "minLength": 1}, "metadata": {"type": "object", "additionalProperties": true}}, "additionalProperties": false}`,
			breaking: true,
		},
		{
			name:     "added required inside new definition",
			old:      `{"type": "object", "required": ["a"], "properties": {"a": {"type": "string"}}}`,
			new:      `{"type": "object", "required": ["a"], "properties": {"a": {"type": "string"}}, "allOf": [{"required": ["b"]}]}`,
			breaking: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			problems := compare(decode(t, test.old), decode(t, test.new))
			if got := len(problems) > 0; got != test.breaking {
				t.Fatalf("breaking = %v, want %v; problems: %v", got, test.breaking, problems)
			}
		})
	}
}
