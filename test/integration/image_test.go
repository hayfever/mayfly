package integration

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/httpapi"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/runner"
	"github.com/hayfever/mayfly/internal/session"
	"github.com/hayfever/mayfly/internal/stub"
)

// recordingLLM records every model request body and forwards it to the stub.
type recordingLLM struct {
	mu      sync.Mutex
	handler http.Handler
	bodies  [][]byte
}

func (r *recordingLLM) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	req.Body = io.NopCloser(bytes.NewReader(body))
	r.handler.ServeHTTP(w, req)
}

func (r *recordingLLM) has(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, body := range r.bodies {
		if strings.Contains(string(body), substr) {
			return true
		}
	}
	return false
}

func (r *recordingLLM) any(fn func([]byte) bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, body := range r.bodies {
		if fn(body) {
			return true
		}
	}
	return false
}

// TestMCPImageReachesModel runs one session whose MCP server returns an image
// and checks that the next model request carries the image as a content part.
func TestMCPImageReachesModel(t *testing.T) {
	runMCPImageSession(t, stub.MCPHandler())
}

// TestMCPImageOverSSEReachesModel does the same through a Streamable HTTP
// server that frames its responses as text/event-stream, with a legal 5 MiB
// image. Its base64 form is about 6.7 MiB, so a smaller response bound would
// turn it into a transport error.
func TestMCPImageOverSSEReachesModel(t *testing.T) {
	runMCPImageSession(t, stub.MCPHandlerWithOptions(stub.MCPOptions{SSE: true, ImageBytes: 5 << 20}))
}

// runMCPImageSession drives one session against mcpHandler and asserts the
// image reaches the recorded model request.
func runMCPImageSession(t *testing.T, mcpHandler http.Handler) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	agentBin := buildAgent(t, root)
	recorder := &recordingLLM{handler: stub.LLMHandler()}
	llmServer := httptest.NewServer(recorder)
	defer llmServer.Close()
	mcpServer := httptest.NewServer(mcpHandler)
	defer mcpServer.Close()

	ts := httptest.NewUnstartedServer(nil)
	addr := ts.Listener.Addr().String()
	cfg := testConfig(t, root, agentBin, addr, llmServer.URL, mcpServer.URL)
	cfg.MCP.Servers[0].Tools = []string{"image"}

	store, err := artifact.New(context.Background(), cfg.Artifacts)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	local, err := runner.NewLocal(cfg.Runtime.AgentBinary, cfg.Runtime.StateDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	journalStore := journal.NewMemory()
	mgr, err := session.New(cfg, testBundle(t, cfg), store, hub.New(1000), journalStore, local, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	toolGateway := gateway.New(gateway.Options{
		Ledger:        journalStore,
		Logger:        logger,
		ContentLimits: cfg.MCP.ContentLimits(),
		MCPServer: func(name string) (config.MCPServerConfig, bool) {
			for _, server := range cfg.MCP.Servers {
				if server.Name == name {
					return server, true
				}
			}
			return config.MCPServerConfig{}, false
		},
	})
	mgr.SetGateway(toolGateway)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())

	ts.Config.Handler = httpapi.New(cfg, mgr, nil, logger, httpapi.WithGateway(toolGateway)).Handler()
	ts.Start()
	defer ts.Close()
	base := ts.URL

	resp, err := admin(http.MethodPost, base+"/api/v1/sessions", map[string]any{
		"prompt": "Describe the image.",
		"agent":  "triage",
	})
	if err != nil {
		t.Fatal(err)
	}
	var created sessionResponse
	decode(t, resp, &created)
	id := created.Session.ID
	if id == "" {
		t.Fatal("session was not created")
	}

	events := waitForAssistant(t, base, id, 1, 40*time.Second)
	found := false
	for _, ev := range events {
		if ev.Type != model.EventToolResult {
			continue
		}
		data, ok := model.DataAs[model.ToolResultData](ev.Data)
		if !ok || data.Tool != "mcp_echo_image" {
			continue
		}
		found = true
		if data.Images != 1 || data.ImagesOmitted != 0 || data.Error {
			t.Fatalf("tool result event = %+v text=%q", data, ev.Text)
		}
		if strings.Contains(ev.Text, stub.ImagePNGBase64) {
			t.Fatal("tool result event contains image base64")
		}
	}
	if !found {
		t.Fatalf("image tool result missing: %s", formatEvents(events))
	}
	if !recorder.any(func(body []byte) bool {
		return strings.Contains(string(body), `"type":"image_url"`) &&
			strings.Contains(string(body), "data:image/png;base64,")
	}) {
		t.Fatal("model request did not carry the image content part")
	}
	if recorder.has(`"parts"`) {
		t.Fatal("model request leaked the conversation log shape")
	}

	closeResp, err := admin(http.MethodPost, base+"/api/v1/sessions/"+id+"/close", map[string]string{"reason": "test"})
	if err != nil {
		t.Fatal(err)
	}
	_ = closeResp.Body.Close()
	var final sessionResponse
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		resp, err := admin(http.MethodGet, base+"/api/v1/sessions/"+id, nil)
		if err != nil {
			t.Fatal(err)
		}
		decode(t, resp, &final)
		if final.Session.Status == model.StatusCompleted {
			break
		}
	}
	if final.Session.Status != model.StatusCompleted {
		t.Fatalf("session status = %s error=%q", final.Session.Status, final.Session.Error)
	}
}
