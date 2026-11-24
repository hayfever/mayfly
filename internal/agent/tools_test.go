package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/platformclient"
)

type fakeArtifacts struct {
	window platformclient.ArtifactWindow
	key    string
	offset int64
	limit  int64
	calls  int
}

func (f *fakeArtifacts) ReadArtifact(_ context.Context, key string, offset, limit int64) (platformclient.ArtifactWindow, error) {
	f.calls++
	f.key, f.offset, f.limit = key, offset, limit
	out := f.window
	out.Key = key
	out.Offset = offset
	return out, nil
}

func artifactToolset(t *testing.T, reader ArtifactReader) *toolset {
	t.Helper()
	ts, err := newToolset(toolsetConfig{Workspace: t.TempDir(), Artifacts: reader})
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestReadArtifactRequiresGrant(t *testing.T) {
	ts := artifactToolset(t, nil)
	if _, _, err := ts.readArtifact(context.Background(), json.RawMessage(`{"key":"runs/a.json"}`)); err == nil || !strings.Contains(err.Error(), "no input artifacts") {
		t.Fatalf("err = %v", err)
	}
	for _, tool := range ts.Tools() {
		if tool.Function.Name == "read_artifact" {
			t.Fatal("read_artifact listed without grants")
		}
	}
}

func TestReadArtifactValidatesArguments(t *testing.T) {
	reader := &fakeArtifacts{window: platformclient.ArtifactWindow{Content: "abc"}}
	ts := artifactToolset(t, reader)
	if _, _, err := ts.readArtifact(context.Background(), json.RawMessage(`{"key":" "}`)); err == nil || !strings.Contains(err.Error(), "key is required") {
		t.Fatalf("empty key err = %v", err)
	}
	if _, _, err := ts.readArtifact(context.Background(), json.RawMessage(`{"key":"a","offset":-1}`)); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("negative offset err = %v", err)
	}
	if reader.calls != 0 {
		t.Fatalf("reader called %d times after bad arguments", reader.calls)
	}
}

func TestReadArtifactClampsWindow(t *testing.T) {
	reader := &fakeArtifacts{window: platformclient.ArtifactWindow{Total: 3, Content: "abc"}}
	ts := artifactToolset(t, reader)

	if _, _, err := ts.readArtifact(context.Background(), json.RawMessage(`{"key":"runs/a.json"}`)); err != nil {
		t.Fatal(err)
	}
	if reader.limit != artifactWindowBytes || reader.offset != 0 {
		t.Fatalf("default window = %d/%d", reader.offset, reader.limit)
	}

	if _, _, err := ts.readArtifact(context.Background(), json.RawMessage(`{"key":"runs/a.json","limit":1048576}`)); err != nil {
		t.Fatal(err)
	}
	if reader.limit != artifactWindowBytes {
		t.Fatalf("oversized limit = %d", reader.limit)
	}
}

func TestReadArtifactReportsNextOffset(t *testing.T) {
	next := int64(5)
	reader := &fakeArtifacts{window: platformclient.ArtifactWindow{Total: 10, Content: "abcde", NextOffset: &next}}
	ts := artifactToolset(t, reader)

	out, isErr, err := ts.readArtifact(context.Background(), json.RawMessage(`{"key":"runs/a.json","offset":2,"limit":5}`))
	if err != nil || isErr {
		t.Fatalf("err = %v isErr = %v", err, isErr)
	}
	var envelope struct {
		Key        string `json:"key"`
		Offset     int64  `json:"offset"`
		Bytes      int64  `json:"bytes"`
		Total      int64  `json:"total"`
		NextOffset *int64 `json:"next_offset"`
		Content    string `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Content != "abcde" || envelope.Total != 10 || envelope.Bytes != 5 || envelope.Offset != 2 {
		t.Fatalf("envelope = %+v", envelope)
	}
	if envelope.NextOffset == nil || *envelope.NextOffset != 7 {
		t.Fatalf("next = %v, want offset plus returned bytes", envelope.NextOffset)
	}

	reader.window.NextOffset = nil
	out, _, err = ts.readArtifact(context.Background(), json.RawMessage(`{"key":"runs/a.json"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, `"next_offset":5`) || !strings.Contains(out, `"next_offset":null`) {
		t.Fatalf("final window = %s", out)
	}
}

func TestReadArtifactShrinksToOutputCap(t *testing.T) {
	body := strings.Repeat("x", 80<<10)
	reader := &fakeArtifacts{window: platformclient.ArtifactWindow{Total: int64(len(body)), Content: body}}
	ts := artifactToolset(t, reader)

	out, isErr, err := ts.readArtifact(context.Background(), json.RawMessage(`{"key":"runs/a.json"}`))
	if err != nil || isErr {
		t.Fatalf("err = %v isErr = %v", err, isErr)
	}
	if len(out) > maxToolOutput {
		t.Fatalf("output = %d bytes, cap %d", len(out), maxToolOutput)
	}
	var envelope struct {
		Bytes   int64  `json:"bytes"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatal(err)
	}
	if int64(len(envelope.Content)) != envelope.Bytes || envelope.Bytes == 0 || len(envelope.Content) >= len(body) {
		t.Fatalf("content = %d bytes, reported %d", len(envelope.Content), envelope.Bytes)
	}
}

func TestToolsListGrantedPluginsLast(t *testing.T) {
	ts, err := newToolset(toolsetConfig{
		Workspace: t.TempDir(),
		Grant: map[string]gateway.DiscoveryTool{
			"plugin_tickets_get_ticket": {
				Name:      "plugin_tickets_get_ticket",
				Owner:     "tickets",
				Placement: gateway.Service,
			},
			"plugin_notify_post": {
				Name:        "plugin_notify_post",
				Owner:       "notify",
				Placement:   gateway.Sidecar,
				Description: "Send a message.",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()

	tools := ts.Tools()
	if len(tools) == 0 {
		t.Fatal("no tools")
	}
	if got := tools[len(tools)-1].Function.Name; got != "plugin_tickets_get_ticket" {
		t.Fatalf("last tool = %q, want the sorted plugin tools", got)
	}
	var tickets, notify bool
	for _, tool := range tools {
		switch tool.Function.Name {
		case "plugin_tickets_get_ticket":
			tickets = true
			want := "Plugin tool plugin_tickets_get_ticket from plugin tickets"
			if tool.Function.Description != want {
				t.Fatalf("description = %q, want %q", tool.Function.Description, want)
			}
		case "plugin_notify_post":
			// A sidecar tool with no local process is not offered until the
			// runtime can reach the sidecar.
			notify = true
		}
	}
	if !tickets || notify {
		t.Fatalf("tickets = %v, notify = %v", tickets, notify)
	}
}

// TestToolsDeclareGatewayHTTPMCP covers an HTTP mcp.servers tool: its
// description and schema come from the gateway grant, and the runtime
// declares it without holding a server of its own.
func TestToolsDeclareGatewayHTTPMCP(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	ts, err := newToolset(toolsetConfig{
		Workspace: t.TempDir(),
		Grant: map[string]gateway.DiscoveryTool{
			"mcp_linear_search": {
				Name:        "mcp_linear_search",
				Owner:       "linear",
				Placement:   gateway.MCPHTTP,
				Description: "Search issues",
				InputSchema: schema,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()

	for _, tool := range ts.Tools() {
		if tool.Function.Name != "mcp_linear_search" {
			continue
		}
		if tool.Function.Description != "Search issues" {
			t.Fatalf("description = %q", tool.Function.Description)
		}
		if string(tool.Function.Parameters) != string(schema) {
			t.Fatalf("schema = %s", tool.Function.Parameters)
		}
		return
	}
	t.Fatal("the gateway's HTTP MCP tool is not declared")
}

// TestCallRefusesUngrantedMCP keeps an mcp_* name that discovery did not grant
// from reaching the broker proxy and skipping the ledger.
func TestCallRefusesUngrantedMCP(t *testing.T) {
	ts, err := newToolset(toolsetConfig{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	if _, err := ts.Call(context.Background(), "mcp_linear_search", json.RawMessage(`{}`)); err == nil {
		t.Fatal("an ungranted MCP tool was called")
	} else if !strings.Contains(err.Error(), "not granted") {
		t.Fatalf("error = %v", err)
	}
}
