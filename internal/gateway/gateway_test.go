package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

const (
	getTool   = "plugin_alpha_get"
	otherTool = "plugin_alpha_other"
	betaTool  = "plugin_beta_query"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestGateway returns a gateway with memory ledger and sequence floor, a
// registered grant, and the session clock.
func newTestGateway(t *testing.T, opts Options, tools ...GrantTool) (*Gateway, *journal.Memory) {
	t.Helper()
	mem := journal.NewMemory()
	if opts.Ledger == nil {
		opts.Ledger = mem
	}
	if opts.Sequencer == nil {
		opts.Sequencer = mem
	}
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	if err := mem.Reserve(context.Background(), "s1", 10); err != nil {
		t.Fatal(err)
	}
	g := New(opts)
	table := make(map[string]GrantTool, len(tools))
	for _, tool := range tools {
		table[tool.Name] = tool
	}
	g.Register(Grant{SessionID: "s1", Agent: "helper", Tools: table, MaxSteps: 4})
	return g, mem
}

func inlineTool(name, owner, remote string, idempotent bool) GrantTool {
	return GrantTool{
		Name:       name,
		Owner:      owner,
		Remote:     remote,
		Placement:  Inline,
		Idempotent: idempotent,
		Timeout:    30 * time.Second,
		MaxOutput:  4096,
	}
}

func begin(g *Gateway, tool string, turn int64, step int, args string) (BeginResponse, error) {
	return g.Begin(context.Background(), "s1", BeginRequest{
		Turn:      turn,
		Step:      step,
		CallID:    "call_1",
		Tool:      tool,
		Arguments: json.RawMessage(args),
	})
}

func statusOf(err error) int {
	var gerr *Error
	if errors.As(err, &gerr) {
		return gerr.Status
	}
	return 0
}

func TestGatewayResumeTable(t *testing.T) {
	ctx := context.Background()

	t.Run("none records pending and dispatches", func(t *testing.T) {
		g, mem := newTestGateway(t, Options{}, inlineTool(getTool, "alpha", "get", true))
		resp, err := begin(g, getTool, 3, 4, `{"q":"x"}`)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Dispatch == nil || resp.Result != nil {
			t.Fatalf("response = %+v, want a dispatch", resp)
		}
		if resp.Dispatch.Key != "s1/3/4" || resp.Dispatch.TimeoutMS != 30_000 {
			t.Fatalf("dispatch = %+v", resp.Dispatch)
		}
		rec, found, err := mem.Get(ctx, "s1", "s1/3/4")
		if err != nil || !found {
			t.Fatalf("ledger record = found %v, %v", found, err)
		}
		if rec.State != journal.CallPending || rec.Attempts != 1 || rec.Tool != getTool {
			t.Fatalf("ledger record = %+v", rec)
		}
	})

	t.Run("done replays the result", func(t *testing.T) {
		g, mem := newTestGateway(t, Options{}, inlineTool(getTool, "alpha", "get", true))
		if _, err := begin(g, getTool, 3, 4, `{}`); err != nil {
			t.Fatal(err)
		}
		if err := g.Complete(ctx, "s1", CompleteRequest{Key: "s1/3/4", Content: "recorded"}); err != nil {
			t.Fatal(err)
		}
		resp, err := begin(g, getTool, 3, 4, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Dispatch != nil || resp.Result == nil {
			t.Fatalf("response = %+v, want a result", resp)
		}
		if resp.Result.Content != "recorded" || !resp.Result.Replayed || resp.Result.Interrupted {
			t.Fatalf("result = %+v", resp.Result)
		}
		rec, _, _ := mem.Get(ctx, "s1", "s1/3/4")
		if rec.Attempts != 1 {
			t.Fatalf("replay incremented attempts: %+v", rec)
		}
	})

	t.Run("pending idempotent dispatches again", func(t *testing.T) {
		g, mem := newTestGateway(t, Options{}, inlineTool(getTool, "alpha", "get", true))
		if _, err := begin(g, getTool, 3, 4, `{}`); err != nil {
			t.Fatal(err)
		}
		resp, err := begin(g, getTool, 3, 4, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Dispatch == nil || resp.Dispatch.Key != "s1/3/4" {
			t.Fatalf("response = %+v, want the same dispatch", resp)
		}
		rec, _, _ := mem.Get(ctx, "s1", "s1/3/4")
		if rec.State != journal.CallPending || rec.Attempts != 2 {
			t.Fatalf("ledger record = %+v", rec)
		}
	})

	t.Run("pending non-idempotent is interrupted once and replayed after", func(t *testing.T) {
		g, mem := newTestGateway(t, Options{}, inlineTool(otherTool, "alpha", "other", false))
		if _, err := begin(g, otherTool, 3, 4, `{}`); err != nil {
			t.Fatal(err)
		}
		resp, err := begin(g, otherTool, 3, 4, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Result == nil || !resp.Result.IsError || !resp.Result.Interrupted || resp.Result.Replayed {
			t.Fatalf("first resume result = %+v", resp.Result)
		}
		if !strings.Contains(resp.Result.Content, "interrupted") {
			t.Fatalf("interrupted content = %q", resp.Result.Content)
		}
		rec, _, _ := mem.Get(ctx, "s1", "s1/3/4")
		if rec.State != journal.CallDone || !rec.Interrupted || !rec.IsError {
			t.Fatalf("ledger record after interruption = %+v", rec)
		}

		// The interruption is now the recorded answer; a second resume gets
		// the same result and does not run the tool again.
		again, err := begin(g, otherTool, 3, 4, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if again.Dispatch != nil || again.Result == nil {
			t.Fatalf("second resume = %+v, want a result", again)
		}
		if !again.Result.Replayed || !again.Result.Interrupted || again.Result.Content != resp.Result.Content {
			t.Fatalf("second resume result = %+v, want the recorded interruption", again.Result)
		}
	})
}

func TestGatewayKeyReuse(t *testing.T) {
	g, mem := newTestGateway(t, Options{},
		inlineTool(getTool, "alpha", "get", true),
		inlineTool(otherTool, "alpha", "other", false),
	)
	if _, err := begin(g, getTool, 1, 1, `{"a":1}`); err != nil {
		t.Fatal(err)
	}

	// Different arguments for a key that is already pending are fatal.
	if _, err := begin(g, getTool, 1, 1, `{"a":2}`); statusOf(err) != 409 {
		t.Fatalf("different arguments error = %v, want 409", err)
	}
	// A different tool for the same key is fatal too.
	if _, err := begin(g, otherTool, 1, 1, `{"a":1}`); statusOf(err) != 409 {
		t.Fatalf("different tool error = %v, want 409", err)
	}
	// The same arguments in another encoding are the same call.
	resp, err := begin(g, getTool, 1, 1, `{ "a" : 1 }`)
	if err != nil {
		t.Fatalf("canonical arguments were rejected: %v", err)
	}
	if resp.Dispatch == nil {
		t.Fatalf("response = %+v, want a dispatch", resp)
	}
	rec, _, _ := mem.Get(context.Background(), "s1", "s1/1/1")
	if rec.Tool != getTool || rec.Attempts != 2 {
		t.Fatalf("ledger record = %+v", rec)
	}
}

func TestGatewayRateLimit(t *testing.T) {
	manifests := func() []plugin.Manifest {
		return []plugin.Manifest{{
			Name:   "alpha",
			Limits: plugin.Limits{Rate: &plugin.Rate{Requests: 1, Interval: "1m", Burst: 1}},
		}}
	}
	g, mem := newTestGateway(t, Options{Manifests: manifests},
		inlineTool(getTool, "alpha", "get", true),
		inlineTool(betaTool, "beta", "query", true),
	)
	if _, err := begin(g, getTool, 1, 1, `{}`); err != nil {
		t.Fatal(err)
	}
	_, err := begin(g, getTool, 1, 2, `{}`)
	var gerr *Error
	if !errors.As(err, &gerr) || gerr.Status != 429 {
		t.Fatalf("rate limited error = %v, want 429", err)
	}
	if gerr.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %s, want a positive hint", gerr.RetryAfter)
	}
	if _, found, _ := mem.Get(context.Background(), "s1", "s1/1/2"); found {
		t.Fatal("a rate-limited begin recorded a call")
	}
	// Another plugin has its own limiter.
	if _, err := begin(g, betaTool, 1, 3, `{}`); err != nil {
		t.Fatalf("another plugin was rate limited: %v", err)
	}
}

func TestGatewayInflightCap(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	g, _ := newTestGateway(t, Options{
		MaxInflight: 1,
		Now:         func() time.Time { return now },
	}, inlineTool(getTool, "alpha", "get", true))

	if _, err := begin(g, getTool, 1, 1, `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := begin(g, getTool, 1, 2, `{}`); statusOf(err) != 503 {
		t.Fatalf("over-cap begin error = %v, want 503", err)
	}
	// Completing the first call frees the slot.
	if err := g.Complete(ctx, "s1", CompleteRequest{Key: "s1/1/1", Content: "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := begin(g, getTool, 1, 2, `{}`); err != nil {
		t.Fatalf("begin after complete: %v", err)
	}
	if _, err := begin(g, getTool, 1, 3, `{}`); statusOf(err) != 503 {
		t.Fatalf("second over-cap begin error = %v, want 503", err)
	}
	// A dispatch that outlived its timeout is released without a complete.
	now = now.Add(31 * time.Second)
	if _, err := begin(g, getTool, 1, 3, `{}`); err != nil {
		t.Fatalf("begin after timeout: %v", err)
	}
}

func TestGatewayStepBounds(t *testing.T) {
	g, _ := newTestGateway(t, Options{}, inlineTool(getTool, "alpha", "get", true))

	if _, err := begin(g, getTool, 0, 1, `{}`); statusOf(err) != 400 {
		t.Fatalf("turn 0 error = %v, want 400", err)
	}
	if _, err := begin(g, getTool, 11, 1, `{}`); statusOf(err) != 400 {
		t.Fatalf("unreserved turn error = %v, want 400", err)
	}
	if _, err := begin(g, getTool, 3, 0, `{}`); statusOf(err) != 400 {
		t.Fatalf("step 0 error = %v, want 400", err)
	}
	// Four model rounds plus the user step allow steps below
	// 1 + 4*(1+128) = 517.
	if _, err := begin(g, getTool, 3, 517, `{}`); statusOf(err) != 400 {
		t.Fatalf("step over the bound error = %v, want 400", err)
	}
	if _, err := begin(g, getTool, 3, 516, `{}`); err != nil {
		t.Fatalf("step at the bound: %v", err)
	}
	if _, err := begin(g, getTool, 10, 1, `{}`); err != nil {
		t.Fatalf("reserved turn: %v", err)
	}
}

func TestGrantFor(t *testing.T) {
	cfg := sessioncfg.Config{
		SessionID: "s1",
		MaxSteps:  8,
		Plugins: []sessioncfg.Plugin{
			{
				Name:           "alpha",
				Placement:      "inline",
				MaxOutputBytes: 4096,
				Tools: []sessioncfg.PluginTool{
					{Name: "get", Idempotent: true, TimeoutSeconds: 30},
					{Name: "post"},
				},
			},
			{
				Name:           "beta",
				Placement:      "service",
				MaxOutputBytes: plugin.MaxOutputBytes,
				Tools:          []sessioncfg.PluginTool{{Name: "query", TimeoutSeconds: 60}},
			},
		},
	}
	grant := GrantFor(cfg, "helper", []config.MCPServerConfig{
		{Name: "search", Transport: "http"},
		{Name: "local", Transport: "stdio"},
	})

	if grant.SessionID != "s1" || grant.Agent != "helper" || grant.MaxSteps != 8 {
		t.Fatalf("grant identity = %+v", grant)
	}
	get, ok := grant.Tools["plugin_alpha_get"]
	if !ok {
		t.Fatalf("missing plugin_alpha_get in %v", grant.Tools)
	}
	if get.Owner != "alpha" || get.Remote != "get" || get.Placement != Inline || !get.Idempotent {
		t.Fatalf("get tool = %+v", get)
	}
	if get.Timeout != 30*time.Second || get.MaxOutput != 4096 {
		t.Fatalf("get limits = %+v", get)
	}
	post := grant.Tools["plugin_alpha_post"]
	if post.Timeout != defaultCallTimeout || post.Idempotent {
		t.Fatalf("post defaults = %+v", post)
	}
	query := grant.Tools["plugin_beta_query"]
	if query.Placement != Service || query.Timeout != time.Minute {
		t.Fatalf("service tool = %+v", query)
	}
	if len(grant.Wildcards) != 2 || grant.Wildcards["search"] != MCPHTTP || grant.Wildcards["local"] != MCPStdio {
		t.Fatalf("wildcards = %v, want the HTTP and stdio servers", grant.Wildcards)
	}
}

func TestAuditOmitsPayloads(t *testing.T) {
	const (
		argsSentinel = "SENTINEL-ARGUMENT-TEXT"
		resultText   = "SENTINEL-RESULT-TEXT"
	)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := NewAudit(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })

	g, _ := newTestGateway(t, Options{Audit: audit, Logger: logger}, inlineTool(getTool, "alpha", "get", true))
	if _, err := begin(g, getTool, 1, 1, `{"value":"`+argsSentinel+`"}`); err != nil {
		t.Fatal(err)
	}
	if err := g.Complete(context.Background(), "s1", CompleteRequest{Key: "s1/1/1", Content: resultText}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{argsSentinel, resultText} {
		if bytes.Contains(raw, []byte(text)) {
			t.Fatalf("audit file contains %q", text)
		}
		if bytes.Contains(logs.Bytes(), []byte(text)) {
			t.Fatalf("audit log contains %q", text)
		}
	}
	for _, want := range []string{`"event":"call.begin"`, `"event":"call.done"`, `"key":"s1/1/1"`, `"args_sha256":"`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("audit file is missing %s: %s", want, raw)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("audit file mode = %o, want 600", got)
	}
}

func TestCallToken(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)
	claims := Claims{
		SessionID:  "s1",
		Plugin:     "alpha",
		Tool:       "post",
		Key:        "s1/1/1",
		ArgsSHA256: strings.Repeat("d", 64),
		Expires:    now.Add(time.Minute).Unix(),
	}
	token, err := SignToken(key, claims)
	if err != nil {
		t.Fatal(err)
	}
	scope := TokenScope{SessionID: "s1", Plugin: "alpha", Tool: "post", ArgsSHA256: claims.ArgsSHA256}
	got, err := VerifyToken(key, token, scope, now)
	if err != nil || got.Key != claims.Key {
		t.Fatalf("VerifyToken = %+v, %v", got, err)
	}

	parts := strings.Split(token, ".")
	tampered := parts[0] + "." + parts[1] + "x." + parts[2]
	otherKey := []byte("ffffffffffffffffffffffffffffffff")
	cases := map[string]struct {
		key   []byte
		token string
		want  TokenScope
		now   time.Time
	}{
		"tampered claims": {key, tampered, scope, now},
		"wrong key":       {otherKey, token, scope, now},
		"expired":         {key, token, scope, now.Add(2 * time.Minute)},
		"wrong tool":      {key, token, TokenScope{Tool: "other", ArgsSHA256: claims.ArgsSHA256}, now},
		"wrong hash":      {key, token, TokenScope{Tool: "post", ArgsSHA256: strings.Repeat("e", 64)}, now},
		"wrong session":   {key, token, TokenScope{SessionID: "s2", Tool: "post"}, now},
		"wrong plugin":    {key, token, TokenScope{Plugin: "beta", Tool: "post"}, now},
		"malformed":       {key, "v1.only-two", scope, now},
		"unknown version": {key, "v2." + parts[1] + "." + parts[2], scope, now},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyToken(tc.key, tc.token, tc.want, tc.now); !errors.Is(err, ErrToken) {
				t.Fatalf("VerifyToken error = %v, want ErrToken", err)
			}
		})
	}

	// A discovery token has no key or hash and authorizes any tool.
	discovery, err := SignToken(key, Claims{
		SessionID: "s1",
		Plugin:    "alpha",
		Tool:      "*",
		Expires:   now.Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyToken(key, discovery, TokenScope{SessionID: "s1", Plugin: "alpha", Tool: "anything"}, now); err != nil {
		t.Fatalf("discovery token: %v", err)
	}
	if _, err := SignToken(nil, claims); !errors.Is(err, ErrToken) {
		t.Fatalf("empty key error = %v, want ErrToken", err)
	}
}

func TestDiscoverLocalTools(t *testing.T) {
	sidecar := inlineTool("plugin_alpha_status", "alpha", "status", true)
	sidecar.Placement = Sidecar
	service := inlineTool("plugin_beta_query", "beta", "query", true)
	service.Placement = Service
	g, _ := newTestGateway(t, Options{}, sidecar, service)
	grant, _ := g.grantFor("s1")
	grant.SidecarKey = []byte("0123456789abcdef0123456789abcdef")
	g.Register(grant)

	disc, err := g.Discover(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(disc.Tools) != 1 || disc.Tools[0].Name != "plugin_alpha_status" {
		t.Fatalf("discovery tools = %+v", disc.Tools)
	}
	if disc.Tools[0].Placement != Sidecar || disc.Tools[0].TimeoutMS != 30_000 {
		t.Fatalf("discovery tool = %+v", disc.Tools[0])
	}
	token := disc.Sidecars["alpha"].DiscoveryToken
	alphaKey := plugin.SidecarKey(grant.SidecarKey, "alpha")
	if _, err := VerifyToken(alphaKey, token, TokenScope{SessionID: "s1", Plugin: "alpha", Tool: "status"}, time.Now()); err != nil {
		t.Fatalf("sidecar discovery token: %v", err)
	}
	if _, err := VerifyToken(alphaKey, token, TokenScope{SessionID: "s1", Plugin: "beta", Tool: "status"}, time.Now()); err == nil {
		t.Fatal("alpha's token verified as beta")
	}
	if _, err := g.Discover(context.Background(), "missing"); statusOf(err) != 404 {
		t.Fatalf("unknown session error = %v, want 404", err)
	}
}

// TestGatewaySameRuntimeReissues covers a begin whose answer was lost: the
// same runtime instance opens the same pending call again, and the gateway
// issues it instead of recording an interruption.
func TestGatewaySameRuntimeReissues(t *testing.T) {
	ctx := context.Background()
	g, mem := newTestGateway(t, Options{MaxInflight: 1}, inlineTool(otherTool, "alpha", "other", false))
	req := BeginRequest{
		Turn:       3,
		Step:       4,
		CallID:     "call_1",
		Tool:       otherTool,
		InstanceID: "run-1",
		Arguments:  json.RawMessage(`{}`),
	}

	resp, err := g.Begin(ctx, "s1", req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Dispatch == nil || resp.Dispatch.Key != "s1/3/4" {
		t.Fatalf("first begin = %+v, want a dispatch", resp)
	}

	// The runtime never got that answer, so the tool never ran. The retry
	// reuses the in-flight slot and is dispatched again.
	again, err := g.Begin(ctx, "s1", req)
	if err != nil {
		t.Fatal(err)
	}
	if again.Dispatch == nil || again.Result != nil || again.Dispatch.Key != "s1/3/4" {
		t.Fatalf("retry from the same instance = %+v, want a dispatch", again)
	}
	rec, found, err := mem.Get(ctx, "s1", "s1/3/4")
	if err != nil || !found {
		t.Fatalf("ledger record = found %v, %v", found, err)
	}
	if rec.State != journal.CallPending || rec.Attempts != 2 || rec.InstanceID != "run-1" {
		t.Fatalf("ledger record = %+v", rec)
	}

	// A replacement container is a new instance: the call it never received
	// is recorded as interrupted, not issued again.
	restarted := req
	restarted.InstanceID = "run-2"
	resumed, err := g.Begin(ctx, "s1", restarted)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Result == nil || !resumed.Result.Interrupted || resumed.Dispatch != nil {
		t.Fatalf("begin from a new instance = %+v, want the interrupted result", resumed)
	}
	rec, _, err = mem.Get(ctx, "s1", "s1/3/4")
	if err != nil || rec.State != journal.CallDone || !rec.Interrupted {
		t.Fatalf("ledger record after interruption = %+v, %v", rec, err)
	}
}

// TestDiscoverRequiredOwnerFollowsPlacement proves that only a service plugin
// fails a session: an mcp.servers owner that shares a required plugin's name
// reports its failure without failing the session.
func TestDiscoverRequiredOwnerFollowsPlacement(t *testing.T) {
	upstream := newFakeUpstream("send")
	upstream.setFailInit(true)
	server := httptest.NewServer(upstream)
	defer server.Close()

	manifests := func() []plugin.Manifest {
		return []plugin.Manifest{
			{Name: "chat", Placement: plugin.Service, Required: true},
			{Name: "tickets", Placement: plugin.Service, Required: true},
		}
	}
	def := config.MCPServerConfig{Name: "chat", Transport: "http", URL: server.URL}
	opts := Options{
		Manifests: manifests,
		MCPServer: func(name string) (config.MCPServerConfig, bool) {
			if name == def.Name {
				return def, true
			}
			return config.MCPServerConfig{}, false
		},
		ServiceURL: func(name string) (string, []byte, error) {
			return "", nil, errors.New("no endpoint")
		},
	}
	mem := journal.NewMemory()
	if err := mem.Reserve(context.Background(), "s1", 10); err != nil {
		t.Fatal(err)
	}
	opts.Ledger = mem
	opts.Sequencer = mem
	opts.Logger = quietLogger()
	g := New(opts)
	g.Register(Grant{
		SessionID: "s1",
		Agent:     "helper",
		Tools: map[string]GrantTool{
			"plugin_tickets_query": {
				Name:      "plugin_tickets_query",
				Owner:     "tickets",
				Remote:    "query",
				Placement: Service,
				Timeout:   5 * time.Second,
				MaxOutput: 4096,
			},
		},
		Wildcards: map[string]Placement{"chat": MCPHTTP},
		MaxSteps:  4,
	})

	discovery, err := g.Discover(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{}
	for _, failure := range discovery.Errors {
		required[failure.Owner] = failure.Required
	}
	// The HTTP server shares the plugin's name but is not a bundle entry.
	if required["chat"] {
		t.Fatalf("discovery errors = %+v, want the HTTP server not required", discovery.Errors)
	}
	if !required["tickets"] {
		t.Fatalf("discovery errors = %+v, want the required plugin reported", discovery.Errors)
	}
}

// TestSidecarTokensUsePerPluginKeys keeps one sidecar's key from verifying a
// token the gateway minted for another, and never signs with the session key.
func TestSidecarTokensUsePerPluginKeys(t *testing.T) {
	ctx := context.Background()
	alpha := inlineTool("plugin_alpha_status", "alpha", "status", true)
	alpha.Placement = Sidecar
	beta := inlineTool("plugin_beta_status", "beta", "status", true)
	beta.Placement = Sidecar
	g, _ := newTestGateway(t, Options{}, alpha, beta)
	grant, _ := g.grantFor("s1")
	grant.SidecarKey = bytes.Repeat([]byte{0x5a}, 32)
	g.Register(grant)

	alphaKey := plugin.SidecarKey(grant.SidecarKey, "alpha")
	betaKey := plugin.SidecarKey(grant.SidecarKey, "beta")
	if bytes.Equal(alphaKey, grant.SidecarKey) {
		t.Fatal("a sidecar key equals the session key")
	}

	disc, err := g.Discover(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	discoveryToken := disc.Sidecars["alpha"].DiscoveryToken
	discoveryScope := TokenScope{SessionID: "s1", Plugin: "alpha", Tool: "*"}
	if _, err := VerifyToken(alphaKey, discoveryToken, discoveryScope, time.Now()); err != nil {
		t.Fatalf("alpha discovery token: %v", err)
	}
	if _, err := VerifyToken(betaKey, discoveryToken, TokenScope{SessionID: "s1", Plugin: "beta", Tool: "*"}, time.Now()); err == nil {
		t.Fatal("alpha's discovery token verified with beta's key")
	}

	resp, err := g.Begin(ctx, "s1", BeginRequest{
		Turn:       1,
		Step:       1,
		Tool:       alpha.Name,
		Arguments:  json.RawMessage(`{}`),
		InstanceID: "run-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Dispatch == nil {
		t.Fatalf("begin = %+v, want a sidecar dispatch", resp)
	}
	callScope := TokenScope{SessionID: "s1", Plugin: "alpha", Tool: "status"}
	if _, err := VerifyToken(alphaKey, resp.Dispatch.Token, callScope, time.Now()); err != nil {
		t.Fatalf("alpha call token: %v", err)
	}
	if _, err := VerifyToken(betaKey, resp.Dispatch.Token, TokenScope{SessionID: "s1", Plugin: "beta", Tool: "status"}, time.Now()); err == nil {
		t.Fatal("alpha's call token verified with beta's key")
	}
}

type recordingCallObserver struct {
	calls []string
}

func (o *recordingCallObserver) ToolCall(tool, result string) {
	o.calls = append(o.calls, tool+":"+result)
}

func TestObserverSeesCompletedAndDeniedCalls(t *testing.T) {
	observer := &recordingCallObserver{}
	g, _ := newTestGateway(t, Options{Observer: observer}, inlineTool(getTool, "alpha", "get", true))

	if _, err := begin(g, "not_granted", 1, 1, `{}`); statusOf(err) != http.StatusForbidden {
		t.Fatalf("ungranted tool error = %v", err)
	}
	if _, err := begin(g, getTool, 1, 2, `{}`); err != nil {
		t.Fatal(err)
	}
	if err := g.Complete(context.Background(), "s1", CompleteRequest{Key: "s1/1/2", Content: "ok"}); err != nil {
		t.Fatal(err)
	}
	// A ledger replay answers from the record and must not count again.
	if _, err := begin(g, getTool, 1, 2, `{}`); err != nil {
		t.Fatal(err)
	}

	want := []string{"unknown:denied", getTool + ":ok"}
	if len(observer.calls) != len(want) {
		t.Fatalf("observed %v, want %v", observer.calls, want)
	}
	for i, call := range want {
		if observer.calls[i] != call {
			t.Fatalf("observed %v, want %v", observer.calls, want)
		}
	}
}

func TestObserverLabelsUngrantedToolsUnknown(t *testing.T) {
	observer := &recordingCallObserver{}
	g, _ := newTestGateway(t, Options{Observer: observer}, inlineTool(getTool, "alpha", "get", true))
	for i := 0; i < 20; i++ {
		if _, err := begin(g, fmt.Sprintf("made_up_%d", i), 1, i+1, `{}`); statusOf(err) != http.StatusForbidden {
			t.Fatalf("ungranted tool error = %v", err)
		}
	}
	for _, call := range observer.calls {
		if call != "unknown:denied" {
			t.Fatalf("observed %v, want only unknown:denied", observer.calls)
		}
	}
	if len(observer.calls) != 20 {
		t.Fatalf("observed %d calls, want 20", len(observer.calls))
	}
}

func TestObserverSeesRefusals(t *testing.T) {
	manifests := func() []plugin.Manifest {
		return []plugin.Manifest{{
			Name:   "alpha",
			Limits: plugin.Limits{Rate: &plugin.Rate{Requests: 1, Interval: "1m", Burst: 1}},
		}}
	}
	observer := &recordingCallObserver{}
	g, _ := newTestGateway(t, Options{Manifests: manifests, MaxInflight: 1, Observer: observer},
		inlineTool(getTool, "alpha", "get", true))
	if _, err := begin(g, getTool, 1, 1, `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := begin(g, getTool, 1, 2, `{}`); statusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("second begin error = %v", err)
	}
	want := []string{getTool + ":rate_limited"}
	if len(observer.calls) != 1 || observer.calls[0] != want[0] {
		t.Fatalf("observed %v, want %v", observer.calls, want)
	}
}
