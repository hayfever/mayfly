package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/httpapi"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/runner"
	"github.com/hayfever/mayfly/internal/session"
	"github.com/hayfever/mayfly/internal/stub"
)

type sessionResponse struct {
	Session model.Session `json:"session"`
	URL     string        `json:"url"`
}

type eventsResponse struct {
	Events []model.Event `json:"events"`
}

func TestWebhookToArtifact(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	agentBin := buildAgent(t, root)
	llmServer := httptest.NewServer(stub.LLMHandler())
	defer llmServer.Close()
	mcpServer := httptest.NewServer(stub.MCPHandler())
	defer mcpServer.Close()

	ts := httptest.NewUnstartedServer(nil)
	addr := ts.Listener.Addr().String()
	cfg := testConfig(t, root, agentBin, addr, llmServer.URL, mcpServer.URL)
	skillArchive := buildSkillArchive(t)
	skillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(skillArchive)
	}))
	defer skillServer.Close()
	sum := sha256.Sum256(skillArchive)
	cfg.Skills.Sources = []config.SkillSource{{
		Name:            "remote",
		URL:             skillServer.URL + "/skills.tar.gz",
		SHA256:          hex.EncodeToString(sum[:]),
		Subdir:          "skills",
		StripComponents: new(1),
		Timeout:         config.Duration(5 * time.Second),
	}}

	store, err := artifact.New(context.Background(), cfg.Artifacts)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	local, err := runner.NewLocal(cfg.Runtime.AgentBinary, cfg.Runtime.StateDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := session.New(cfg, store, hub.New(1000), local, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !containsString(mgr.SkillNames(), "remote-skill") {
		t.Fatalf("remote skill not loaded: %v", mgr.SkillNames())
	}
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())

	queueStore, err := queue.New(ctx, config.Default().Queue)
	if err != nil {
		t.Fatal(err)
	}
	defer queueStore.Close()
	worker := queue.NewWorker(queueStore, func(ctx context.Context, job queue.Job) error {
		if _, err := mgr.Create(ctx, job.Options); err != nil {
			if errors.Is(err, session.ErrCapacity) {
				return fmt.Errorf("%w: %v", queue.ErrRetryLater, err)
			}
			return err
		}
		return nil
	}, 2, mgr.FreeSlots, 50*time.Millisecond, logger)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker.Run(ctx)
	}()
	defer func() {
		cancel()
		<-workerDone
	}()

	ts.Config.Handler = httpapi.New(cfg, mgr, queueStore, logger).Handler()
	ts.Start()
	defer ts.Close()

	base := ts.URL
	// The session runtime calls the control plane at the listener address.
	if cfg.Server.InternalURL != base {
		t.Fatalf("internal URL %q does not match test server %q", cfg.Server.InternalURL, base)
	}

	// Trigger through the unsigned generic webhook.
	webhookBody := []byte(`{"prompt":"Investigate the failing deploy.","title":"integration","agent":"triage"}`)
	resp, err := http.Post(base+"/hooks/generic", "application/json", bytes.NewReader(webhookBody))
	if err != nil {
		t.Fatal(err)
	}
	var hook struct {
		Accepted bool   `json:"accepted"`
		JobID    string `json:"job_id"`
	}
	decode(t, resp, &hook)
	if !hook.Accepted || hook.JobID == "" {
		t.Fatalf("webhook was not accepted: %+v", hook)
	}
	// The worker creates the session after the webhook is answered.
	id := waitForSession(t, base, 20*time.Second)

	events := waitForAssistant(t, base, id, 1, 40*time.Second)
	if !hasMCPResult(events) {
		t.Fatalf("MCP tool result missing from events: %s", formatEvents(events))
	}
	if !hasAssistantText(events) {
		t.Fatal("assistant reply missing")
	}

	// Attach over WebSocket and send a second message.
	ticketResp, err := admin(http.MethodPost, base+"/api/v1/sessions/"+id+"/attach-ticket", map[string]string{"actor": "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	var ticket struct {
		WSURL string `json:"ws_url"`
	}
	decode(t, ticketResp, &ticket)
	conn, _, err := websocket.DefaultDialer.Dial(ticket.WSURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	wsCh := make(chan model.Event, 128)
	go func() {
		defer close(wsCh)
		for {
			var msg struct {
				Type  string      `json:"type"`
				Event model.Event `json:"event"`
			}
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			if msg.Type == "event" {
				wsCh <- msg.Event
			}
		}
	}()
	// Drain the replayed backlog until the agent has replied once.
	replayed := 0
	deadline := time.After(20 * time.Second)
	for replayed < 1 {
		select {
		case ev, ok := <-wsCh:
			if !ok {
				t.Fatal("websocket closed during replay")
			}
			if ev.Type == model.EventAssistantMessage {
				replayed++
			}
		case <-deadline:
			t.Fatal("websocket replay timed out")
		}
	}
	if err := conn.WriteJSON(map[string]string{"type": "user_message", "text": "Second question."}); err != nil {
		t.Fatal(err)
	}
	secondDeadline := time.After(30 * time.Second)
	for replayed < 2 {
		select {
		case ev, ok := <-wsCh:
			if !ok {
				t.Fatal("websocket closed before second reply")
			}
			if ev.Type == model.EventAssistantMessage {
				replayed++
			}
		case <-secondDeadline:
			t.Fatal("second assistant reply timed out")
		}
	}

	if resp, err := admin(http.MethodPost, base+"/internal/v1/sessions/"+id+"/events", nil); err != nil {
		t.Fatal(err)
	} else if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("internal endpoint without token returned %d", resp.StatusCode)
	}

	// Close and wait for export.
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
	if final.Session.Artifact == nil {
		t.Fatal("session has no artifact")
	}
	if final.Session.MessageCount < 4 {
		t.Fatalf("message count = %d", final.Session.MessageCount)
	}
	if final.Session.Agent != "triage" {
		t.Fatalf("agent = %q", final.Session.Agent)
	}
	if final.Session.Harness != "mayfly" {
		t.Fatalf("harness = %q", final.Session.Harness)
	}
	if len(final.Session.Skills) != 1 || final.Session.Skills[0] != "incident-triage" {
		t.Fatalf("skills = %v", final.Session.Skills)
	}
	rawConfig, err := os.ReadFile(filepath.Join(cfg.Runtime.StateDir, "sessions", id, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var runtimeCfg struct {
		Agent        string         `json:"agent"`
		Harness      string         `json:"harness"`
		SystemPrompt string         `json:"system_prompt"`
		Protocol     int            `json:"runtime_protocol"`
		AgentConfig  map[string]any `json:"agent_config"`
	}
	if err := json.Unmarshal(rawConfig, &runtimeCfg); err != nil {
		t.Fatal(err)
	}
	if runtimeCfg.Agent != "triage" || runtimeCfg.Harness != "mayfly" || runtimeCfg.Protocol != 1 || !strings.Contains(runtimeCfg.SystemPrompt, "triage agent") {
		t.Fatalf("runtime config agent=%q harness=%q protocol=%d prompt=%q", runtimeCfg.Agent, runtimeCfg.Harness, runtimeCfg.Protocol, runtimeCfg.SystemPrompt)
	}
	if runtimeCfg.AgentConfig["triage"] != true {
		t.Fatalf("agent config = %#v", runtimeCfg.AgentConfig)
	}

	// Download and inspect the bundle.
	resp, err = admin(http.MethodGet, base+"/api/v1/sessions/"+id+"/artifact", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	entries := readBundle(t, raw)
	if string(entries["files/report.md"]) == "" || !strings.Contains(string(entries["files/report.md"]), "Mayfly session report") {
		t.Fatalf("report.md missing or empty: %v", mapKeys(entries))
	}
	if !strings.Contains(string(entries["traces.jsonl"]), "echo: hello from mayfly") {
		t.Fatal("MCP trace missing from bundle")
	}
	if !strings.Contains(string(entries["conversation.jsonl"]), "Second question.") {
		t.Fatal("second message missing from conversation")
	}
	var manifest struct {
		Status  string `json:"status"`
		Partial bool   `json:"partial"`
		Agent   string `json:"agent"`
		Harness string `json:"harness"`
		Files   []string
	}
	if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "completed" || manifest.Partial || manifest.Agent != "triage" || manifest.Harness != "mayfly" {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func testConfig(t *testing.T, root, agentBin, addr, llmURL, mcpURL string) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Server.Addr = addr
	cfg.Server.PublicURL = "http://" + addr
	cfg.Server.InternalURL = "http://" + addr
	cfg.AdminToken = "test-admin-token"
	cfg.Sessions.Namespace = "mayfly"
	cfg.Sessions.DefaultTTL = config.Duration(5 * time.Minute)
	cfg.Sessions.MaxTTL = config.Duration(10 * time.Minute)
	cfg.Sessions.IdleTimeout = 0
	cfg.Sessions.CloseGrace = config.Duration(20 * time.Second)
	cfg.Sessions.AllowShell = new(true)
	cfg.Runtime.Backend = "local"
	cfg.Runtime.AgentBinary = agentBin
	cfg.Runtime.StateDir = t.TempDir()
	cfg.Runtime.NetworkPolicy = new(false)
	cfg.Runtime.PollInterval = config.Duration(500 * time.Millisecond)
	cfg.LLM.BaseURL = llmURL + "/v1"
	cfg.LLM.Model = "stub"
	cfg.LLM.AllowModels = []string{"stub"}
	cfg.LLM.Timeout = config.Duration(20 * time.Second)
	cfg.Artifacts.Store = "local"
	cfg.Artifacts.Local.Dir = t.TempDir()
	cfg.Artifacts.MaxBytes = 32 << 20
	cfg.Skills.Dirs = []string{filepath.Join(root, "examples", "skills")}
	cfg.Agents.Dirs = []string{filepath.Join(root, "examples", "agents")}
	cfg.MCP.Servers = []config.MCPServerConfig{{
		Name:      "echo",
		Transport: "http",
		URL:       mcpURL + "/mcp",
	}}
	cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
	return cfg
}

func buildAgent(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mayfly-agent")
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, "./cmd/mayfly-agent")
	cmd.Dir = root
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build agent: %v\n%s", err, out)
	}
	return bin
}

func admin(method, url string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer test-admin-token")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func decode(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
}

// waitForSession polls until the worker has created a session.
func waitForSession(t *testing.T, base string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := admin(http.MethodGet, base+"/api/v1/sessions", nil)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Sessions []model.Session `json:"sessions"`
		}
		decode(t, resp, &out)
		if len(out.Sessions) > 0 {
			return out.Sessions[0].ID
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("worker did not create a session in time")
	return ""
}

func waitForAssistant(t *testing.T, base, id string, want int, timeout time.Duration) []model.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var events []model.Event
	for time.Now().Before(deadline) {
		resp, err := admin(http.MethodGet, base+"/api/v1/sessions/"+id+"/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		var out eventsResponse
		decode(t, resp, &out)
		events = out.Events
		count := 0
		for _, ev := range events {
			if ev.Type == model.EventAssistantMessage && ev.Text != "" {
				count++
			}
		}
		if count >= want {
			return events
		}
		if len(events) > 0 && events[len(events)-1].Type == model.EventError {
			t.Fatalf("session error: %s", formatEvents(events))
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for assistant reply: %s", formatEvents(events))
	return nil
}

func hasMCPResult(events []model.Event) bool {
	for _, ev := range events {
		if ev.Type == model.EventToolResult {
			if tool, _ := ev.Data["tool"].(string); strings.HasPrefix(tool, "mcp_") && strings.Contains(ev.Text, "echo: hello from mayfly") {
				return true
			}
		}
	}
	return false
}

func hasAssistantText(events []model.Event) bool {
	for _, ev := range events {
		if ev.Type == model.EventAssistantMessage && strings.Contains(ev.Text, "Completed.") {
			return true
		}
	}
	return false
}

func formatEvents(events []model.Event) string {
	var b strings.Builder
	for _, ev := range events {
		fmt.Fprintf(&b, "%s %q\n", ev.Type, ev.Text)
	}
	return b.String()
}

func readBundle(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[header.Name] = data
	}
	return out
}

func mapKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}

func buildSkillArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "---\nname: remote-skill\ndescription: Remote skill\n---\nUse remote evidence."
	if err := tw.WriteHeader(&tar.Header{Name: "repo/skills/remote.md", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
