package sources

import (
	"fmt"
	"strconv"
	"strings"
)

// pathSegment is one component of a dotted document path.
type pathSegment struct {
	key      string
	index    int
	hasIndex bool
	wildcard bool
}

// parsePath accepts "a.b[0].c" and "a.items[*].url". It does not accept
// bracket-quoted keys; a property name that contains a dot needs a source
// that returns flat fields.
func parsePath(raw string) ([]pathSegment, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("path is empty")
	}
	parts := strings.Split(raw, ".")
	segments := make([]pathSegment, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("path %q has an empty segment", raw)
		}
		segment := pathSegment{key: part}
		if open := strings.IndexByte(part, '['); open >= 0 {
			if !strings.HasSuffix(part, "]") {
				return nil, fmt.Errorf("path %q has an unterminated index", raw)
			}
			segment.key = part[:open]
			inner := part[open+1 : len(part)-1]
			if inner == "*" {
				segment.wildcard = true
			} else {
				index, err := strconv.Atoi(inner)
				if err != nil || index < 0 {
					return nil, fmt.Errorf("path %q has an invalid index %q", raw, inner)
				}
				segment.index, segment.hasIndex = index, true
			}
		}
		if segment.key == "" {
			return nil, fmt.Errorf("path %q has an empty key", raw)
		}
		segments = append(segments, segment)
	}
	return segments, nil
}

// lookupSegments resolves one path against decoded JSON.
func lookupSegments(value any, segments []pathSegment) (any, bool) {
	if len(segments) == 0 {
		return value, true
	}
	segment := segments[0]
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	child, ok := lookupKey(object, segment.key)
	if !ok {
		return nil, false
	}
	if segment.wildcard {
		items, ok := child.([]any)
		if !ok {
			return nil, false
		}
		out := make([]any, 0, len(items))
		for _, item := range items {
			if got, ok := lookupSegments(item, segments[1:]); ok {
				out = append(out, got)
			}
		}
		return out, len(out) > 0
	}
	if segment.hasIndex {
		items, ok := child.([]any)
		if !ok || segment.index >= len(items) {
			return nil, false
		}
		child = items[segment.index]
	}
	return lookupSegments(child, segments[1:])
}

// lookupKey matches the exact key first, then the single key whose
// case, spaces, underscores, and hyphens are ignored.
func lookupKey(object map[string]any, key string) (any, bool) {
	if value, ok := object[key]; ok {
		return value, true
	}
	want := normalizeKey(key)
	var found any
	matches := 0
	for name, value := range object {
		if normalizeKey(name) == want {
			found, matches = value, matches+1
		}
	}
	if matches == 1 {
		return found, true
	}
	return nil, false
}

func normalizeKey(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range strings.ToLower(name) {
		switch r {
		case ' ', '_', '-':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Text renders a decoded JSON value as plain text.
func Text(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, Text(item))
		}
		return strings.Join(parts, "")
	case map[string]any:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// compileField parses a field's paths.
type compiledField struct {
	name  string
	paths [][]pathSegment
	join  string
}

func compileField(name string, field Field, where string) (compiledField, error) {
	raw := field.Paths
	if field.Path != "" {
		raw = append([]string{field.Path}, raw...)
	}
	if len(raw) == 0 {
		return compiledField{}, fmt.Errorf("%s: field %q needs path or paths", where, name)
	}
	out := compiledField{name: name, join: field.Join}
	for _, one := range raw {
		segs, err := parsePath(one)
		if err != nil {
			return compiledField{}, fmt.Errorf("%s: field %q: %w", where, name, err)
		}
		out.paths = append(out.paths, segs)
	}
	return out, nil
}

// value returns the first path that resolves. Arrays become text joined with
// the field's separator.
func (f compiledField) value(document any) (any, bool) {
	for _, segments := range f.paths {
		value, ok := lookupSegments(document, segments)
		if !ok {
			continue
		}
		if items, ok := value.([]any); ok {
			parts := make([]string, 0, len(items))
			for _, item := range items {
				parts = append(parts, Text(item))
			}
			return strings.Join(parts, f.join), true
		}
		return value, true
	}
	return nil, false
}
