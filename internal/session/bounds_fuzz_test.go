package session

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hayfever/mayfly/internal/model"
)

// FuzzEventBound checks the rune-safe event truncation. A shortened value
// stays within the cap, keeps a prefix of the input, and stays valid UTF-8
// whenever the input was valid. The seeds sit on the cap and on a multibyte
// rune that straddles it.
func FuzzEventBound(f *testing.F) {
	f.Add("", int16(64))
	f.Add(strings.Repeat("a", 64), int16(64))
	f.Add(strings.Repeat("a", 65), int16(64))
	f.Add(strings.Repeat("a", 63)+"é", int16(64))
	f.Add(strings.Repeat("a", 62)+"日", int16(64))
	f.Add("日本語", int16(4))
	f.Add(strings.Repeat("x", 300), int16(1))

	f.Fuzz(func(t *testing.T, s string, n int16) {
		limit := int(n)
		out := truncateEventString(s, limit)
		if limit > 0 && len(out) > limit {
			t.Fatalf("truncateEventString(%q, %d) returned %d bytes", s, limit, len(out))
		}
		if utf8.ValidString(s) && !utf8.ValidString(out) {
			t.Fatalf("valid UTF-8 input produced invalid UTF-8 %q", out)
		}
		if prefix := strings.TrimSuffix(out, eventTruncationMark); !strings.HasPrefix(s, prefix) {
			t.Fatalf("truncated value %q is not a prefix of the input", out)
		}

		ev := boundRuntimeEvent(model.Event{
			Text:    s,
			Actor:   s,
			StepKey: s,
			Data: model.SystemData{
				Model:      s,
				Skills:     []string{s},
				MCPServers: []string{s},
				Plugins:    []string{s},
			},
		}, limit)
		if limit > 0 {
			if len(ev.Text) > limit {
				t.Fatalf("event text is %d bytes, over %d", len(ev.Text), limit)
			}
			data, ok := model.DataAs[model.SystemData](ev.Data)
			if !ok {
				t.Fatalf("system data = %T", ev.Data)
			}
			for _, value := range []string{data.Model, data.Skills[0], data.MCPServers[0], data.Plugins[0]} {
				if len(value) > limit {
					t.Fatalf("payload string is %d bytes, over %d", len(value), limit)
				}
			}
		}
		if utf8.ValidString(s) && !utf8.ValidString(ev.Text) {
			t.Fatalf("valid event text became invalid: %q", ev.Text)
		}
	})
}
