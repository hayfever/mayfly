package sources

import (
	"math"
	"strings"
)

// match reports whether document fields satisfy every configured clause.
func (f *Filter) match(fields map[string]any) bool {
	if f == nil {
		return true
	}
	for key, want := range f.Equals {
		got, ok := fields[key]
		if !ok || !equalValue(got, want) {
			return false
		}
	}
	for key, want := range f.NotEquals {
		got, ok := fields[key]
		if ok && equalValue(got, want) {
			return false
		}
	}
	for _, key := range f.NotTrue {
		if value, ok := fields[key]; ok {
			if flag, isBool := value.(bool); isBool && flag {
				return false
			}
		}
	}
	for _, key := range f.NotEmpty {
		if !present(fields[key]) {
			return false
		}
	}
	if len(f.PresentAny) > 0 {
		found := false
		for _, key := range f.PresentAny {
			if present(fields[key]) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func present(value any) bool {
	return strings.TrimSpace(Text(value)) != ""
}

func equalValue(got, want any) bool {
	switch w := want.(type) {
	case nil:
		return got == nil
	case bool:
		g, ok := got.(bool)
		return ok && g == w
	case float64:
		g, ok := asNumber(got)
		return ok && g == w
	case int:
		g, ok := asNumber(got)
		return ok && g == float64(w)
	case string:
		return Text(got) == w
	default:
		return Text(got) == Text(want)
	}
}

func asNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return math.NaN(), false
	}
}
