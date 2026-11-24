package gateway

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/content"
)

func TestGatewayStoresAndReplaysImageParts(t *testing.T) {
	ctx := context.Background()
	g, _ := newTestGateway(t, Options{}, inlineTool(getTool, "alpha", "get", true))
	resp, err := begin(g, getTool, 1, 1, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	image := content.ImagePart("image/png", []byte("png-bytes"))
	if err := g.Complete(ctx, "s1", CompleteRequest{
		Key:     resp.Dispatch.Key,
		Content: "caption",
		Parts:   []content.Part{content.TextPart("caption"), image},
	}); err != nil {
		t.Fatal(err)
	}

	again, err := begin(g, getTool, 1, 1, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if again.Result == nil || !again.Result.Replayed {
		t.Fatalf("replayed response = %+v", again)
	}
	if len(again.Result.Parts) != 2 || !again.Result.Parts[1].IsImage() ||
		string(again.Result.Parts[1].Data) != "png-bytes" {
		t.Fatalf("replayed parts = %+v", again.Result.Parts)
	}
	if again.Result.Content != "caption\n"+content.ImageSummary(image) {
		t.Fatalf("content = %q", again.Result.Content)
	}
}

func TestGatewaySessionImageCap(t *testing.T) {
	ctx := context.Background()
	limits := content.Limits{MaxImageBytes: 64, MaxResultImageBytes: 64, MaxSessionImageBytes: 64}
	g, _ := newTestGateway(t, Options{ContentLimits: limits}, inlineTool(getTool, "alpha", "get", true))
	image := content.ImagePart("image/png", []byte(strings.Repeat("x", 40)))
	for _, tc := range []struct {
		step      int
		wantError bool
	}{{1, false}, {2, true}} {
		step, wantError := tc.step, tc.wantError
		resp, err := begin(g, getTool, 1, step, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if err := g.Complete(ctx, "s1", CompleteRequest{
			Key:     resp.Dispatch.Key,
			Content: "caption",
			Parts:   []content.Part{image},
		}); err != nil {
			t.Fatal(err)
		}
		replayed, err := begin(g, getTool, 1, step, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if replayed.Result.IsError != wantError {
			t.Fatalf("step %d error = %v, want %v", step, replayed.Result.IsError, wantError)
		}
		if wantError {
			if len(replayed.Result.Parts) != 0 {
				t.Fatalf("step %d kept parts: %+v", step, replayed.Result.Parts)
			}
			if !strings.Contains(replayed.Result.Content, "session image limit") {
				t.Fatalf("step %d content = %q", step, replayed.Result.Content)
			}
		}
	}
}

func TestGatewayAuditHasNoImageBytes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := NewAudit(path, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer audit.Close()
	g, _ := newTestGateway(t, Options{Audit: audit}, inlineTool(getTool, "alpha", "get", true))
	resp, err := begin(g, getTool, 1, 1, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("audit-png-payload")
	if err := g.Complete(ctx, "s1", CompleteRequest{
		Key:     resp.Dispatch.Key,
		Content: "caption",
		Parts:   []content.Part{content.ImagePart("image/png", data)},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), base64.StdEncoding.EncodeToString(data)) {
		t.Fatalf("audit contains image base64: %s", raw)
	}
	if strings.Contains(string(raw), string(data)) {
		t.Fatalf("audit contains image bytes: %s", raw)
	}
}
