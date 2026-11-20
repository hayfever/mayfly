package project

import (
	"fmt"
	"sort"
)

// merge applies an RFC 7386 JSON merge patch. Maps merge recursively, arrays
// and scalars replace, and a null value deletes the key.
func merge(base, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(patch))
	for key, value := range base {
		out[key] = cloneValue(value)
	}
	for key, value := range patch {
		if value == nil {
			delete(out, key)
			continue
		}
		patchMap, patchIsMap := value.(map[string]any)
		baseMap, baseIsMap := out[key].(map[string]any)
		if patchIsMap && baseIsMap {
			out[key] = merge(baseMap, patchMap)
			continue
		}
		out[key] = cloneValue(value)
	}
	return out
}

func cloneValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = cloneValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return value
	}
}

// rawName reads the string name of a raw definition.
func rawName(raw map[string]any) (string, bool) {
	value, ok := raw["name"]
	if !ok {
		return "", false
	}
	name, ok := value.(string)
	return name, ok
}

func rawString(raw map[string]any, key string) string {
	value, ok := raw[key]
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// resolveExtends returns the fully merged definition for name.
func resolveExtends(kind string, defs map[string]rawDefinition, name string, stack []string) (map[string]any, error) {
	record, ok := defs[name]
	if !ok {
		return nil, fmt.Errorf("%s %q is not defined", kind, name)
	}
	for _, seen := range stack {
		if seen == name {
			return nil, fmt.Errorf("%s %q has a cycle in extends: %v", kind, name, append(stack, name))
		}
	}
	parent := rawString(record.raw, "extends")
	base := map[string]any{}
	if parent != "" {
		if err := checkVisible(kind, name, record.scope, parent, defs); err != nil {
			return nil, err
		}
		merged, err := resolveExtends(kind, defs, parent, append(stack, name))
		if err != nil {
			return nil, err
		}
		base = merged
	}
	out := merge(base, record.raw)
	if out["name"] != name {
		return nil, fmt.Errorf("%s %q: extends must not change the name", kind, name)
	}
	out["scope"] = record.scope
	if patchType := rawString(record.raw, "type"); patchType != "" {
		if baseType := rawString(base, "type"); baseType != "" && baseType != patchType {
			return nil, fmt.Errorf("%s %q: extends must not change the type", kind, name)
		}
	}
	delete(out, "extends")
	return out, nil
}

// checkVisible rejects a definition that extends one it cannot see: a shared
// definition may extend only shared ones, and a private one only shared ones
// or its own agent's.
func checkVisible(kind, name, scope, parent string, defs map[string]rawDefinition) error {
	parentRecord, ok := defs[parent]
	if !ok {
		return fmt.Errorf("%s %q extends unknown %s %q", kind, name, kind, parent)
	}
	if scope == scopeShared && parentRecord.scope != scopeShared {
		return fmt.Errorf("%s %q is shared and cannot extend private %s %q", kind, name, kind, parent)
	}
	if scope != scopeShared && !visible(parentRecord.scope, scopeOwner(scope)) {
		return fmt.Errorf("%s %q (scope %s) cannot extend %s %q, which is private to %s", kind, name, scope, kind, parent, scopeOwner(parentRecord.scope))
	}
	return nil
}
