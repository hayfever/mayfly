package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/agents"
	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/project"
	"github.com/hayfever/mayfly/internal/runner"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	store, err := artifact.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime.Image = "example/agent:1"
	return &Manager{
		cfg:        cfg,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:      store,
		hub:        hub.New(100),
		records:    NewMemoryRecords(),
		sessions:   map[string]*instance{},
		tokenIndex: map[string]string{},
		history:    map[string]model.Session{},
		artifacts:  map[string]model.ArtifactInfo{},
	}
}

func TestPrepareAttachesImagePullSecrets(t *testing.T) {
	m := testManager(t)
	m.cfg.Runtime.ImagePullSecrets = []string{"registry-credentials", " "}
	inst := &instance{
		sess: model.Session{ID: "abc123"},
		runtimeCfg: sessioncfg.Config{
			Version:      sessioncfg.Version,
			SessionID:    "abc123",
			Model:        "model",
			PlatformURL:  "http://mayfly:8080",
			LLMBaseURL:   "http://mayfly:8080/internal/v1/llm",
			WorkspaceDir: "/workspace",
			ArtifactsDir: "/var/lib/mayfly/artifacts",
			MaxSteps:     1,
		},
	}
	spec, err := m.prepare(context.Background(), inst, "mayfly-abc123", "token", 30*time.Minute, "example/agent:1", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pod := spec.Pod()
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "registry-credentials" {
		t.Fatalf("image pull secrets = %v", pod.Spec.ImagePullSecrets)
	}
}

func TestRuntimeConnectedMarksSessionRunning(t *testing.T) {
	m := testManager(t)
	inst := &instance{
		sess:      model.Session{ID: "s1", Status: model.StatusPending},
		tokenHash: hashToken("token"),
	}
	m.sessions["s1"] = inst
	m.tokenIndex[inst.tokenHash] = "s1"

	m.RuntimeConnected("s1")
	sess, ok := m.Get("s1")
	if !ok {
		t.Fatal("session disappeared")
	}
	if sess.Status != model.StatusRunning || sess.StartedAt == nil {
		t.Fatalf("status = %s started = %v", sess.Status, sess.StartedAt)
	}
	m.RuntimeConnected("s1")
	running := 0
	for _, ev := range m.hub.EventsAfter("s1", 0) {
		if ev.Type == model.EventStatus && ev.Text == "Session running." {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("running events = %d, want 1", running)
	}
}

func TestReserveEnforcesMaxConcurrent(t *testing.T) {
	m := testManager(t)
	m.cfg.Sessions.MaxConcurrent = 1
	first := &instance{sess: model.Session{ID: "a"}, tokenHash: hashToken("a")}

	m.mu.Lock()
	err := m.reserveLocked(first)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	err = m.reserveLocked(&instance{sess: model.Session{ID: "b"}, tokenHash: hashToken("b")})
	m.mu.Unlock()
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("second reservation error = %v, want ErrCapacity", err)
	}

	first.mu.Lock()
	first.finalized = true
	first.mu.Unlock()
	m.mu.Lock()
	err = m.reserveLocked(&instance{sess: model.Session{ID: "c"}, tokenHash: hashToken("c")})
	m.mu.Unlock()
	if err != nil {
		t.Fatalf("finalized sessions must free capacity: %v", err)
	}
}

func TestReapForgetsFinishedSessionsAfterRetention(t *testing.T) {
	m := testManager(t)
	m.cfg.Sessions.Retention = config.Duration(30 * time.Minute)
	m.hub.Append("old", model.Event{Type: model.EventSystem, Text: "finished"})
	m.hub.Append("new", model.Event{Type: model.EventSystem, Text: "just closed"})

	oldClosed := time.Now().UTC().Add(-2 * time.Hour)
	recentClosed := time.Now().UTC()
	m.sessions["old"] = &instance{
		sess:      model.Session{ID: "old", Status: model.StatusCompleted, ClosedAt: &oldClosed},
		finalized: true,
		tokenHash: "h",
	}
	m.sessions["new"] = &instance{
		sess:      model.Session{ID: "new", Status: model.StatusCompleted, ClosedAt: &recentClosed},
		finalized: true,
		tokenHash: "h2",
	}
	m.tokenIndex["h"] = "old"

	m.reap()

	if _, ok := m.Get("old"); ok {
		t.Fatal("session past retention was kept")
	}
	if m.hub.LastSeq("old") != 0 {
		t.Fatal("hub stream was not removed")
	}
	if _, ok := m.tokenIndex["h"]; ok {
		t.Fatal("token survived session removal")
	}
	if _, ok := m.Get("new"); !ok {
		t.Fatal("session inside retention was removed")
	}
}

func TestReadyFollowsLifecycleStateOnly(t *testing.T) {
	m := testManager(t)
	if err := m.Ready(); err == nil {
		t.Fatal("ready before start")
	}
	m.started.Store(true)
	if err := m.Ready(); err != nil {
		t.Fatalf("ready after start: %v", err)
	}
	// The artifact store is deliberately not part of readiness: it can be
	// replaced while the process keeps serving live sessions.
	m.store = nil
	if err := m.Ready(); err != nil {
		t.Fatalf("store state leaked into readiness: %v", err)
	}
	m.started.Store(false)
	if err := m.Ready(); err == nil {
		t.Fatal("ready after shutdown")
	}
}

func TestArtifactSurvivesSessionRetention(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()
	payload := []byte("retained bundle")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	key := "sessions/s1/bundle.tar.gz"
	if _, err := m.store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), digest, "application/gzip"); err != nil {
		t.Fatal(err)
	}
	m.setArtifact("s1", model.ArtifactInfo{Key: key, Size: int64(len(payload)), SHA256: digest})

	closed := time.Now().UTC().Add(-time.Hour)
	m.cfg.Sessions.Retention = config.Duration(time.Minute)
	m.sessions["s1"] = &instance{
		sess:      model.Session{ID: "s1", Status: model.StatusCompleted, ClosedAt: &closed},
		finalized: true,
	}

	if sess, ok := m.Get("s1"); !ok || sess.Artifact == nil || sess.Artifact.Key != key {
		t.Fatalf("live session lost its artifact: %+v", sess)
	}

	m.reap()
	if _, ok := m.Get("s1"); ok {
		t.Fatal("session past retention was kept")
	}
	info, ok := m.ArtifactInfo("s1")
	if !ok || info.Key != key {
		t.Fatalf("artifact metadata lost with the session: %+v", info)
	}
	rc, size, err := m.OpenArtifact(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(payload)) || !bytes.Equal(got, payload) {
		t.Fatalf("download mismatch: size=%d body=%q", size, got)
	}
}

type nopRunner struct{}

func (nopRunner) Start(context.Context, runner.Spec) (runner.Handle, error) {
	return runner.Handle{}, nil
}

func (nopRunner) Stop(context.Context, runner.Handle, bool) error { return nil }

func (nopRunner) Logs(context.Context, runner.Handle, int64) (string, error) {
	return "", nil
}

func TestReapFailsPendingSessionThatNeverConnects(t *testing.T) {
	m := testManager(t)
	m.runner = nopRunner{}
	m.cfg.Sessions.ConnectTimeout = config.Duration(time.Minute)
	m.cfg.Sessions.Retention = config.Duration(time.Hour)
	m.cfg.Sessions.IdleTimeout = config.Duration(time.Hour)
	old := time.Now().UTC().Add(-5 * time.Minute)
	token := "stuck"
	m.sessions["p1"] = &instance{
		sess: model.Session{
			ID:           "p1",
			Status:       model.StatusPending,
			Backend:      "local",
			ExpiresAt:    time.Now().UTC().Add(time.Hour),
			LastActivity: old,
		},
		tokenHash:  hashToken(token),
		launchedAt: old,
	}
	m.tokenIndex[hashToken(token)] = "p1"
	// A session launched now is still inside the connect window.
	m.sessions["p2"] = &instance{
		sess: model.Session{
			ID:           "p2",
			Status:       model.StatusPending,
			Backend:      "local",
			ExpiresAt:    time.Now().UTC().Add(time.Hour),
			LastActivity: time.Now().UTC(),
		},
		launchedAt: time.Now().UTC(),
	}

	m.reap()

	stuck, ok := m.Get("p1")
	if !ok {
		t.Fatal("session vanished")
	}
	if stuck.Status != model.StatusFailed {
		t.Fatalf("stuck session status = %s, want failed", stuck.Status)
	}
	if fresh, _ := m.Get("p2"); fresh.Status != model.StatusPending {
		t.Fatalf("fresh session status = %s, want pending", fresh.Status)
	}
}

// TestCreateMarksResumable checks the runtime gets the signal behavior that
// matches where its workspace lives: local restates in place, Kubernetes needs
// the persistent claim.
func TestCreateMarksResumable(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	m.runner = nopRunner{}
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}

	local, err := m.Create(ctx, CreateOptions{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	inst, ok := m.instance(local.ID)
	if !ok {
		t.Fatal("created session is not registered")
	}
	if !inst.runtimeCfg.Resumable {
		t.Fatalf("local backend Resumable = false, want true")
	}

	m.cfg.Runtime.Backend = "kubernetes"
	m.cfg.Sessions.PersistentWorkspace = new(false)
	remote, err := m.Create(ctx, CreateOptions{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	inst, ok = m.instance(remote.ID)
	if !ok {
		t.Fatal("created session is not registered")
	}
	if inst.runtimeCfg.Resumable {
		t.Fatalf("kubernetes without a persistent workspace Resumable = true, want false")
	}

	m.cfg.Sessions.PersistentWorkspace = new(true)
	persistent, err := m.Create(ctx, CreateOptions{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	inst, _ = m.instance(persistent.ID)
	if !inst.runtimeCfg.Resumable {
		t.Fatalf("kubernetes with a persistent workspace Resumable = false, want true")
	}

	// max_restarts: 0 starts no replacement, so nothing would resume a turn
	// a signal interrupted.
	m.cfg.Sessions.MaxRestarts = 0
	noRestarts, err := m.Create(ctx, CreateOptions{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	inst, _ = m.instance(noRestarts.ID)
	if inst.runtimeCfg.Resumable {
		t.Fatalf("max_restarts 0 Resumable = true, want false")
	}
}

// TestCreateIsIdempotentPerKey checks a repeated Create with the same key
// returns the first session, even when the session cap is full, and that a
// different key or no key starts a new one.
func TestCreateIsIdempotentPerKey(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	m.runner = nopRunner{}
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}
	m.cfg.Sessions.MaxConcurrent = 1

	first, err := m.Create(ctx, CreateOptions{Prompt: "hello", IdempotencyKey: "job-1"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.Create(ctx, CreateOptions{Prompt: "hello again", IdempotencyKey: "job-1"})
	if err != nil {
		t.Fatalf("repeated create: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("repeated create started %q, want %q", again.ID, first.ID)
	}
	if n := len(m.List()); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
	if inst, _ := m.instance(first.ID); inst.runtimeCfg.IdempotencyKey != "job-1" {
		t.Fatalf("runtime config key = %q", inst.runtimeCfg.IdempotencyKey)
	}
	if _, err := m.Create(ctx, CreateOptions{Prompt: "other", IdempotencyKey: "job-2"}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("new key at capacity err = %v, want ErrCapacity", err)
	}
}

// fakeGateway records the grants the manager registers and drops.
type fakeGateway struct {
	mu     sync.Mutex
	grants map[string]gateway.Grant
	drops  []string
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{grants: map[string]gateway.Grant{}}
}

func (f *fakeGateway) Register(grant gateway.Grant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grants[grant.SessionID] = grant
}

func (f *fakeGateway) Drop(session string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.grants, session)
	f.drops = append(f.drops, session)
}

func (f *fakeGateway) grant(session string) (gateway.Grant, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	grant, ok := f.grants[session]
	return grant, ok
}

func (f *fakeGateway) dropped() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.drops...)
}

// parseManifest parses and validates one manifest as the bundle loader does.
func parseManifest(t *testing.T, dir, raw string, requireImage bool) plugin.Manifest {
	t.Helper()
	manifest, err := plugin.Parse([]byte(raw), filepath.Join("plugins", "manifest", plugin.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Validate(plugin.ValidateOptions{RequireImage: requireImage}); err != nil {
		t.Fatal(err)
	}
	manifest.Dir = dir
	return manifest
}

// pluginManager returns a manager whose bundle holds these manifests and one
// agent that selects them all.
func pluginManager(t *testing.T, manifests ...plugin.Manifest) *Manager {
	t.Helper()
	m := testManager(t)
	m.cfg.LLM.Model = "test-model"
	m.cfg.Server.InternalURL = "http://mayfly:8080"
	m.stateDir = t.TempDir()
	names := make([]string, 0, len(manifests))
	bundle := &project.Bundle{Plugins: map[string]plugin.Manifest{}}
	for _, manifest := range manifests {
		bundle.Plugins[manifest.Name] = manifest
		names = append(names, manifest.Name)
	}
	agent, err := agents.Build(agents.Definition{Name: "tester", Plugins: &names}, "agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bundle.Agents = []agents.Agent{agent}
	m.bundle = bundle
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}
	return m
}

const notifyManifest = `
schemaVersion: 1
name: notify
version: 1.0.0
placement: inline
image: example/notify:1
command: /plugin/notify
args: ["serve"]
env:
  MODE: prod
secrets:
  - env: SLACK_TOKEN
    secret: mayfly-plugin-slack
    key: token
tools:
  - name: post_message
    idempotent: false
    timeout: 30s
  - name: list_channels
    idempotent: true
`

const ticketsManifest = `
schemaVersion: 1
name: tickets
placement: service
image: example/tickets:1
command: /plugin/tickets
service:
  replicas: 1
tools:
  - name: get_ticket
    idempotent: true
    timeout: 30s
`

const wordCountManifest = `
schemaVersion: 1
name: word-count
image: example/word-count:1
command: /plugin/word-count
local:
  command: ./word-count
tools:
  - name: count
    idempotent: true
    timeout: 10s
`

// TestCreateResolvesPlugins checks the runtime copy of each manifest: pod
// paths on the Kubernetes backend, the service entry trimmed to a grant, and
// the declared tools with their timeouts.
func TestCreateResolvesPlugins(t *testing.T) {
	notify := parseManifest(t, "/bundle/plugins/notify", notifyManifest, true)
	tickets := parseManifest(t, "/bundle/plugins/tickets", ticketsManifest, true)
	m := pluginManager(t, notify, tickets)
	m.runner = &fakeRunner{handle: runner.Handle{Backend: "kubernetes", SessionID: "s1"}}

	session, err := m.Create(context.Background(), CreateOptions{Prompt: "hello", Agent: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if len(session.Plugins) != 2 || session.Plugins[0] != "notify" || session.Plugins[1] != "tickets" {
		t.Fatalf("session plugins = %v", session.Plugins)
	}
	inst, ok := m.instance(session.ID)
	if !ok {
		t.Fatal("session is not registered")
	}
	plugins := inst.runtimeCfg.Plugins
	if len(plugins) != 2 {
		t.Fatalf("runtime plugins = %+v", plugins)
	}
	inline := plugins[0]
	if inline.Name != "notify" || inline.Placement != "inline" {
		t.Fatalf("inline plugin = %+v", inline)
	}
	if inline.Dir != k8s.PluginsDir+"/notify" || inline.SecretsDir != k8s.PluginSecretsDir+"/notify" {
		t.Fatalf("inline paths = %q %q", inline.Dir, inline.SecretsDir)
	}
	if inline.Image != "example/notify:1" || inline.Command != "/plugin/notify" {
		t.Fatalf("inline launch = %q %q", inline.Image, inline.Command)
	}
	if len(inline.Args) != 1 || inline.Args[0] != "serve" || inline.Workdir != "plugin" {
		t.Fatalf("inline args/workdir = %v %q", inline.Args, inline.Workdir)
	}
	if inline.MaxRestarts != 2 || inline.StderrBytes != 1<<20 {
		t.Fatalf("inline restarts/stderr = %d %d", inline.MaxRestarts, inline.StderrBytes)
	}
	if len(inline.Secrets) != 1 || inline.Secrets[0].Secret != "mayfly-plugin-slack" || inline.Secrets[0].Key != "token" {
		t.Fatalf("inline secrets = %+v", inline.Secrets)
	}
	if len(inline.Tools) != 2 || inline.Tools[0].Name != "post_message" || inline.Tools[0].TimeoutSeconds != 30 {
		t.Fatalf("inline tools = %+v", inline.Tools)
	}
	if inline.Env["MODE"] != "prod" {
		t.Fatalf("inline env = %v", inline.Env)
	}

	service := plugins[1]
	if service.Name != "tickets" || service.Placement != "service" {
		t.Fatalf("service plugin = %+v", service)
	}
	if service.Dir != "" || service.Image != "" || service.Command != "" || service.Socket != "" {
		t.Fatalf("service plugin carries launch fields: %+v", service)
	}
	if len(service.Tools) != 1 || service.Tools[0].Name != "get_ticket" || service.InitTimeoutSeconds != 30 {
		t.Fatalf("service grant fields = %+v", service)
	}
	if service.MaxOutputBytes != plugin.MaxOutputBytes {
		t.Fatalf("service max output = %d", service.MaxOutputBytes)
	}

	// A request that selects an MCP server takes the plugins its agent names,
	// unchanged.
	m.mcpByName = map[string]config.MCPServerConfig{
		"search": {Name: "search", Transport: "stdio", Command: "/usr/bin/search"},
	}
	withMCP, err := m.Create(context.Background(), CreateOptions{
		Prompt:     "hello",
		Agent:      "tester",
		MCPServers: []string{"search"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(withMCP.MCPServers) != 1 || withMCP.MCPServers[0] != "search" {
		t.Fatalf("session MCP servers = %v", withMCP.MCPServers)
	}
	if len(withMCP.Plugins) != 2 || withMCP.Plugins[0] != "notify" || withMCP.Plugins[1] != "tickets" {
		t.Fatalf("plugins changed with mcp_servers = %v", withMCP.Plugins)
	}
}

// TestCreatePluginSessionJSONHasNoSecrets checks the document the runtime
// mounts: Kubernetes paths, and no secret value.
func TestCreatePluginSessionJSONHasNoSecrets(t *testing.T) {
	const sentinel = "dont-leak-me-7f3c"
	t.Setenv("SLACK_TOKEN", sentinel)
	notify := parseManifest(t, "/bundle/plugins/notify", notifyManifest, true)
	m := pluginManager(t, notify)
	runner := &fakeRunner{handle: runner.Handle{Backend: "kubernetes", SessionID: "s1"}}
	m.runner = runner

	session, err := m.Create(context.Background(), CreateOptions{Prompt: "hello", Agent: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	starts := runner.specs()
	if len(starts) != 1 {
		t.Fatalf("starts = %d", len(starts))
	}
	raw, err := json.Marshal(starts[0].Runtime)
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)
	if strings.Contains(document, sentinel) {
		t.Fatalf("session.json holds a secret value:\n%s", document)
	}
	if !strings.Contains(document, k8s.PluginsDir+"/notify") || !strings.Contains(document, k8s.PluginSecretsDir+"/notify") {
		t.Fatalf("session.json is missing the Kubernetes plugin paths:\n%s", document)
	}
	if strings.Contains(document, k8s.PluginSecretsDir+"/notify/SLACK_TOKEN") {
		t.Fatalf("session.json names a secret file:\n%s", document)
	}
	if _, ok := m.instance(session.ID); !ok {
		t.Fatal("session is not registered")
	}
}

// TestCreateRegistersGrant checks the grant holds exactly the declared tools,
// fixed before the container starts.
func TestCreateRegistersGrant(t *testing.T) {
	notify := parseManifest(t, "/bundle/plugins/notify", notifyManifest, true)
	tickets := parseManifest(t, "/bundle/plugins/tickets", ticketsManifest, true)
	m := pluginManager(t, notify, tickets)
	m.runner = &fakeRunner{handle: runner.Handle{Backend: "kubernetes", SessionID: "s1"}}
	registry := newFakeGateway()
	m.SetGateway(registry)

	session, err := m.Create(context.Background(), CreateOptions{Prompt: "hello", Agent: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	grant, ok := registry.grant(session.ID)
	if !ok {
		t.Fatal("create did not register a grant")
	}
	want := map[string]string{
		"plugin_notify_post_message":  "notify",
		"plugin_notify_list_channels": "notify",
		"plugin_tickets_get_ticket":   "tickets",
	}
	if len(grant.Tools) != len(want) {
		t.Fatalf("grant tools = %+v", grant.Tools)
	}
	for name, owner := range want {
		tool, ok := grant.Tools[name]
		if !ok {
			t.Fatalf("grant is missing %q: %+v", name, grant.Tools)
		}
		if tool.Owner != owner {
			t.Fatalf("%s owner = %q, want %q", name, tool.Owner, owner)
		}
	}
	if grant.Tools["plugin_notify_post_message"].Placement != gateway.Inline {
		t.Fatalf("notify placement = %q", grant.Tools["plugin_notify_post_message"].Placement)
	}
	if grant.Tools["plugin_tickets_get_ticket"].Placement != gateway.Service {
		t.Fatalf("tickets placement = %q", grant.Tools["plugin_tickets_get_ticket"].Placement)
	}
	if grant.Tools["plugin_notify_post_message"].Idempotent {
		t.Fatal("post_message must be non-idempotent")
	}
	if grant.Tools["plugin_notify_post_message"].Timeout != 30*time.Second {
		t.Fatalf("post_message timeout = %s", grant.Tools["plugin_notify_post_message"].Timeout)
	}
	if grant.Tools["plugin_notify_list_channels"].Timeout != 2*time.Minute {
		t.Fatalf("list_channels timeout = %s, want the plugin call timeout", grant.Tools["plugin_notify_list_channels"].Timeout)
	}
	if grant.SidecarKey != nil {
		t.Fatal("a session without sidecars must not carry a sidecar key")
	}
}

// TestFinalizeDropsLedger checks finalize forgets the grant and removes the
// session's ledger records with the journal.
func TestFinalizeDropsLedger(t *testing.T) {
	notify := parseManifest(t, "/bundle/plugins/notify", notifyManifest, true)
	m := pluginManager(t, notify)
	m.runner = &fakeRunner{handle: runner.Handle{Backend: "kubernetes", SessionID: "s1"}}
	registry := newFakeGateway()
	m.SetGateway(registry)
	ledger := journal.NewMemory()
	m.journal = ledger

	session, err := m.Create(context.Background(), CreateOptions{Prompt: "hello", Agent: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	key := session.ID + "/1/1"
	if _, _, err := ledger.Begin(context.Background(), journal.Call{
		Key:        key,
		SessionID:  session.ID,
		Turn:       1,
		Step:       1,
		Tool:       "plugin_notify_post_message",
		ArgsSHA256: "aa",
	}); err != nil {
		t.Fatal(err)
	}
	inst, ok := m.instance(session.ID)
	if !ok {
		t.Fatal("session is not registered")
	}
	m.finalize(inst, model.StatusCompleted, "done", "")
	m.wg.Wait()

	if _, ok := registry.grant(session.ID); ok {
		t.Fatal("finalize kept the grant")
	}
	if drops := registry.dropped(); len(drops) != 1 || drops[0] != session.ID {
		t.Fatalf("drops = %v", drops)
	}
	if _, found, err := ledger.Get(context.Background(), session.ID, key); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("finalize kept the ledger record")
	}
}

// TestLocalMissingPluginSecret checks a missing local secret fails before a
// session starts, both for a webhook's Validate and for Create.
func TestLocalMissingPluginSecret(t *testing.T) {
	const envName = "WP7_TEST_UNSET_PLUGIN_TOKEN"
	t.Setenv(envName, "")
	if err := os.Unsetenv(envName); err != nil {
		t.Fatal(err)
	}
	raw := strings.Replace(notifyManifest, "SLACK_TOKEN", envName, 1)
	notify := parseManifest(t, t.TempDir(), raw, false)
	m := pluginManager(t, notify)
	m.cfg.Runtime.Backend = "local"
	m.runner = &fakeRunner{handle: runner.Handle{Backend: "local", SessionID: "s1"}}

	want := `plugin "notify": environment variable ` + envName + " is not set"
	err := m.Validate(CreateOptions{Prompt: "hello", Agent: "tester"})
	if err == nil || err.Error() != want {
		t.Fatalf("Validate error = %v, want %q", err, want)
	}
	session, err := m.Create(context.Background(), CreateOptions{Prompt: "hello", Agent: "tester"})
	if err == nil || err.Error() != want {
		t.Fatalf("Create error = %v, want %q", err, want)
	}
	if session != nil {
		t.Fatalf("Create started a session: %+v", session)
	}
	if n := len(m.sessions); n != 0 {
		t.Fatalf("sessions registered = %d", n)
	}
}

// TestLocalSidecarRunsInline checks the local backend resolves a sidecar
// plugin as inline, with its secret values read from the mayflyd environment.
func TestLocalSidecarRunsInline(t *testing.T) {
	const envName = "WP7_TEST_SIDECAR_TOKEN"
	t.Setenv(envName, "value")
	sidecar := `
schemaVersion: 1
name: notify
placement: sidecar
image: example/notify:1
command: /plugin/notify
secrets:
  - env: ` + envName + `
    secret: mayfly-plugin-slack
    key: token
  - env: WP7_TEST_SIDECAR_OTHER
    secret: mayfly-plugin-slack
    key: other
tools:
  - name: post_message
sidecar:
  resources:
    requests:
      cpu: 10m
    limits:
      memory: 64Mi
  workspace: read
`
	t.Setenv("WP7_TEST_SIDECAR_OTHER", "other")
	manifest := parseManifest(t, t.TempDir(), sidecar, false)
	m := pluginManager(t, manifest)
	m.cfg.Runtime.Backend = "local"
	runner := &fakeRunner{handle: runner.Handle{Backend: "local", SessionID: "s1"}}
	m.runner = runner
	registry := newFakeGateway()
	m.SetGateway(registry)

	session, err := m.Create(context.Background(), CreateOptions{Prompt: "hello", Agent: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	starts := runner.specs()
	if len(starts) != 1 {
		t.Fatalf("starts = %d", len(starts))
	}
	plugin := starts[0].Runtime.Plugins[0]
	if plugin.Placement != "inline" {
		t.Fatalf("placement = %q, want inline on the local backend", plugin.Placement)
	}
	if plugin.Socket != "" {
		t.Fatalf("socket = %q, want none for an inline plugin", plugin.Socket)
	}
	if plugin.SecretsDir != filepath.Join(m.stateDir, "sessions", session.ID, "plugin-secrets", "notify") {
		t.Fatalf("secrets dir = %q", plugin.SecretsDir)
	}
	if len(plugin.Secrets) != 2 || plugin.Secrets[0].Secret != "" || plugin.Secrets[0].Key != "" {
		t.Fatalf("local secrets = %+v", plugin.Secrets)
	}
	if plugin.MaxRestarts != 2 || plugin.Workdir != "plugin" || plugin.StderrBytes != 1<<20 {
		t.Fatalf("inline defaults = %+v", plugin)
	}
	secrets := starts[0].PluginSecrets["notify"]
	if secrets[envName] != "value" || secrets["WP7_TEST_SIDECAR_OTHER"] != "other" {
		t.Fatalf("plugin secret values = %v", secrets)
	}
	grant, ok := registry.grant(session.ID)
	if !ok {
		t.Fatal("create did not register a grant")
	}
	tool, ok := grant.Tools["plugin_notify_post_message"]
	if !ok || tool.Placement != gateway.Inline {
		t.Fatalf("grant tool = %+v", tool)
	}
}

// TestShutdownStopsAtItsDeadline keeps a stop that outlives the budget from
// holding the process past the pod's grace period.
func TestShutdownStopsAtItsDeadline(t *testing.T) {
	m := testManager(t)
	m.journal = journal.NewMemory()
	m.runner = &stopRecorder{}
	inst := journalInstance(t, m)
	inst.sess.Status = model.StatusRunning
	inst.handle = runner.Handle{Backend: "kubernetes", SessionID: "s1", PodName: "mayfly-s1"}
	m.finalize(inst, model.StatusCompleted, "closed", "")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	m.Shutdown(ctx)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Shutdown took %v with a 20ms budget", elapsed)
	}
}
