package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/content"
)

// FuzzDecodeSSE feeds arbitrary bytes to the Streamable HTTP event-stream
// decoder. It asserts that a returned message matches the requested id, is
// valid JSON, and stays within maxResponseBytes. When the message is a
// tools/call result, its content goes through the parser with small image
// limits, so the transport cannot carry an accepted oversize image.
func FuzzDecodeSSE(f *testing.F) {
	const id = 7
	tiny := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	large := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 400)))
	frame := func(payload string) string {
		return "event: message\ndata: " + payload + "\n\n"
	}
	seeds := []string{
		``,
		frame(`{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`),
		frame(`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"image","mimeType":"image/png","data":"` + tiny + `"}]}}`),
		frame(`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"image","mimeType":"image/png","data":"` + large + `"}]}}`),
		frame(`{"jsonrpc":"2.0","id":8,"result":{"ok":true}}`) + frame(`{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`),
		"data: {\"jsonrpc\":\"2.0\",\ndata: \"id\":7,\"result\":{}}\n\n",
		"data: {\"jsonrpc\":\"2.0\",\"id\":7\n\n",
		`data: {"jsonrpc":"2.0","id":7,"result":{"ok":true}}` + "\n",
		": comment\n\n",
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	limits := content.Limits{MaxImageBytes: 64, MaxResultImageBytes: 96, MaxSessionImageBytes: 128}
	f.Fuzz(func(t *testing.T, raw []byte) {
		out, err := decodeSSE(bytes.NewReader(raw), id)
		if err != nil {
			return
		}
		if len(out) > maxResponseBytes {
			t.Fatalf("decoded message is %d bytes, over %d", len(out), maxResponseBytes)
		}
		if !json.Valid(out) {
			t.Fatalf("decoded message is not valid JSON: %q", out)
		}
		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(out, &probe); err != nil || !matchesID(probe.ID, id) {
			t.Fatalf("decoded message does not match id %d: %q", id, out)
		}
		var response struct {
			Result struct {
				Content []json.RawMessage `json:"content"`
			} `json:"result"`
		}
		if json.Unmarshal(out, &response) != nil || len(response.Result.Content) == 0 {
			return
		}
		res := parseContent(response.Result.Content, limits)
		summary := res.Text()
		var total int64
		for _, part := range res.Parts {
			if !part.IsImage() {
				continue
			}
			if len(part.Data) == 0 || int64(len(part.Data)) > limits.MaxImageBytes {
				t.Fatalf("accepted image has %d bytes", len(part.Data))
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
	})
}
