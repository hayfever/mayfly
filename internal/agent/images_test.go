package agent

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/content"
	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/llm"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

func TestAdjustToolResultModelSupport(t *testing.T) {
	image := content.ImagePart("image/png", []byte("png-bytes"))
	res := toolResult{Parts: []content.Part{content.TextPart("caption"), image}}

	r := &Runtime{cfg: sessioncfg.Config{}}
	out, images, omitted := r.adjustToolResult(res)
	if images != 1 || omitted != 0 || len(out.Parts) != 2 || !out.Parts[1].IsImage() {
		t.Fatalf("accepted = images %d omitted %d parts %+v", images, omitted, out.Parts)
	}

	disabled := false
	r = &Runtime{cfg: sessioncfg.Config{ToolResultImages: &disabled}}
	out, images, omitted = r.adjustToolResult(res)
	if images != 0 || omitted != 1 || len(out.Parts) != 0 {
		t.Fatalf("disabled = images %d omitted %d parts %+v", images, omitted, out.Parts)
	}
	if !strings.Contains(out.Content, "not sent: the assigned model does not accept image content") {
		t.Fatalf("content = %q", out.Content)
	}
	if strings.Contains(out.Content, base64.StdEncoding.EncodeToString(image.Data)) {
		t.Fatal("content contains image bytes")
	}
}

func TestAdjustToolResultSessionBudget(t *testing.T) {
	image := content.ImagePart("image/png", []byte("png-bytes"))
	r := &Runtime{cfg: sessioncfg.Config{MaxSessionImageBytes: 4}}
	out, images, omitted := r.adjustToolResult(toolResult{Parts: []content.Part{image}})
	if images != 0 || omitted != 1 || !out.IsError || len(out.Parts) != 0 {
		t.Fatalf("over budget = images %d omitted %d error %v parts %+v", images, omitted, out.IsError, out.Parts)
	}
	if !strings.Contains(out.Content, "session image limit") {
		t.Fatalf("content = %q", out.Content)
	}

	// A second result that still fits is accepted.
	r = &Runtime{cfg: sessioncfg.Config{MaxSessionImageBytes: 20}}
	if out, images, _ = r.adjustToolResult(toolResult{Parts: []content.Part{image}}); images != 1 {
		t.Fatalf("first result rejected: %+v", out)
	}
	if out, images, _ = r.adjustToolResult(toolResult{Parts: []content.Part{image}}); images != 1 {
		t.Fatalf("second result rejected: %+v", out)
	}
	if out, images, _ = r.adjustToolResult(toolResult{Parts: []content.Part{image}}); images != 0 || !out.IsError {
		t.Fatalf("third result accepted: %+v", out)
	}
}

func TestConversationRoundTripsImageParts(t *testing.T) {
	path := t.TempDir() + "/conversation.jsonl"
	conv, err := openConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	msg := llm.Message{Role: "tool", ToolCallID: "call_1", Content: "caption", Parts: []content.Part{
		content.TextPart("caption"),
		content.ImagePart("image/png", []byte("png-bytes")),
	}}
	if err := conv.Append(msg); err != nil {
		t.Fatal(err)
	}
	if got := conv.ImageBytes(); got != 9 {
		t.Fatalf("ImageBytes = %d", got)
	}
	if err := conv.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.ImageBytes(); got != 9 {
		t.Fatalf("reloaded ImageBytes = %d", got)
	}
	if reopened.Count() != 1 || len(reopened.messages[0].Parts) != 2 || !reopened.messages[0].Parts[1].IsImage() {
		t.Fatalf("reloaded messages = %+v", reopened.messages)
	}
}

func TestExecuteToolCarriesImageParts(t *testing.T) {
	data := []byte("png-bytes")
	gw := &fakeGateway{
		begin: func(gateway.BeginRequest) (gateway.BeginResponse, error) {
			return gateway.BeginResponse{Result: &gateway.Result{
				Content: "caption\n[image image/png, 9 bytes]",
				Parts: []content.Part{
					content.TextPart("caption"),
					content.ImagePart("image/png", data),
				},
			}}, nil
		},
	}
	out := runTool(t, gw, nil, echoCall())
	step := decodeToolStep(t, out.run)
	if len(step.Message.Parts) != 2 || !step.Message.Parts[1].IsImage() {
		t.Fatalf("message parts = %+v", step.Message.Parts)
	}
	var result model.ToolResultData
	found := false
	for _, ev := range step.Events {
		if model.EventType(ev.Type) == model.EventToolResult {
			value, ok := model.DataAs[model.ToolResultData](ev.Data)
			if !ok {
				t.Fatal("tool result event has no data")
			}
			result, found = value, true
		}
	}
	if !found || result.Images != 1 || result.ImagesOmitted != 0 {
		t.Fatalf("tool result event = %+v found=%v", result, found)
	}
	if strings.Contains(out.trace, base64.StdEncoding.EncodeToString(data)) {
		t.Fatal("trace contains image base64")
	}
	if !strings.Contains(out.trace, `"images":1`) {
		t.Fatalf("trace does not record the image count: %s", out.trace)
	}
}

func TestReplayedToolStepChargesImageBudget(t *testing.T) {
	conv, err := openConversation(t.TempDir() + "/conversation.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer conv.Close()
	msg := llm.Message{Role: "tool", ToolCallID: "call_1", Content: "caption", Parts: []content.Part{
		content.TextPart("caption"),
		content.ImagePart("image/png", []byte("png-bytes")),
	}}
	output, err := json.Marshal(toolStepOutput{Message: msg, Executed: true})
	if err != nil {
		t.Fatal(err)
	}
	entry := journal.Entry{
		SessionID: "s1",
		Turn:      1,
		Step:      2,
		Name:      "tool:mcp_echo_image",
		Output:    output,
		ConvLen:   conv.Size() + conversationLineLen(msg),
	}
	r := &Runtime{cfg: sessioncfg.Config{}, conv: conv}
	if err := r.applyExisting(entry, r.applyTool); err != nil {
		t.Fatal(err)
	}
	if got := conv.ImageBytes(); got != 9 {
		t.Fatalf("conversation image bytes = %d", got)
	}
	if got := r.imageBudget().Used(); got != 9 {
		t.Fatalf("budget used = %d", got)
	}
}
