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
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hayfever/mayfly/internal/agents"
	"github.com/hayfever/mayfly/internal/artifact"
	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/hub"
	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/model"
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
}

type instance struct {
	mu               sync.Mutex
	sess             model.Session
	tokenHash        string
	runtimeCfg       sessioncfg.Config
	handle           runner.Handle
	closeReason      string
	closeRequestedAt time.Time
	finalized        bool
	resourcesDeleted bool
	launchedAt       time.Time
	podRunningSince  time.Time
	podTerminalSince time.Time
	podMissingSince  time.Time
}

// Manager is the control plane session registry and lifecycle engine.
type Manager struct {
	cfg    *config.Config
	logger *slog.Logger
	store  artifact.Store
	hub    *hub.Hub
	runner runner.Runner
	kube   *k8s.Client

	skills       []sessioncfg.Skill
	skillByName  map[string]sessioncfg.Skill
	mcpByName    map[string]config.MCPServerConfig
	defaultSkill []string
	defaultMCP   []string
	agents       []agents.Agent
	agentByName  map[string]agents.Agent
	defaultAgent string
	stateDir     string

	mu         sync.Mutex
	sessions   map[string]*instance
	tokenIndex map[string]string
	// artifacts survives session retention so a stored bundle stays
	// downloadable after the session record is gone.
	artifacts map[string]model.ArtifactInfo

	started atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New builds a manager. kube may be nil for the local backend.
func New(cfg *config.Config, store artifact.Store, h *hub.Hub, r runner.Runner, kube *k8s.Client, logger *slog.Logger) (*Manager, error) {
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
		logger:      logger,
		store:       store,
		hub:         h,
		runner:      r,
		kube:        kube,
		skills:      loaded,
		skillByName: map[string]sessioncfg.Skill{},
		mcpByName:   map[string]config.MCPServerConfig{},
		sessions:    map[string]*instance{},
		tokenIndex:  map[string]string{},
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
	if m.kube != nil {
		if err := m.adopt(ctx); err != nil {
			m.logger.Warn("adopt existing sessions", "error", err)
		}
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.maintenance()
	}()
	m.started.Store(true)
	return nil
}

// Shutdown stops maintenance and local agent processes.
func (m *Manager) Shutdown(ctx context.Context) {
	m.started.Store(false)
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
	m.wg.Wait()
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
	if len(requestedSkills) == 0 {
		requestedSkills = agent.Skills
	}
	resolvedSkills, skillNames, err := m.resolveSkills(requestedSkills)
	if err != nil {
		return request{}, err
	}
	requestedMCP := opts.MCPServers
	if len(requestedMCP) == 0 {
		requestedMCP = agent.MCPServers
	}
	resolvedMCP, mcpNames, err := m.resolveMCP(requestedMCP)
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
		CreatedAt:          now,
		AgentVersion:       version.Version,
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
			Trigger:      opts.Trigger,
			Backend:      m.cfg.Runtime.Backend,
			TTLSeconds:   int(ttl.Seconds()),
			CreatedAt:    now,
			ExpiresAt:    now.Add(ttl),
			LastActivity: now,
		},
		tokenHash:  hash,
		runtimeCfg: runtimeCfg,
		launchedAt: now,
	}

	if m.cfg.Runtime.Backend == "local" && agent.Runtime.Image != "" {
		m.logger.Warn("agent image is ignored by the local backend", "agent", agent.Name, "image", agent.Runtime.Image)
	}
	spec, err := m.prepare(ctx, inst, podName, token, ttl, image, agent.Runtime.Command, agent.Runtime.Args, runtimeEnv)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if err := m.reserveLocked(inst); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	m.mu.Unlock()
	m.hub.Create(id)
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
		m.removeToken(hash)
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
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			select {
			case <-h.Done:
				time.Sleep(5 * time.Second)
				inst.mu.Lock()
				done := inst.finalized
				inst.mu.Unlock()
				if !done {
					m.fallback(id, "runtime exited without finalizing")
				}
			case <-m.ctx.Done():
			}
		}()
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
		inst.runtimeCfg = cfg
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		return nil, nil
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	port := platformPort(m.cfg.Server.Addr)
	spec := &k8s.SessionSpec{
		Namespace:        m.cfg.Sessions.Namespace,
		SessionID:        id,
		PodName:          podName,
		SecretName:       podName,
		ConfigMapName:    podName,
		Image:            image,
		ImagePullPolicy:  m.cfg.Runtime.ImagePullPolicy,
		ImagePullSecrets: m.cfg.Runtime.ImagePullSecrets,
		ServiceAccount:   m.cfg.Runtime.ServiceAccount,
		TTL:              ttl,
		WorkspaceLimit:   m.cfg.Runtime.WorkspaceLimit,
		ArtifactLimit:    m.cfg.Runtime.ArtifactLimit,
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

// reserveLocked registers a new session and enforces sessions.max_concurrent.
// The caller must hold m.mu.
func (m *Manager) reserveLocked(inst *instance) error {
	id := inst.sess.ID
	if _, exists := m.sessions[id]; exists {
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

func (m *Manager) resolveSkills(requested []string) ([]sessioncfg.Skill, []string, error) {
	names := requested
	if len(names) == 0 {
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
		seen[name] = true
		out = append(out, skill)
		selected = append(selected, name)
	}
	return out, selected, nil
}

func (m *Manager) resolveMCP(requested []string) ([]sessioncfg.MCPServer, []string, error) {
	names := requested
	if len(names) == 0 {
		names = m.defaultMCP
	}
	internal := strings.TrimRight(m.cfg.Server.InternalURL, "/")
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
		seen[name] = true
		server := sessioncfg.MCPServer{
			Name:               def.Name,
			Transport:          def.Transport,
			Command:            def.Command,
			Args:               def.Args,
			Env:                def.Env,
			InitTimeoutSeconds: int(def.InitTimeout.Std().Seconds()),
		}
		if def.Transport == "http" {
			server.URL = internal + "/internal/v1/mcp/" + url.PathEscape(def.Name)
			server.Command = ""
			server.Args = nil
			server.Env = nil
		}
		out = append(out, server)
	}
	return out, names, nil
}

func (m *Manager) newID() string {
	for {
		id := randomHex(5)
		m.mu.Lock()
		_, exists := m.sessions[id]
		m.mu.Unlock()
		if !exists {
			return id
		}
	}
}

// Get returns a copy of a session.
func (m *Manager) Get(id string) (*model.Session, bool) {
	m.mu.Lock()
	inst := m.sessions[id]
	m.mu.Unlock()
	if inst == nil {
		return nil, false
	}
	snapshot := m.copySession(inst)
	if info, ok := m.artifactFor(id); ok {
		snapshot.Artifact = &info
	}
	return &snapshot, true
}

// List returns sessions newest first.
func (m *Manager) List() []model.Session {
	m.mu.Lock()
	insts := make([]*instance, 0, len(m.sessions))
	for _, inst := range m.sessions {
		insts = append(insts, inst)
	}
	m.mu.Unlock()
	out := make([]model.Session, 0, len(insts))
	for _, inst := range insts {
		snapshot := m.copySession(inst)
		if info, ok := m.artifactFor(inst.sess.ID); ok {
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
	out := inst.sess
	out.Skills = append([]string(nil), inst.sess.Skills...)
	out.MCPServers = append([]string(nil), inst.sess.MCPServers...)
	return out
}

// setArtifact records the latest bundle for a session.
func (m *Manager) setArtifact(id string, info model.ArtifactInfo) {
	m.mu.Lock()
	m.artifacts[id] = info
	m.mu.Unlock()
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
