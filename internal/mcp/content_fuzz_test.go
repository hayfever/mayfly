package mcp

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/content"
)

// FuzzToolResultContent feeds arbitrary MCP content arrays to the parser. It
// asserts the bounds the gateway and the runtime rely on: no panic, every
// accepted image is allowed and within the limits, and the text rendering
// never contains an accepted image's base64.
func FuzzToolResultContent(f *testing.F) {
	png := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	large := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 200)))
	seeds := []string{
		``,
		`[]`,
		`[{"type":"text","text":"hello"}]`,
		`[{"type":"image","mimeType":"image/png","data":"` + png + `"}]`,
		`[{"type":"text","text":"caption"},{"type":"image","mimeType":"image/jpeg","data":"` + png + `"}]`,
		`[{"type":"image","mimeType":"image/svg+xml","data":"` + png + `"}]`,
		`[{"type":"image","mimeType":"image/png","data":"!!!"}]`,
		`[{"type":"image","mimeType":"image/png","data":"` + large + `"}]`,
		`[{"type":"audio","mimeType":"audio/wav","data":"` + png + `"}]`,
		`[{"type":"resource","resource":{"uri":"file:///x"}}]`,
		`{`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	limits := content.Limits{MaxImageBytes: 64, MaxResultImageBytes: 96, MaxSessionImageBytes: 128}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return
		}
		res := parseContent(items, limits)
		var total int64
		summary := res.Text()
		if strings.Contains(summary, "base64,") {
			t.Fatalf("summary contains a data URI: %q", summary)
		}
		for _, part := range res.Parts {
			if !part.IsImage() {
				continue
			}
			if len(part.Data) == 0 || int64(len(part.Data)) > limits.MaxImageBytes {
				t.Fatalf("accepted image has %d bytes", len(part.Data))
			}
			if _, ok := content.NormalizeMIMEType(part.MIMEType); !ok {
				t.Fatalf("accepted image MIME %q", part.MIMEType)
			}
			total += int64(len(part.Data))
			encoded := base64.StdEncoding.EncodeToString(part.Data)
			if len(encoded) >= 24 && strings.Contains(summary, encoded) {
				t.Fatalf("summary contains image base64 for a %d-byte image", len(part.Data))
			}
		}
		if total > limits.MaxResultImageBytes {
			t.Fatalf("accepted images total %d bytes, over %d", total, limits.MaxResultImageBytes)
		}
		if again := res.Text(); again != summary {
			t.Fatalf("Text is not stable: %q then %q", summary, again)
		}
	})
}
