// Package session owns the session lifecycle: creation, thread fan-out,
// artifact export, timeout handling, and teardown.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

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
	"github.com/hayfever/mayfly/internal/skills"
	"github.com/hayfever/mayfly/internal/version"
)

// ErrNotFound is returned for unknown sessions.
var ErrNotFound = errors.New("session not found")

// ErrClosed is returned when a session no longer accepts messages.
var ErrClosed = errors.New("session is closed")

// ErrUnauthorized is returned for a missing or wrong session token.
var ErrUnauthorized = errors.New("unauthorized")

// ErrCapacity is returned when sessions.max_concurrent is reached.
var ErrCapacity = errors.New("session capacity reached")

const maxPromptBytes = 64 << 10
const maxMessageBytes = 32 << 10
const podTerminalGrace = 20 * time.Second

// sequenceBlock is how far ahead the control plane reserves hub sequence
// numbers. One journal write covers a block of events, so the reservation
// stays cheap without ever handing out a number twice across a restart.
const sequenceBlock = int64(1000)

// CreateOptions describes a new session request.
type CreateOptions struct {
	Prompt     string
	Title      string
	Model      string
	TTL        time.Duration
	Skills     []string
	MCPServers []string
	Agent      string
	Trigger    model.TriggerInfo
	// Artifacts are inputs the caller already stored. The control plane never
	// fetches them; it only checks the reference and lets the session read it.
	Artifacts []ArtifactRef
	// IdempotencyKey makes Create safe to repeat. While a session created
	// with the same key is still registered, Create returns that session
	// instead of starting another one. Queue handlers pass the job ID so a
	// crash between Create and Ack cannot start a second session.
	IdempotencyKey string
}

// ArtifactRef is one stored input a session may read.
type ArtifactRef struct {
	Key         string
	SHA256      string
	Bytes       int64
	ContentType string
}

type instance struct {
	mu               sync.Mutex
	sess             model.Session
	token            string
	tokenHash        string
	runtimeCfg       sessioncfg.Config
	handle           runner.Handle
	image            string
	command          []string
	args             []string
	env              map[string]string
	closeReason      string
	closeRequestedAt time.Time
	finalized        bool
	resourcesDeleted bool
	launchedAt       time.Time
	podNotReadySince time.Time
	podTerminalSince time.Time
	podMissingSince  time.Time
	podDeletingSince time.Time
	restarts         int
	restarting       bool
	// artifacts are the inputs this session may read; the map keys are the
	// only keys the internal artifact endpoint serves for this session.
	artifacts map[string]sessioncfg.Artifact
	// idempotencyKey is CreateOptions.IdempotencyKey. The manager reads and
	// writes it under Manager.mu.
	idempotencyKey string
	// verifiedArtifacts records the artifact keys whose stored bytes already
	// matched their declared SHA-256.
	verifiedArtifacts map[string]bool
	// sidecarKey signs this session's sidecar call tokens. Create generates
	// it and adoption reads it back from the session Secret, so a replacement
	// pod and this control plane sign the same tokens. It never reaches the
	// agent container.
	sidecarKey []byte
	stalePods  []string
	// seqFloor is the highest sequence this control plane durably reserved
	// for the session. The hub may hand out numbers up to it after a restart
	// without touching the journal.
	seqFloor atomic.Int64
	// journalMu orders AppendJournal's turn check with its write, so two
	// containers of one session cannot both pass the check.
	journalMu sync.Mutex
	// journalBytes counts what this session journaled in this control-plane
	// lifetime, for journal.max_session_bytes. It is guarded by journalMu.
	journalBytes int64
}

// Manager is the control plane session registry and lifecycle engine.
type Manager struct {
	cfg     *config.Config
	bundle  *project.Bundle
	logger  *slog.Logger
	store   artifact.Store
	hub     *hub.Hub
	journal journal.Durable
	runner  runner.Runner
	kube    *k8s.Client

	skills       []sessioncfg.Skill
	skillByName  map[string]sessioncfg.Skill
	mcpByName    map[string]config.MCPServerConfig
	defaultSkill []string
	defaultMCP   []string
	agents       []agents.Agent
	agentByName  map[string]agents.Agent
	defaultAgent string
	stateDir     string
	gateway      GrantRegistry

	// records keeps session records and the artifact index across restarts.
	records RecordStore
	// adoptPending holds the session IDs the last adoption pass saw but could
	// not rebuild, and adoptIncomplete reports a pass that failed before it
	// saw every session. A record for such an ID is not marked failed until
	// adoption resolves it.
	adoptPending    map[string]struct{}
	adoptIncomplete atomic.Bool

	mu         sync.Mutex
	sessions   map[string]*instance
	tokenIndex map[string]string
	// history holds finished sessions loaded from the record store. A live
	// instance always wins over its history entry.
	history map[string]model.Session
	// artifacts survives session retention so a stored bundle stays
	// downloadable after the session record is gone.
	artifacts map[string]model.ArtifactInfo

	started  atomic.Bool
	stopping bool
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// GrantRegistry is the part of the tool gateway the session manager uses:
// register a grant when a session is created or adopted, drop it at finalize.
// A nil registry means no tool routes are in use, as in tests without a
// gateway.
type GrantRegistry interface {
	Register(gateway.Grant)
	Drop(session string)
}

// SetGateway attaches the tool gateway. Call it before Start so adopted
// sessions get their grants.
func (m *Manager) SetGateway(g GrantRegistry) { m.gateway = g }

// New builds a manager. kube may be nil for the local backend.
func New(cfg *config.Config, bundle *project.Bundle, store artifact.Store, h *hub.Hub, js journal.Durable, r runner.Runner, kube *k8s.Client, logger *slog.Logger) (*Manager, error) {
	if js == nil {
		return nil, errors.New("journal store is required")
	}
	sources := make([]skills.Source, 0, len(cfg.Skills.Sources))
	for _, source := range cfg.Skills.Sources {
		strip := 1
		if source.StripComponents != nil {
			strip = *source.StripComponents
		}
		sourceURL := source.ResolvedURL
		if sourceURL == "" {
			sourceURL = source.URL
		}
		sources = append(sources, skills.Source{
			Name:            source.Name,
			URL:             sourceURL,
			SHA256:          source.SHA256,
			Subdir:          source.Subdir,
			StripComponents: strip,
			Headers:         source.ResolvedHeaders,
			Timeout:         source.Timeout.Std(),
		})
	}
	loaded, err := skills.Load(cfg.Skills.Dirs, sources)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		cfg:         cfg,
		bundle:      bundle,
		logger:      logger,
		store:       store,
		hub:         h,
		journal:     js,
		runner:      r,
		kube:        kube,
		skills:      loaded,
		skillByName: map[string]sessioncfg.Skill{},
		mcpByName:   map[string]config.MCPServerConfig{},
		records:     NewMemoryRecords(),
		sessions:    map[string]*instance{},
		tokenIndex:  map[string]string{},
		history:     map[string]model.Session{},
		artifacts:   map[string]model.ArtifactInfo{},
	}
	for _, s := range loaded {
		m.skillByName[s.Name] = s
	}
	if len(cfg.Skills.Default) == 0 {
		for _, s := range loaded {
			m.defaultSkill = append(m.defaultSkill, s.Name)
		}
	} else {
		for _, name := range cfg.Skills.Default {
			if _, ok := m.skillByName[name]; !ok {
				return nil, fmt.Errorf("skills.default references unknown skill %q", name)
			}
			m.defaultSkill = append(m.defaultSkill, name)
		}
	}
	for _, s := range cfg.MCP.Servers {
		m.mcpByName[s.Name] = s
		if !s.OptIn {
			m.defaultMCP = append(m.defaultMCP, s.Name)
		}
	}
	if err := m.loadAgents(); err != nil {
		return nil, err
	}
	if cfg.Runtime.Backend == "local" {
		abs, err := filepath.Abs(cfg.Runtime.StateDir)
		if err != nil {
			return nil, err
		}
		m.stateDir = abs
	}
	return m, nil
}

// Start launches maintenance loops and adopts existing Kubernetes pods.
func (m *Manager) Start(ctx context.Context) error {
	m.ctx, m.cancel = context.WithCancel(ctx)
	var replacements []*instance
	if m.kube != nil {
		var err error
		replacements, err = m.adopt(ctx)
		if err != nil {
			m.logger.Warn("adopt existing sessions", "error", err)
		}
	}
	// Adoption rebuilds live sessions first, so a record it owns is not
	// reported as interrupted. A record this build cannot read stops startup:
	// serving a partial list would hide sessions an operator can still see.
	if err := m.loadRecords(ctx); err != nil {
		m.cancel()
		return err
	}
	m.settleInterrupted()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.maintenance()
	}()
	m.started.Store(true)
	// A session that lost its pod has nothing for the poller to notice, so
	// start its replacement once dispatch accepts work.
	for _, inst := range replacements {
		m.restartSoon(inst, "pod missing at control-plane start")
	}
	return nil
}

// Shutdown stops maintenance and local agent processes.
func (m *Manager) Shutdown(ctx context.Context) {
	m.started.Store(false)
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	if m.cfg.Runtime.Backend == "local" {
		for _, inst := range m.snapshot() {
			inst.mu.Lock()
			h := inst.handle
			inst.mu.Unlock()
			if h.Backend == "local" {
				_ = m.runner.Stop(ctx, h, true)
			}
		}
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// The pod is about to be killed. Cleanup that has not finished keeps
		// running until the process exits, and the next control plane adopts
		// or collects whatever it left behind.
		m.logger.Warn("shutdown budget reached before cleanup finished")
	}
}

// dispatch runs work in the background and keeps Shutdown waiting for it. It
// refuses work once shutdown has started so a WaitGroup Add never races Wait.
func (m *Manager) dispatch(fn func()) bool {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return false
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		fn()
	}()
	return true
}

// request is a validated session request with its references resolved.
type request struct {
	prompt     string
	ttl        time.Duration
	agent      agents.Agent
	model      string
	skills     []sessioncfg.Skill
	skillNames []string
	mcp        []sessioncfg.MCPServer
	mcpNames   []string
	plugins    []sessioncfg.Plugin
	artifacts  []sessioncfg.Artifact
}

// resolveRequest checks opts and resolves every reference it names. It has no
// side effects, so it can run before anything is created.
func (m *Manager) resolveRequest(opts CreateOptions) (request, error) {
	prompt := strings.TrimSpace(opts.Prompt)
	if prompt == "" {
		return request{}, errors.New("prompt is required")
	}
	if len(prompt) > maxPromptBytes {
		return request{}, fmt.Errorf("prompt exceeds %d bytes", maxPromptBytes)
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = m.cfg.Sessions.DefaultTTL.Std()
	}
	if ttl > m.cfg.Sessions.MaxTTL.Std() {
		ttl = m.cfg.Sessions.MaxTTL.Std()
	}
	agent, err := m.resolveAgent(opts.Agent)
	if err != nil {
		return request{}, err
	}
	modelName := opts.Model
	if modelName == "" {
		modelName = agent.Model
	}
	if modelName == "" {
		modelName = m.cfg.LLM.Model
	}
	if !m.modelAllowed(modelName) {
		return request{}, fmt.Errorf("model %q is not allowed", modelName)
	}
	requestedSkills := opts.Skills
	if len(requestedSkills) == 0 && agent.Skills != nil {
		requestedSkills = *agent.Skills
	}
	resolvedSkills, skillNames, err := m.resolveSkills(agent, requestedSkills)
	if err != nil {
		return request{}, err
	}
	requestedMCP := opts.MCPServers
	if len(requestedMCP) == 0 && agent.MCPServers != nil {
		requestedMCP = *agent.MCPServers
	}
	resolvedMCP, mcpNames, err := m.resolveMCP(agent, requestedMCP)
	if err != nil {
		return request{}, err
	}
	resolvedPlugins, err := m.resolvePlugins(agent)
	if err != nil {
		return request{}, err
	}
	artifacts, err := m.resolveArtifacts(opts.Artifacts)
	if err != nil {
		return request{}, err
	}
	return request{
		prompt:     prompt,
		ttl:        ttl,
		agent:      agent,
		model:      modelName,
		skills:     resolvedSkills,
		skillNames: skillNames,
		mcp:        resolvedMCP,
		mcpNames:   mcpNames,
		plugins:    resolvedPlugins,
		artifacts:  artifacts,
	}, nil
}

// Validate reports whether Create would accept opts, without starting
// anything. Webhooks call it before queueing so a bad request gets 400 while
// the sender is still listening.
func (m *Manager) Validate(opts CreateOptions) error {
	_, err := m.resolveRequest(opts)
	return err
}

// Create starts a new session.
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (*model.Session, error) {
	req, err := m.resolveRequest(opts)
	if err != nil {
		return nil, err
	}
	prompt, ttl, agent, modelName := req.prompt, req.ttl, req.agent, req.model
	resolvedSkills, skillNames, resolvedMCP, mcpNames := req.skills, req.skillNames, req.mcp, req.mcpNames
	maxSteps := agent.MaxSteps
	if maxSteps <= 0 {
		maxSteps = m.cfg.Sessions.MaxSteps
	}
	maxHistory := agent.MaxHistoryMessages
	if maxHistory <= 0 {
		maxHistory = m.cfg.Sessions.MaxHistoryMessages
	}
	allowShell := agentAllowShell(agent, m.cfg)
	harness := harnessName(agent)
	image := agent.Runtime.Image
	if image == "" {
		image = m.cfg.Runtime.Image
	}
	runtimeEnv := mergeEnv(m.cfg.Runtime.Env, agent.Runtime.Env)

	now := time.Now().UTC()
	id := m.newID()
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = firstLine(prompt)
	}
	if title == "" {
		title = "Session " + id
	}
	systemPrompt, err := agent.RenderPrompt(promptData{
		SessionID: id,
		Title:     title,
		Agent:     agent.Name,
		Harness:   harness,
		Model:     modelName,
		Trigger:   opts.Trigger,
		Now:       now,
	})
	if err != nil {
		return nil, err
	}
	token := newToken()
	hash := hashToken(token)
	podName := "mayfly-" + id

	internal := strings.TrimRight(m.cfg.Server.InternalURL, "/")
	runtimeCfg := sessioncfg.Config{
		Version:            sessioncfg.Version,
		SessionID:          id,
		Title:              title,
		Prompt:             prompt,
		Agent:              agent.Name,
		Harness:            harness,
		SystemPrompt:       systemPrompt,
		RuntimeProtocol:    agent.Runtime.Protocol,
		AgentConfig:        agent.Runtime.Config,
		Model:              modelName,
		PlatformURL:        internal,
		LLMBaseURL:         internal + "/internal/v1/llm",
		LLMTimeoutSeconds:  int(m.cfg.LLM.Timeout.Std().Seconds()),
		MaxSteps:           maxSteps,
		MaxHistoryMessages: maxHistory,
		TTLSeconds:         int(ttl.Seconds()),
		IdleTimeoutSeconds: int(m.cfg.Sessions.IdleTimeout.Std().Seconds()),
		WorkspaceDir:       m.cfg.Sessions.WorkspaceDir,
		ArtifactsDir:       m.cfg.Sessions.ArtifactsDir,
		AllowShell:         allowShell,
		ShellTimeoutSecs:   int(m.cfg.Sessions.ShellTimeout.Std().Seconds()),
		ArtifactMaxBytes:   m.cfg.Artifacts.MaxBytes,
		Skills:             resolvedSkills,
		MCPServers:         resolvedMCP,
		Plugins:            req.plugins,
		Artifacts:          req.artifacts,
		CreatedAt:          now,
		AgentVersion:       version.Version,
		IdempotencyKey:     opts.IdempotencyKey,
		// max_restarts: 0 means no replacement will ever start, so a signal
		// must finalize instead of leaving the turn to one.
		Resumable: m.cfg.Sessions.MaxRestarts > 0 &&
			(m.cfg.Runtime.Backend == "local" || config.Bool(m.cfg.Sessions.PersistentWorkspace, true)),
	}

	inst := &instance{
		sess: model.Session{
			ID:           id,
			Title:        title,
			Status:       model.StatusPending,
			Prompt:       prompt,
			Agent:        agent.Name,
			Harness:      harness,
			Model:        modelName,
			Skills:       skillNames,
			MCPServers:   mcpNames,
			Plugins:      pluginNames(req.plugins),
			Trigger:      opts.Trigger,
			Backend:      m.cfg.Runtime.Backend,
			TTLSeconds:   int(ttl.Seconds()),
			CreatedAt:    now,
			ExpiresAt:    now.Add(ttl),
			LastActivity: now,
		},
		tokenHash:  hash,
		token:      token,
		artifacts:  artifactMap(req.artifacts),
		sidecarKey: newSidecarKey(req.plugins),
		runtimeCfg: runtimeCfg,
		image:      image,
		command:    append([]string(nil), agent.Runtime.Command...),
		args:       append([]string(nil), agent.Runtime.Args...),
		env:        cloneEnv(runtimeEnv),
		launchedAt: now,
	}

	if m.cfg.Runtime.Backend == "local" && agent.Runtime.Image != "" {
		m.logger.Warn("agent image is ignored by the local backend", "agent", agent.Name, "image", agent.Runtime.Image)
	}
	spec, err := m.prepare(ctx, inst, podName, token, ttl, image, agent.Runtime.Command, agent.Runtime.Args, runtimeEnv)
	if err != nil {
		return nil, err
	}
	// The local backend writes each plugin secret to a file before the agent
	// starts. A missing value already failed resolveRequest; re-reading here
	// keeps the value out of session.json and out of every log line.
	pluginSecrets, err := m.pluginSecretValues(req.plugins)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if existing := m.idempotentLocked(opts.IdempotencyKey); existing != nil {
		m.mu.Unlock()
		snapshot := m.copySession(existing)
		m.logger.Info("session create repeated; returning existing session", "session", snapshot.ID)
		return &snapshot, nil
	}
	inst.idempotencyKey = opts.IdempotencyKey
	if err := m.reserveLocked(inst); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	m.mu.Unlock()
	// The trigger's sequence becomes the first turn key, so reserve it before
	// the hub assigns it. A failed reservation must not leave the session
	// registered with unreserved keys.
	if err := m.reserveSequence(inst); err != nil {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		m.removeToken(hash)
		return nil, fmt.Errorf("reserve turn sequence: %w", err)
	}
	// The grant is in place before the pod exists, so the runtime's first call
	// finds its table.
	m.registerGrant(inst)
	m.hub.Create(id)
	m.persistInstance(inst)
	m.appendEvent(inst, model.Event{
		Type:  model.EventUserMessage,
		Actor: "trigger",
		Text:  prompt,
		Data: map[string]any{
			"kind":  opts.Trigger.Kind,
			"name":  opts.Trigger.Name,
			"event": opts.Trigger.Event,
		},
	})
	m.appendEvent(inst, model.Event{Type: model.EventStatus, Actor: "mayfly", Text: "Session created.", Data: map[string]any{"status": string(model.StatusPending)}})

	startCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	h, err := m.runner.Start(startCtx, runner.Spec{
		Backend:       m.cfg.Runtime.Backend,
		SessionID:     id,
		Namespace:     m.cfg.Sessions.Namespace,
		PodName:       podName,
		SecretName:    podName,
		ConfigMapName: podName,
		StateDir:      filepath.Join(m.stateDir, "sessions", id),
		Token:         token,
		Env:           runtimeEnv,
		Runtime:       inst.runtimeCfg,
		PluginSecrets: pluginSecrets,
		K8sSpec:       spec,
	})
	if err != nil {
		now := time.Now().UTC()
		inst.mu.Lock()
		inst.sess.Status = model.StatusFailed
		inst.sess.Error = err.Error()
		inst.sess.ClosedAt = &now
		inst.finalized = true
		inst.mu.Unlock()
		// A session that never started must not answer a retry: the caller
		// gets the error and may try again with the same key.
		m.mu.Lock()
		inst.idempotencyKey = ""
		m.mu.Unlock()
		m.removeToken(hash)
		m.dropGrant(id)
		m.persistInstance(inst)
		m.hub.Append(id, model.Event{Type: model.EventError, Actor: "mayfly", Text: "Failed to start session: " + err.Error()})
		m.hub.Append(id, model.Event{Type: model.EventStatus, Actor: "mayfly", Text: "Session failed.", Data: map[string]any{"status": string(model.StatusFailed)}})
		snapshot := m.copySession(inst)
		return &snapshot, err
	}

	inst.mu.Lock()
	inst.handle = h
	inst.sess.LastActivity = time.Now().UTC()
	inst.mu.Unlock()
	m.hub.Append(id, model.Event{Type: model.EventStatus, Actor: "mayfly", Text: "Session container started.", Data: map[string]any{"status": string(model.StatusPending)}})

	if h.Done != nil {
		m.watchLocal(inst, h)
	}
	snapshot := m.copySession(inst)
	m.logger.Info("session created", "session", id, "backend", m.cfg.Runtime.Backend, "ttl", ttl.String())
	return &snapshot, nil
}

// prepare builds backend-specific launch data.
func (m *Manager) prepare(_ context.Context, inst *instance, podName, token string, ttl time.Duration, image string, command, args []string, env map[string]string) (*k8s.SessionSpec, error) {
	cfg := inst.runtimeCfg
	id := inst.sess.ID
	if m.cfg.Runtime.Backend == "local" {
		state := filepath.Join(m.stateDir, "sessions", id)
		cfg.WorkspaceDir = filepath.Join(state, "workspace")
		cfg.ArtifactsDir = filepath.Join(state, "artifacts")
		for i := range cfg.Plugins {
			if cfg.Plugins[i].Placement != "inline" || len(cfg.Plugins[i].Secrets) == 0 {
				continue
			}
			cfg.Plugins[i].SecretsDir = filepath.Join(state, "plugin-secrets", cfg.Plugins[i].Name)
		}
		inst.runtimeCfg = cfg
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		return nil, nil
	}

	cfg.WorkspaceDir = k8s.WorkspaceDir
	cfg.ArtifactsDir = k8s.ArtifactsDir
	inst.runtimeCfg = cfg
	persistent := config.Bool(m.cfg.Sessions.PersistentWorkspace, true)
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	port := platformPort(m.cfg.Server.Addr)
	spec := &k8s.SessionSpec{
		Namespace:           m.cfg.Sessions.Namespace,
		SessionID:           id,
		PodName:             podName,
		SecretName:          podName,
		ConfigMapName:       podName,
		Image:               image,
		ImagePullPolicy:     m.cfg.Runtime.ImagePullPolicy,
		ImagePullSecrets:    m.cfg.Runtime.ImagePullSecrets,
		ServiceAccount:      m.cfg.Runtime.ServiceAccount,
		TTL:                 ttl,
		WorkspaceLimit:      m.cfg.Runtime.WorkspaceLimit,
		ArtifactLimit:       m.cfg.Runtime.ArtifactLimit,
		PersistentWorkspace: persistent,
		PVCName:             "mayfly-" + id + "-workspace",
		StorageClass:        m.cfg.Runtime.StorageClass,
		Resources: k8s.ResourceConfig{
			Requests: m.cfg.Runtime.Resources.Requests,
			Limits:   m.cfg.Runtime.Resources.Limits,
		},
		Env:                 env,
		Labels:              m.cfg.Runtime.PodLabels,
		Command:             command,
		Args:                args,
		EnableNetworkPolicy: config.Bool(m.cfg.Runtime.NetworkPolicy, true),
		DNSNamespace:        m.cfg.Runtime.DNSNamespace,
		PlatformNs:          m.cfg.Runtime.PlatformNamespace,
		PlatformLabels:      m.cfg.Runtime.PlatformLabels,
		PlatformPort:        port,
		SessionJSON:         raw,
		Token:               token,
		Plugins:             cfg.Plugins,
		ToolsImage:          m.cfg.Runtime.Image,
		PluginVolumeLimit:   m.cfg.Runtime.PluginVolumeLimit,
		GatewayKey:          inst.sidecarKey,
		SidecarKeys:         sidecarKeys(inst.sidecarKey, cfg.Plugins),
	}
	for _, e := range m.cfg.Runtime.Egress {
		spec.Egress = append(spec.Egress, k8s.Egress{CIDR: e.CIDR, Ports: e.Ports})
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return spec, nil
}

func (m *Manager) modelAllowed(name string) bool {
	return m.cfg.LLM.ModelAllowed(name)
}

// completeRunes returns the length of b without a trailing incomplete UTF-8
// sequence. A window too short to hold a full sequence is left as is.
func completeRunes(b []byte) int {
	n := len(b)
	for back := 1; back <= utf8.UTFMax && back <= n; back++ {
		if !utf8.RuneStart(b[n-back]) {
			continue
		}
		if !utf8.FullRune(b[n-back:]) && n-back > 0 {
			return n - back
		}
		return n
	}
	return n
}

// idempotentLocked returns the registered session created with key, or nil.
// The caller holds mu.
func (m *Manager) idempotentLocked(key string) *instance {
	if key == "" {
		return nil
	}
	for _, inst := range m.sessions {
		if inst.idempotencyKey == key {
			return inst
		}
	}
	return nil
}

// reserveLocked registers a new session and enforces sessions.max_concurrent.
// The caller must hold m.mu.
func (m *Manager) reserveLocked(inst *instance) error {
	id := inst.sess.ID
	if _, exists := m.sessions[id]; exists {
		return fmt.Errorf("session id %q is already in use", id)
	}
	if _, exists := m.history[id]; exists {
		return fmt.Errorf("session id %q is already in use", id)
	}
	if max := m.cfg.Sessions.MaxConcurrent; max > 0 {
		active := 0
		for _, other := range m.sessions {
			if !other.done() {
				active++
			}
		}
		if active >= max {
			return fmt.Errorf("%w: %d sessions are already active", ErrCapacity, active)
		}
	}
	m.sessions[id] = inst
	m.tokenIndex[inst.tokenHash] = id
	return nil
}

// RuntimeConnected records that the session runtime reached the control plane.
// A session becomes running here, not when its container starts.
func (m *Manager) RuntimeConnected(id string) {
	inst, ok := m.instance(id)
	if !ok {
		return
	}
	inst.mu.Lock()
	if inst.sess.Status != model.StatusPending {
		inst.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	inst.sess.Status = model.StatusRunning
	inst.sess.StartedAt = &now
	inst.sess.LastActivity = now
	inst.mu.Unlock()
	m.persistInstance(inst)
	m.hub.Append(id, model.Event{Type: model.EventStatus, Actor: "mayfly", Text: "Session running.", Data: map[string]any{"status": string(model.StatusRunning)}})
	m.logger.Info("session runtime connected", "session", id)
}

// forget drops a finished session from memory. Its artifact stays in the
// configured store.
func (m *Manager) forget(id string) {
	m.mu.Lock()
	inst := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if inst == nil {
		return
	}
	inst.mu.Lock()
	hash := inst.tokenHash
	inst.mu.Unlock()
	m.removeToken(hash)
	m.hub.Remove(id)
	m.deleteRecord(id)
	// The artifact index outlives the session so downloads keep working.
	m.logger.Info("session removed from memory", "session", id)
}

// FreeSlots reports how many sessions may start before sessions.max_concurrent.
// A worker uses it to hold queued work instead of racing the cap.
func (m *Manager) FreeSlots() int {
	max := m.cfg.Sessions.MaxConcurrent
	if max <= 0 {
		return 1 << 20
	}
	m.mu.Lock()
	active := 0
	for _, inst := range m.sessions {
		if !inst.done() {
			active++
		}
	}
	m.mu.Unlock()
	if active >= max {
		return 0
	}
	return max - active
}

// Ready reports whether the control plane can serve traffic. It checks the
// lifecycle engine only: the artifact store is remote and a slow or failed
// store must not take the replica out of the Service that live sessions use.
func (m *Manager) Ready() error {
	if !m.started.Load() {
		return errors.New("lifecycle engine is not running")
	}
	return nil
}

func (m *Manager) resolveSkills(agent agents.Agent, requested []string) ([]sessioncfg.Skill, []string, error) {
	names := requested
	if len(names) == 0 && agent.Skills == nil {
		names = m.defaultSkill
	}
	out := make([]sessioncfg.Skill, 0, len(names))
	selected := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		skill, ok := m.skillByName[name]
		if !ok {
			return nil, nil, fmt.Errorf("unknown skill %q", name)
		}
		if !scopeVisible(skill.Scope, agent.Name) {
			return nil, nil, fmt.Errorf("skill %q is not available to agent %q", name, agent.Name)
		}
		seen[name] = true
		out = append(out, skill)
		selected = append(selected, name)
	}
	return out, selected, nil
}

func (m *Manager) resolveMCP(agent agents.Agent, requested []string) ([]sessioncfg.MCPServer, []string, error) {
	names := requested
	if len(names) == 0 && agent.MCPServers == nil {
		names = m.defaultMCP
	}
	out := make([]sessioncfg.MCPServer, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		def, ok := m.mcpByName[name]
		if !ok {
			return nil, nil, fmt.Errorf("unknown mcp server %q", name)
		}
		if !scopeVisible(def.Scope, agent.Name) {
			return nil, nil, fmt.Errorf("mcp server %q is not available to agent %q", name, agent.Name)
		}
		seen[name] = true
		// An HTTP entry carries its name and allowlist only: the gateway owns
		// the endpoint and the call, and the runtime never starts a client for
		// it. Keeping the name lets an adopted session rebuild the grant.
		out = append(out, sessioncfg.MCPServer{
			Name:               def.Name,
			Transport:          def.Transport,
			Command:            def.Command,
			Args:               def.Args,
			Env:                def.Env,
			InitTimeoutSeconds: int(def.InitTimeout.Std().Seconds()),
			Tools:              append([]string(nil), def.Tools...),
			IdempotentTools:    append([]string(nil), def.IdempotentTools...),
		})
	}
	return out, names, nil
}

// Defaults the manifest package fills for an inline block. A sidecar manifest
// carries none, but the local backend runs it inline, so it needs them.
const (
	defaultInlineWorkdir     = "plugin"
	defaultInlineMaxRestarts = 2
	defaultInlineStderrBytes = 1 << 20
)

// resolvePlugins turns the agent's plugin selection into the runtime's copy of
// each manifest. It runs inside resolveRequest, so Validate rejects a session
// whose local secret is not in the mayflyd environment before anything starts.
func (m *Manager) resolvePlugins(agent agents.Agent) ([]sessioncfg.Plugin, error) {
	names := agent.PluginNames()
	if len(names) == 0 {
		return nil, nil
	}
	manifests := map[string]plugin.Manifest{}
	if m.bundle != nil {
		manifests = m.bundle.Plugins
	}
	local := m.cfg.Runtime.Backend == "local"
	out := make([]sessioncfg.Plugin, 0, len(names))
	for _, name := range names {
		manifest, ok := manifests[name]
		if !ok {
			return nil, fmt.Errorf("unknown plugin %q", name)
		}
		if !scopeVisible(manifest.Scope, agent.Name) {
			return nil, fmt.Errorf("plugin %q is not available to agent %q", name, agent.Name)
		}
		resolved, err := m.resolvePlugin(manifest, local)
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

// resolvePlugin copies one manifest into the runtime's view. Paths and secret
// references differ by backend; no secret value is ever copied.
func (m *Manager) resolvePlugin(manifest plugin.Manifest, local bool) (sessioncfg.Plugin, error) {
	placement := manifest.Placement
	if placement == "" {
		placement = plugin.Inline
	}
	if local && placement == plugin.Sidecar {
		placement = plugin.Inline
	}
	resolved := sessioncfg.Plugin{
		Name:               manifest.Name,
		Placement:          string(placement),
		Required:           manifest.Required,
		Env:                cloneEnv(manifest.Env),
		Tools:              pluginTools(manifest),
		InitTimeoutSeconds: int(manifest.InitTimeout().Seconds()),
		MaxOutputBytes:     pluginMaxOutput(manifest),
	}
	switch placement {
	case plugin.Inline:
		resolved.Command = manifest.Command
		resolved.Args = append([]string(nil), manifest.Args...)
		resolved.Workdir, resolved.MaxRestarts, resolved.StderrBytes = inlineSettings(manifest)
		if local {
			// The control plane reads the value from its own environment and
			// the local runner writes it to SecretsDir before the agent starts.
			resolved.Dir = manifest.Dir
			resolved.Command = manifest.Local.Command
			resolved.Args = append([]string(nil), manifest.Local.Args...)
			resolved.Secrets = pluginSecretList(manifest, false)
			if err := checkLocalSecrets(manifest); err != nil {
				return sessioncfg.Plugin{}, err
			}
		} else {
			resolved.Dir = k8s.PluginsDir + "/" + manifest.Name
			resolved.Image = manifest.Image
			resolved.Secrets = pluginSecretList(manifest, true)
			resolved.SecretsDir = k8s.PluginSecretsDir + "/" + manifest.Name
		}
	case plugin.Sidecar:
		if manifest.Sidecar == nil || manifest.Sidecar.RunAsUser == nil {
			return sessioncfg.Plugin{}, fmt.Errorf("plugin %q: sidecar settings are not resolved", manifest.Name)
		}
		resolved.Image = manifest.Image
		resolved.Command = manifest.Command
		resolved.Args = append([]string(nil), manifest.Args...)
		resolved.Socket = k8s.PluginSocketPath(manifest.Name)
		resolved.Secrets = pluginSecretList(manifest, true)
		resolved.Sidecar = &sessioncfg.PluginSidecar{
			Requests:  manifest.Sidecar.Resources.Requests,
			Limits:    manifest.Sidecar.Resources.Limits,
			RunAsUser: *manifest.Sidecar.RunAsUser,
			Workspace: manifest.Sidecar.Workspace,
		}
	case plugin.Service:
		// A service plugin records only what the grant is rebuilt from.
	}
	return resolved, nil
}

// inlineSettings are the inline controls of a manifest, with the defaults a
// sidecar manifest never received.
func inlineSettings(manifest plugin.Manifest) (workdir string, maxRestarts int, stderrBytes int64) {
	if manifest.Inline == nil {
		return defaultInlineWorkdir, defaultInlineMaxRestarts, defaultInlineStderrBytes
	}
	workdir = manifest.Inline.Workdir
	if workdir == "" {
		workdir = defaultInlineWorkdir
	}
	stderrBytes = manifest.Inline.StderrBytes
	if stderrBytes == 0 {
		stderrBytes = defaultInlineStderrBytes
	}
	if manifest.Inline.MaxRestarts != nil {
		maxRestarts = *manifest.Inline.MaxRestarts
	} else {
		maxRestarts = defaultInlineMaxRestarts
	}
	return workdir, maxRestarts, stderrBytes
}

// pluginTools copies the declared tool list and resolves each call timeout.
func pluginTools(manifest plugin.Manifest) []sessioncfg.PluginTool {
	out := make([]sessioncfg.PluginTool, 0, len(manifest.Tools))
	for _, tool := range manifest.Tools {
		out = append(out, sessioncfg.PluginTool{
			Name:           tool.Name,
			Idempotent:     tool.Idempotent,
			TimeoutSeconds: int(manifest.ToolTimeout(tool.Name).Seconds()),
		})
	}
	return out
}

// pluginMaxOutput is the plugin's output cap, or the shared default.
func pluginMaxOutput(manifest plugin.Manifest) int {
	if manifest.Limits.MaxOutputBytes > 0 {
		return manifest.Limits.MaxOutputBytes
	}
	return plugin.MaxOutputBytes
}

// pluginSecretList copies the declared secret names. withRefs keeps the
// Kubernetes Secret name and key; the local backend reads the value itself.
func pluginSecretList(manifest plugin.Manifest, withRefs bool) []sessioncfg.PluginSecret {
	if len(manifest.Secrets) == 0 {
		return nil
	}
	out := make([]sessioncfg.PluginSecret, 0, len(manifest.Secrets))
	for _, secret := range manifest.Secrets {
		entry := sessioncfg.PluginSecret{Env: secret.Env}
		if withRefs {
			entry.Secret = secret.Secret
			entry.Key = secret.Key
		}
		out = append(out, entry)
	}
	return out
}

// checkLocalSecrets requires every plugin secret in the mayflyd environment:
// the local backend copies the value into a file for the agent's process.
func checkLocalSecrets(manifest plugin.Manifest) error {
	for _, secret := range manifest.Secrets {
		if _, ok := os.LookupEnv(secret.Env); !ok {
			return fmt.Errorf("plugin %q: environment variable %s is not set", manifest.Name, secret.Env)
		}
	}
	return nil
}

// pluginSecretValues reads the local backend's plugin secrets from the
// mayflyd environment. Create calls it once; a local restart calls it again so
// a changed environment reaches the replacement.
func (m *Manager) pluginSecretValues(plugins []sessioncfg.Plugin) (map[string]map[string]string, error) {
	if m.cfg.Runtime.Backend != "local" {
		return nil, nil
	}
	out := map[string]map[string]string{}
	for _, p := range plugins {
		if len(p.Secrets) == 0 {
			continue
		}
		values := make(map[string]string, len(p.Secrets))
		for _, secret := range p.Secrets {
			value, ok := os.LookupEnv(secret.Env)
			if !ok {
				return nil, fmt.Errorf("plugin %q: environment variable %s is not set", p.Name, secret.Env)
			}
			values[secret.Env] = value
		}
		out[p.Name] = values
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// pluginNames lists the plugin names of a resolved session, in selection
// order.
func pluginNames(plugins []sessioncfg.Plugin) []string {
	if len(plugins) == 0 {
		return nil
	}
	out := make([]string, 0, len(plugins))
	for _, p := range plugins {
		out = append(out, p.Name)
	}
	return out
}

// newSidecarKey generates the signing key of a session with sidecar plugins,
// or returns nil when none is placed there.
func newSidecarKey(plugins []sessioncfg.Plugin) []byte {
	for _, p := range plugins {
		if p.Placement != string(plugin.Sidecar) {
			continue
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic(err)
		}
		return key
	}
	return nil
}

// sidecarKeys derives one signing key per sidecar plugin. The session key stays
// in the Secret's gateway-key entry; each sidecar mounts only its own derived
// item, so no plugin can sign a token its sibling accepts.
func sidecarKeys(sessionKey []byte, plugins []sessioncfg.Plugin) map[string][]byte {
	var out map[string][]byte
	for _, p := range plugins {
		if p.Placement != string(plugin.Sidecar) {
			continue
		}
		if out == nil {
			out = map[string][]byte{}
		}
		out[p.Name] = plugin.SidecarKey(sessionKey, p.Name)
	}
	return out
}

// registerGrant installs the session's tool grant. Create and adoption both
// call it, so a replacement container sees the same table either way.
func (m *Manager) registerGrant(inst *instance) {
	if m.gateway == nil {
		return
	}
	grant := gateway.GrantFor(inst.runtimeCfg, inst.sess.Agent, m.mcpDefs(inst.sess.MCPServers))
	grant.SidecarKey = inst.sidecarKey
	m.gateway.Register(grant)
}

// warnMissingGatewayKey reports a session whose plugins need sidecar call
// tokens but whose Secret holds no signing key. Create writes the key once and
// nothing rotates it, so an empty key means the Secret predates sidecars.
func (m *Manager) warnMissingGatewayKey(cfg sessioncfg.Config, key []byte, session string) {
	if len(key) > 0 {
		return
	}
	for _, p := range cfg.Plugins {
		if p.Placement != string(plugin.Sidecar) {
			continue
		}
		m.logger.Warn("adopt: session has a sidecar plugin and no gateway key; its call tokens cannot be signed", "session", session, "plugin", p.Name)
		return
	}
}

// dropGrant forgets the session's grant. A nil registry has nothing to forget.
func (m *Manager) dropGrant(id string) {
	if m.gateway == nil {
		return
	}
	m.gateway.Drop(id)
}

// mcpDefs returns the definitions of the named MCP servers in order, so the
// grant and the runtime read one resolution.
func (m *Manager) mcpDefs(names []string) []config.MCPServerConfig {
	out := make([]config.MCPServerConfig, 0, len(names))
	for _, name := range names {
		if def, ok := m.mcpByName[name]; ok {
			out = append(out, def)
		}
	}
	return out
}

func (m *Manager) newID() string {
	for {
		id := randomHex(5)
		m.mu.Lock()
		_, live := m.sessions[id]
		_, stored := m.history[id]
		m.mu.Unlock()
		if !live && !stored {
			return id
		}
	}
}

// Get returns a copy of a session, including a finished session restored from
// the record store.
func (m *Manager) Get(id string) (*model.Session, bool) {
	m.mu.Lock()
	inst := m.sessions[id]
	stored, inHistory := m.history[id]
	m.mu.Unlock()
	var snapshot model.Session
	switch {
	case inst != nil:
		snapshot = m.copySession(inst)
	case inHistory:
		snapshot = cloneSession(stored)
	default:
		return nil, false
	}
	if info, ok := m.artifactFor(id); ok {
		snapshot.Artifact = &info
	}
	return &snapshot, true
}

// List returns sessions newest first, including finished sessions restored
// from the record store.
func (m *Manager) List() []model.Session {
	m.mu.Lock()
	insts := make([]*instance, 0, len(m.sessions))
	live := make(map[string]struct{}, len(m.sessions))
	for id, inst := range m.sessions {
		insts = append(insts, inst)
		live[id] = struct{}{}
	}
	stored := make([]model.Session, 0, len(m.history))
	for id, sess := range m.history {
		if _, ok := live[id]; ok {
			continue
		}
		stored = append(stored, cloneSession(sess))
	}
	m.mu.Unlock()
	out := make([]model.Session, 0, len(insts)+len(stored))
	for _, inst := range insts {
		snapshot := m.copySession(inst)
		if info, ok := m.artifactFor(inst.sess.ID); ok {
			snapshot.Artifact = &info
		}
		out = append(out, snapshot)
	}
	for _, snapshot := range stored {
		if info, ok := m.artifactFor(snapshot.ID); ok {
			snapshot.Artifact = &info
		}
		out = append(out, snapshot)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// done reports whether the session has finished.
func (inst *instance) done() bool {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.finalized
}

func (m *Manager) snapshot() []*instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*instance, 0, len(m.sessions))
	for _, inst := range m.sessions {
		out = append(out, inst)
	}
	return out
}

// copySession returns a snapshot without the artifact; callers attach it with
// artifactFor so the two locks are never held together.
func (m *Manager) copySession(inst *instance) model.Session {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return cloneSession(inst.sess)
}

// cloneSession copies a session and the slices a caller may append to.
func cloneSession(in model.Session) model.Session {
	out := in
	out.Skills = append([]string(nil), in.Skills...)
	out.MCPServers = append([]string(nil), in.MCPServers...)
	out.Plugins = append([]string(nil), in.Plugins...)
	return out
}

// setArtifact records the latest bundle for a session.
func (m *Manager) setArtifact(id string, info model.ArtifactInfo) {
	m.mu.Lock()
	m.artifacts[id] = info
	m.mu.Unlock()
	m.saveArtifactIndex(id, info)
}

// artifactFor returns the latest bundle for a session, including sessions that
// retention already removed from memory.
func (m *Manager) artifactFor(id string) (model.ArtifactInfo, bool) {
	m.mu.Lock()
	info, ok := m.artifacts[id]
	m.mu.Unlock()
	return info, ok
}

// Authenticate checks a scoped session token.
func (m *Manager) Authenticate(id, token string) bool {
	if token == "" {
		return false
	}
	hash := hashToken(token)
	m.mu.Lock()
	mapped, ok := m.tokenIndex[hash]
	m.mu.Unlock()
	return ok && mapped == id
}

func (m *Manager) removeToken(hash string) {
	m.mu.Lock()
	delete(m.tokenIndex, hash)
	m.mu.Unlock()
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() string { return randomHex(32) }

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func firstLine(s string) string {
	line := s
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if len(line) > 100 {
		line = line[:100] + "…"
	}
	return line
}

func platformPort(addr string) int32 {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 8080
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 || p > 65535 {
		return 8080
	}
	return int32(p)
}

// StateDir returns the local state directory (local backend only).
func (m *Manager) StateDir() string { return m.stateDir }

// SessionIDForToken resolves a scoped session token.
func (m *Manager) SessionIDForToken(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	hash := hashToken(token)
	m.mu.Lock()
	id, ok := m.tokenIndex[hash]
	m.mu.Unlock()
	return id, ok
}

// SkillNames lists every configured skill.
func (m *Manager) SkillNames() []string {
	out := make([]string, 0, len(m.skills))
	for _, s := range m.skills {
		out = append(out, s.Name)
	}
	return out
}

// MCPServerNames lists every configured MCP server.
func (m *Manager) MCPServerNames() []string {
	out := make([]string, 0, len(m.mcpByName))
	for name := range m.mcpByName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// MaxSessionArtifacts bounds how many inputs one session may reference.
const MaxSessionArtifacts = 8

const (
	maxArtifactKeyBytes  = 200
	maxArtifactTypeBytes = 100
)

var (
	artifactKeyPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	artifactDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	artifactTypePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+-]*/[A-Za-z0-9][A-Za-z0-9.+-]*$`)
)

// resolveArtifacts checks stored artifact references without reading them.
func (m *Manager) resolveArtifacts(refs []ArtifactRef) ([]sessioncfg.Artifact, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if len(refs) > MaxSessionArtifacts {
		return nil, fmt.Errorf("a session accepts at most %d artifacts", MaxSessionArtifacts)
	}
	out := make([]sessioncfg.Artifact, 0, len(refs))
	seen := map[string]bool{}
	for _, ref := range refs {
		key := strings.TrimSpace(ref.Key)
		if len(key) > maxArtifactKeyBytes || !artifactKeyPattern.MatchString(key) || strings.Contains(key, "..") || strings.Contains(key, "//") {
			return nil, fmt.Errorf("invalid artifact key %q", ref.Key)
		}
		if seen[key] {
			return nil, fmt.Errorf("artifact key %q is repeated", key)
		}
		seen[key] = true
		if !artifactDigestPattern.MatchString(ref.SHA256) {
			return nil, fmt.Errorf("artifact %q: sha256 must be 64 lowercase hex characters", key)
		}
		if ref.Bytes <= 0 {
			return nil, fmt.Errorf("artifact %q: bytes must be positive", key)
		}
		if max := m.cfg.Sources.MaxArtifactBytes; max > 0 && ref.Bytes > max {
			return nil, fmt.Errorf("artifact %q: %d bytes exceeds sources.max_artifact_bytes", key, ref.Bytes)
		}
		contentType := strings.TrimSpace(ref.ContentType)
		if len(contentType) > maxArtifactTypeBytes || !artifactTypePattern.MatchString(contentType) {
			return nil, fmt.Errorf("artifact %q: invalid content type %q", key, ref.ContentType)
		}
		out = append(out, sessioncfg.Artifact{
			Key:         key,
			SHA256:      ref.SHA256,
			Bytes:       ref.Bytes,
			ContentType: contentType,
		})
	}
	return out, nil
}

func artifactMap(artifacts []sessioncfg.Artifact) map[string]sessioncfg.Artifact {
	if len(artifacts) == 0 {
		return nil
	}
	out := make(map[string]sessioncfg.Artifact, len(artifacts))
	for _, a := range artifacts {
		out[a.Key] = a
	}
	return out
}

// ArtifactWindow is one bounded read of a stored session input.
type ArtifactWindow struct {
	Key         string `json:"key"`
	Offset      int64  `json:"offset"`
	Bytes       int64  `json:"bytes"`
	Total       int64  `json:"total"`
	NextOffset  *int64 `json:"next_offset"`
	ContentType string `json:"content_type"`
	Content     []byte `json:"-"`
}

// ReadArtifact returns a window of one artifact the session was granted. The
// token must belong to the session and the key must be in its grant list.
func (m *Manager) ReadArtifact(ctx context.Context, id, token, key string, offset, limit int64) (ArtifactWindow, error) {
	idForToken, ok := m.SessionIDForToken(token)
	if !ok || idForToken != id {
		return ArtifactWindow{}, ErrUnauthorized
	}
	m.mu.Lock()
	inst := m.sessions[id]
	m.mu.Unlock()
	if inst == nil {
		return ArtifactWindow{}, ErrNotFound
	}
	inst.mu.Lock()
	ref, ok := inst.artifacts[key]
	inst.mu.Unlock()
	if !ok {
		return ArtifactWindow{}, ErrNotFound
	}
	if limit <= 0 {
		limit = 24 << 10
	}
	if limit > 24<<10 {
		limit = 24 << 10
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= ref.Bytes {
		return ArtifactWindow{Key: key, Offset: offset, Bytes: 0, Total: ref.Bytes, ContentType: ref.ContentType}, nil
	}
	if offset+limit > ref.Bytes {
		limit = ref.Bytes - offset
	}
	body, size, err := m.store.Get(ctx, key)
	if err != nil {
		return ArtifactWindow{}, err
	}
	defer body.Close()
	if size != ref.Bytes {
		return ArtifactWindow{}, fmt.Errorf("artifact %q changed size: %d bytes stored, %d declared", key, size, ref.Bytes)
	}
	inst.mu.Lock()
	verified := inst.verifiedArtifacts[key]
	inst.mu.Unlock()
	// The first read of each artifact hashes the whole object against the
	// declared digest; later windows only check the size, so paging through
	// a large artifact does not rehash it every time.
	var hasher hash.Hash
	var reader io.Reader = body
	if !verified {
		hasher = sha256.New()
		reader = io.TeeReader(body, hasher)
	}
	if _, err := io.CopyN(io.Discard, reader, offset); err != nil {
		return ArtifactWindow{}, err
	}
	content := make([]byte, limit)
	n, err := io.ReadFull(reader, content)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return ArtifactWindow{}, err
	}
	if hasher != nil {
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return ArtifactWindow{}, err
		}
		if got := hex.EncodeToString(hasher.Sum(nil)); got != ref.SHA256 {
			return ArtifactWindow{}, fmt.Errorf("artifact %q does not match its declared sha256", key)
		}
		inst.mu.Lock()
		if inst.verifiedArtifacts == nil {
			inst.verifiedArtifacts = map[string]bool{}
		}
		inst.verifiedArtifacts[key] = true
		inst.mu.Unlock()
	}
	// The window is sent as text. Ending it inside a UTF-8 sequence would
	// garble that character, so a window that is not the last one stops at
	// the sequence start and the next window begins there.
	if offset+int64(n) < ref.Bytes {
		n = completeRunes(content[:n])
	}
	window := ArtifactWindow{
		Key:         key,
		Offset:      offset,
		Bytes:       int64(n),
		Total:       ref.Bytes,
		ContentType: ref.ContentType,
		Content:     content[:n],
	}
	if offset+int64(n) < ref.Bytes {
		next := offset + int64(n)
		window.NextOffset = &next
	}
	return window, nil
}
