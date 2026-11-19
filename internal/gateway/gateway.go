package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/mcp"
	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// Placement is where a granted tool runs. Local placements are called by the
// session runtime after the gateway authorizes them; remote placements are
// called by the gateway itself.
type Placement string

const (
	Inline  Placement = "inline"
	Sidecar Placement = "sidecar"
	Service Placement = "service"
	// MCPStdio is a stdio mcp.servers tool, called by the runtime.
	MCPStdio Placement = "mcp-stdio"
	// MCPHTTP is an HTTP mcp.servers tool, called by the gateway.
	MCPHTTP Placement = "mcp-http"
)

const (
	// defaultCallTimeout is the manifest's default for one plugin tool.
	defaultCallTimeout = 120 * time.Second
	// mcpCallTimeout matches the runtime's MCP tool timeout.
	mcpCallTimeout = 120 * time.Second
	// defaultMaxInflight bounds dispatched calls across every session.
	defaultMaxInflight = 256
	// discoveryTokenTTL covers session start: the runtime initializes and
	// lists sidecar tools before its first turn.
	discoveryTokenTTL = 15 * time.Minute
	// sidecarTokenGrace keeps a call token valid while the shim reports the
	// result, so a token never expires mid-response.
	sidecarTokenGrace = 30 * time.Second
	// toolCallsPerRound mirrors internal/session.maxToolCallsPerRound. The
	// runtime journals at most that many tool steps per model round, so a
	// ledger key cannot escape the step range the journal accepts.
	toolCallsPerRound = 128
)

// interruptedFormat is the result stored for a non-idempotent call a
// container restart cut short. A second resume replays exactly this text.
const interruptedFormat = "ERROR: tool %s was interrupted by a container restart and may already have taken effect. Mayfly did not run it again because the tool is not idempotent. Check the target before retrying."

// GrantTool is one call the session may make.
type GrantTool struct {
	Name       string // model-visible
	Owner      string // plugin or mcp server name
	Remote     string // tool name on the server
	Placement  Placement
	Idempotent bool
	Timeout    time.Duration
	MaxOutput  int
}

// Grant is one session's call table, fixed when the session is created and
// rebuilt from session.json on adoption.
type Grant struct {
	SessionID string
	Agent     string
	Tools     map[string]GrantTool // exact model-visible names
	// Wildcards holds the mcp.servers entries whose tools are not named in
	// the bundle. An HTTP entry resolves against the server's tools; a stdio
	// entry resolves by its tool-name prefix.
	Wildcards map[string]Placement
	// SidecarKey is the session's signing key. Each sidecar's own key is
	// derived from it with plugin.SidecarKey, so a token minted for one plugin
	// does not verify at another. It is nil when the session has no sidecar.
	SidecarKey []byte
	// MaxSteps is the session's model step bound. The gateway derives the
	// turn's journal step range from it, as internal/session does.
	MaxSteps int
}

// Options wires the gateway to the control plane. Ledger and Sequencer are
// required; the rest select behavior the gateway enables.
type Options struct {
	Ledger journal.Ledger
	// Sequencer bounds a call's turn by the sequence range the session
	// reserved. A nil Sequencer skips the bound.
	Sequencer journal.Sequencer
	// Manifests returns the bundle's plugin manifests. The gateway reads
	// limits.rate from them when a plugin's first call arrives.
	Manifests func() []plugin.Manifest
	// MCPServer returns one mcp.servers definition for remote dispatch.
	MCPServer func(name string) (config.MCPServerConfig, bool)
	// HTTPClient reaches service plugins and HTTP MCP servers. The caller
	// supplies the broker's client to share its transport policy; a nil
	// client uses the MCP package's default.
	HTTPClient *http.Client
	// ServiceURL returns a service plugin's endpoint and gateway key. It is
	// nil until the service reconciler supplies it; service calls then report
	// that no service transport is configured.
	ServiceURL func(name string) (url string, key []byte, err error)
	// MaxInflight bounds dispatched calls across every session.
	MaxInflight int
	Audit       *Audit
	Logger      *slog.Logger
	Now         func() time.Time
	// Observer records every finished or refused call. A nil observer drops
	// the notifications.
	Observer CallObserver
}

// CallObserver receives one notification per finished or refused tool call.
// The gateway calls it on the request path, so an implementation must not
// block.
type CallObserver interface {
	ToolCall(tool, result string)
}

// Results the gateway reports to a CallObserver.
const (
	CallOK          = "ok"
	CallError       = "error"
	CallInterrupted = "interrupted"
	CallDenied      = "denied"
	CallRateLimited = "rate_limited"
	CallBusy        = "busy"
)

func (g *Gateway) observeCall(tool, result string) {
	if g.opts.Observer != nil {
		g.opts.Observer.ToolCall(tool, result)
	}
}

// Gateway is the control plane's tool gateway.
type Gateway struct {
	opts   Options
	ledger journal.Ledger
	now    func() time.Time
	audit  *Audit
	limits *limits

	mu     sync.RWMutex
	grants map[string]Grant

	// flightMu guards flights, which collapses concurrent begins of one call
	// key into one upstream dispatch.
	flightMu sync.Mutex
	flights  map[string]*flight

	// remote holds the upstream MCP clients and their discovery results,
	// one client and one listing per (session, server).
	remote remoteState
}

// flight is one begin in progress. A second begin for the same key waits for
// it instead of calling the upstream again.
type flight struct {
	done chan struct{}
	resp BeginResponse
	err  error
}

// New returns an empty gateway. A nil Logger uses the process logger; a nil
// Audit keeps the audit in that logger and writes no file.
func New(opts Options) *Gateway {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxInflight <= 0 {
		opts.MaxInflight = defaultMaxInflight
	}
	audit := opts.Audit
	if audit == nil {
		audit = &Audit{logger: opts.Logger}
	}
	return &Gateway{
		opts:    opts,
		ledger:  opts.Ledger,
		now:     opts.Now,
		audit:   audit,
		limits:  newLimits(opts.MaxInflight, opts.Logger),
		grants:  map[string]Grant{},
		flights: map[string]*flight{},
		remote: remoteState{
			clients:   map[remoteKey]*clientEntry{},
			discovery: map[remoteKey]*discoveryEntry{},
		},
	}
}

// sidecarKey returns the call-token key of one sidecar plugin, derived from
// the session key. A sidecar mounts only its own derived key, so a token it
// mints for a sibling fails that sibling's signature check.
func sidecarKey(grant Grant, owner string) []byte {
	return plugin.SidecarKey(grant.SidecarKey, owner)
}

// Register installs or replaces one session's grant. A grant registered
// twice keeps the newer table, which is what adoption does.
func (g *Gateway) Register(grant Grant) {
	tools := make(map[string]GrantTool, len(grant.Tools))
	for name, tool := range grant.Tools {
		if tool.Name == "" {
			tool.Name = name
		}
		if tool.Timeout <= 0 {
			if tool.Placement == MCPHTTP || tool.Placement == MCPStdio {
				tool.Timeout = mcpCallTimeout
			} else {
				tool.Timeout = defaultCallTimeout
			}
		}
		if tool.MaxOutput <= 0 {
			tool.MaxOutput = plugin.MaxOutputBytes
		}
		tools[name] = tool
	}
	grant.Tools = tools
	wildcards := make(map[string]Placement, len(grant.Wildcards))
	for name, placement := range grant.Wildcards {
		wildcards[name] = placement
	}
	grant.Wildcards = wildcards
	grant.SidecarKey = append([]byte(nil), grant.SidecarKey...)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.grants[grant.SessionID] = grant
}

// Drop forgets a session's grant, rate limiters, and in-flight slots. The
// ledger records go when the session's journal is deleted.
func (g *Gateway) Drop(session string) {
	g.mu.Lock()
	delete(g.grants, session)
	g.mu.Unlock()
	g.limits.dropSession(session)
	g.dropRemote(session)
}

// Discover reports the session's tools. A local tool carries the grant
// fields only; a remote tool carries the description and schema the upstream
// reported. A remote owner the gateway cannot reach is reported in Errors and
// contributes no tools.
func (g *Gateway) Discover(ctx context.Context, session string) (Discovery, error) {
	grant, ok := g.grantFor(session)
	if !ok {
		return Discovery{}, errf(http.StatusNotFound, "unknown session %q", session)
	}
	out := Discovery{Tools: []DiscoveryTool{}}
	names := make([]string, 0, len(grant.Tools))
	for name := range grant.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	sidecars := map[string]SidecarToken{}
	sidecarSeen := map[string]bool{}
	owners := map[string]Placement{}
	for _, name := range names {
		tool := grant.Tools[name]
		if tool.Placement == Service || tool.Placement == MCPHTTP {
			owners[tool.Owner] = tool.Placement
			continue
		}
		out.Tools = append(out.Tools, DiscoveryTool{
			Name:       tool.Name,
			Owner:      tool.Owner,
			Placement:  tool.Placement,
			Idempotent: tool.Idempotent,
			TimeoutMS:  tool.Timeout.Milliseconds(),
		})
		if tool.Placement == Sidecar && !sidecarSeen[tool.Owner] {
			token, err := SignToken(sidecarKey(grant, tool.Owner), Claims{
				SessionID: grant.SessionID,
				Plugin:    tool.Owner,
				Tool:      "*",
				Expires:   g.now().Add(discoveryTokenTTL).Unix(),
			})
			if err != nil {
				return Discovery{}, errf(http.StatusInternalServerError, "sidecar %q: %v", tool.Owner, err)
			}
			sidecarSeen[tool.Owner] = true
			sidecars[tool.Owner] = SidecarToken{DiscoveryToken: token}
		}
	}
	for owner, placement := range grant.Wildcards {
		owners[owner] = placement
	}

	remoteOwners := make([]string, 0, len(owners))
	for owner := range owners {
		remoteOwners = append(remoteOwners, owner)
	}
	sort.Strings(remoteOwners)
	// Discovery is the retry point for an owner that failed before: the
	// runtime asks for its grant again, so cached handshake and listing
	// failures are dropped here.
	g.retryRemoteFailures(session)
	for _, owner := range remoteOwners {
		placement := owners[owner]
		if placement == MCPStdio {
			// The runtime lists a local server's tools from mcp.Manager.
			continue
		}
		tools, err := g.remoteTools(ctx, session, owner, placement)
		if err != nil {
			out.Errors = append(out.Errors, DiscoveryError{
				Owner:    owner,
				Required: g.requiredOwner(owner, placement),
				Error:    err.Error(),
			})
			continue
		}
		if _, wildcard := grant.Wildcards[owner]; wildcard {
			def, _ := g.mcpServer(owner)
			for _, tool := range tools {
				out.Tools = append(out.Tools, DiscoveryTool{
					Name:        mcp.ToolName(owner, tool.Name),
					Owner:       owner,
					Placement:   placement,
					Description: tool.Description,
					InputSchema: tool.InputSchema,
					Idempotent:  containsName(def.IdempotentTools, tool.Name),
					TimeoutMS:   mcpCallTimeout.Milliseconds(),
				})
			}
			continue
		}
		for _, name := range names {
			tool := grant.Tools[name]
			if tool.Owner != owner {
				continue
			}
			entry := DiscoveryTool{
				Name:       tool.Name,
				Owner:      tool.Owner,
				Placement:  tool.Placement,
				Idempotent: tool.Idempotent,
				TimeoutMS:  tool.Timeout.Milliseconds(),
			}
			for _, reported := range tools {
				if reported.Name == tool.Remote {
					entry.Description = reported.Description
					entry.InputSchema = reported.InputSchema
					break
				}
			}
			out.Tools = append(out.Tools, entry)
		}
	}
	sort.Slice(out.Tools, func(i, j int) bool { return out.Tools[i].Name < out.Tools[j].Name })
	if len(sidecars) > 0 {
		out.Sidecars = sidecars
	}
	if len(grant.Wildcards) > 0 {
		out.Wildcards = grant.Wildcards
	}
	return out, nil
}

// requiredOwner reports whether the bundle marks one plugin required, so a
// discovery failure can fail the session only when the bundle asked for it.
// Only a service plugin is a bundle entry: an mcp.servers owner that happens
// to share a plugin's name fails nothing.
func (g *Gateway) requiredOwner(name string, placement Placement) bool {
	if placement != Service || g.opts.Manifests == nil {
		return false
	}
	for _, manifest := range g.opts.Manifests() {
		if manifest.Name == name {
			return manifest.Required
		}
	}
	return false
}

// Begin authorizes one tool call and applies the resume table.
func (g *Gateway) Begin(ctx context.Context, session string, req BeginRequest) (BeginResponse, error) {
	grant, ok := g.grantFor(session)
	if !ok {
		return BeginResponse{}, errf(http.StatusNotFound, "unknown session %q", session)
	}
	tool, ok := grant.Tools[req.Tool]
	if !ok {
		// A tool of an mcp.servers entry without an allowlist is not in the
		// fixed table; resolve it against the entry before refusing.
		tool, ok = g.wildcardTool(ctx, grant, req.Tool)
		if !ok {
			g.observeCall(req.Tool, CallDenied)
			return BeginResponse{}, errf(http.StatusForbidden, "tool %q is not granted to this session", req.Tool)
		}
	}
	if err := g.checkTurnStep(ctx, session, grant, req); err != nil {
		return BeginResponse{}, err
	}
	hash, err := argsHash(req.Arguments)
	if err != nil {
		return BeginResponse{}, errf(http.StatusBadRequest, "arguments are not valid JSON: %v", err)
	}
	key := callKey(session, req.Turn, req.Step)
	// A begin can run upstream work or record a dispatch. A retry of the same
	// key waits for the begin in flight and gets its answer, instead of
	// starting the call a second time.
	return g.beginFlight(ctx, key, func(ctx context.Context) (BeginResponse, error) {
		return g.begin(ctx, grant, session, req, tool, key, hash)
	})
}

// resumedByAnotherInstance reports whether a pending record belongs to a
// different runtime instance, which is what a container restart leaves. A
// record this same instance opened means the runtime never saw the answer, so
// the tool never ran and the call is safe to issue again.
func resumedByAnotherInstance(tool GrantTool, rec journal.Call, req BeginRequest) bool {
	if tool.Idempotent {
		return false
	}
	return rec.InstanceID == "" || rec.InstanceID != req.InstanceID
}

// beginFlight runs one begin per call key. A second begin for the same key
// waits for the first and returns its answer, so a retry of a lost response
// cannot dispatch the call twice. The work runs on a context detached from the
// request: an upstream call is already under way and its result must reach the
// ledger even when the runtime disconnects.
func (g *Gateway) beginFlight(ctx context.Context, key string, fn func(context.Context) (BeginResponse, error)) (BeginResponse, error) {
	g.flightMu.Lock()
	if active, ok := g.flights[key]; ok {
		g.flightMu.Unlock()
		select {
		case <-active.done:
			return active.resp, active.err
		case <-ctx.Done():
			return BeginResponse{}, ctx.Err()
		}
	}
	active := &flight{done: make(chan struct{})}
	g.flights[key] = active
	g.flightMu.Unlock()
	defer func() {
		g.flightMu.Lock()
		delete(g.flights, key)
		g.flightMu.Unlock()
		close(active.done)
	}()
	active.resp, active.err = fn(context.WithoutCancel(ctx))
	return active.resp, active.err
}

// begin answers one resolved call: a recorded result, the resume table, or a
// fresh dispatch.
func (g *Gateway) begin(ctx context.Context, grant Grant, session string, req BeginRequest, tool GrantTool, key, hash string) (BeginResponse, error) {
	// A recorded call needs no rate token and no in-flight slot: the gateway
	// answers from the ledger, and a resume must not be refused for a call it
	// never makes.
	if rec, found, err := g.ledger.Get(ctx, session, key); err != nil {
		return BeginResponse{}, err
	} else if found {
		if rec.Tool != tool.Name || rec.ArgsSHA256 != hash {
			return BeginResponse{}, errf(http.StatusConflict, "call key %s was used for another tool or arguments", key)
		}
		switch {
		case rec.State == journal.CallDone:
			g.auditBegin(ctx, grant, tool, rec)
			g.auditDone(ctx, grant, tool, rec, true)
			return BeginResponse{Result: resultOf(rec, true)}, nil
		case resumedByAnotherInstance(tool, rec, req):
			return g.interrupt(ctx, grant, tool, rec)
		}
	}

	limiter := g.limits.limiter(session, tool.Owner, g.rateFor(tool.Owner))
	if !limiter.Allow() {
		g.observeCall(tool.Name, CallRateLimited)
		return BeginResponse{}, &Error{
			Status:     http.StatusTooManyRequests,
			Message:    fmt.Sprintf("plugin %q is rate limited", tool.Owner),
			RetryAfter: limiter.RetryAfter(),
		}
	}
	now := g.now()
	if !g.limits.reserve(key, now, now.Add(tool.Timeout)) {
		g.observeCall(tool.Name, CallBusy)
		return BeginResponse{}, errf(http.StatusServiceUnavailable, "too many tool calls are in flight")
	}
	rec, created, err := g.ledger.Begin(ctx, journal.Call{
		Key:        key,
		SessionID:  session,
		Turn:       req.Turn,
		Step:       req.Step,
		Tool:       tool.Name,
		ArgsSHA256: hash,
		InstanceID: req.InstanceID,
	})
	if err != nil {
		g.limits.release(key)
		return BeginResponse{}, fmt.Errorf("record tool call: %w", err)
	}
	if !created {
		// Another begin for this key reached the ledger first.
		if rec.Tool != tool.Name || rec.ArgsSHA256 != hash {
			g.limits.release(key)
			return BeginResponse{}, errf(http.StatusConflict, "call key %s was used for another tool or arguments", key)
		}
		if rec.State == journal.CallDone {
			g.limits.release(key)
			g.auditBegin(ctx, grant, tool, rec)
			g.auditDone(ctx, grant, tool, rec, true)
			return BeginResponse{Result: resultOf(rec, true)}, nil
		}
		if resumedByAnotherInstance(tool, rec, req) {
			g.limits.release(key)
			return g.interrupt(ctx, grant, tool, rec)
		}
	}

	g.auditBegin(ctx, grant, tool, rec)
	if tool.Placement == Service || tool.Placement == MCPHTTP {
		return g.beginRemote(ctx, grant, tool, rec, req.Arguments)
	}
	dispatch := Dispatch{Key: key, TimeoutMS: tool.Timeout.Milliseconds()}
	if tool.Placement == Sidecar {
		token, err := SignToken(sidecarKey(grant, tool.Owner), Claims{
			SessionID:  session,
			Plugin:     tool.Owner,
			Tool:       tool.Remote,
			Key:        key,
			ArgsSHA256: hash,
			Expires:    now.Add(tool.Timeout + sidecarTokenGrace).Unix(),
		})
		if err != nil {
			g.limits.release(key)
			return BeginResponse{}, errf(http.StatusInternalServerError, "sign call token: %v", err)
		}
		dispatch.Token = token
	}
	return BeginResponse{Dispatch: &dispatch}, nil
}

// interrupt answers a pending non-idempotent call, records the interrupted
// result, and returns it. A later resume reads that record and gets the same
// answer.
func (g *Gateway) interrupt(ctx context.Context, grant Grant, tool GrantTool, rec journal.Call) (BeginResponse, error) {
	content := fmt.Sprintf(interruptedFormat, tool.Name)
	done, err := g.ledger.Complete(ctx, grant.SessionID, rec.Key, content, true, true)
	if err != nil {
		return BeginResponse{}, fmt.Errorf("record interrupted call: %w", err)
	}
	g.auditBegin(ctx, grant, tool, done)
	g.auditDone(ctx, grant, tool, done, false)
	return BeginResponse{Result: resultOf(done, false)}, nil
}

// Complete stores the result of a locally dispatched call.
func (g *Gateway) Complete(ctx context.Context, session string, req CompleteRequest) error {
	rec, found, err := g.ledger.Get(ctx, session, req.Key)
	if err != nil {
		return err
	}
	if !found {
		return errf(http.StatusNotFound, "unknown call %q", req.Key)
	}
	grant, hasGrant := g.grantFor(session)
	tool, ok := grant.Tools[rec.Tool]
	if !ok {
		// A stdio mcp.servers entry without an allowlist is not in the fixed
		// table. Keep the audit record's owner and placement for it.
		tool = GrantTool{Name: rec.Tool, MaxOutput: plugin.MaxOutputBytes}
		for owner, placement := range grant.Wildcards {
			if placement == MCPStdio && strings.HasPrefix(rec.Tool, mcp.ToolPrefix(owner)) {
				tool.Owner = owner
				tool.Placement = placement
				break
			}
		}
	}
	limit := tool.MaxOutput
	if limit <= 0 {
		limit = plugin.MaxOutputBytes
	}
	done, err := g.ledger.Complete(ctx, session, req.Key, truncate(req.Content, limit), req.IsError, false)
	g.limits.release(req.Key)
	if err != nil {
		switch {
		case errors.Is(err, journal.ErrCallNotFound):
			return errf(http.StatusNotFound, "unknown call %q", req.Key)
		case errors.Is(err, journal.ErrCallConflict):
			return errf(http.StatusConflict, "call %q already completed with another result", req.Key)
		default:
			return err
		}
	}
	if rec.State == journal.CallDone {
		// A repeated complete is a no-op; the first one wrote the audit.
		return nil
	}
	if !hasGrant {
		grant = Grant{SessionID: session}
	}
	g.auditDone(ctx, grant, tool, done, false)
	return nil
}

// grantFor returns the session's grant.
func (g *Gateway) grantFor(session string) (Grant, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	grant, ok := g.grants[session]
	return grant, ok
}

// checkTurnStep bounds the call key to a turn this control plane reserved and
// to a step the runtime can journal.
func (g *Gateway) checkTurnStep(ctx context.Context, session string, grant Grant, req BeginRequest) error {
	if req.Turn <= 0 {
		return errf(http.StatusBadRequest, "turn must be positive")
	}
	if g.opts.Sequencer != nil {
		high, err := g.opts.Sequencer.HighWater(ctx, session)
		if err != nil {
			return fmt.Errorf("read reserved sequence for %q: %w", session, err)
		}
		if req.Turn > high {
			return errf(http.StatusBadRequest, "turn %d was not reserved for this session", req.Turn)
		}
	}
	if req.Step <= 0 {
		return errf(http.StatusBadRequest, "step must be positive")
	}
	if limit := stepLimit(grant.MaxSteps); req.Step >= limit {
		return errf(http.StatusBadRequest, "step %d is over the %d steps a turn can take", req.Step, limit)
	}
	return nil
}

// rateFor returns the bundle's rate limit for one plugin, or nil.
func (g *Gateway) rateFor(name string) *plugin.Rate {
	if g.opts.Manifests == nil {
		return nil
	}
	for _, manifest := range g.opts.Manifests() {
		if manifest.Name == name {
			return manifest.Limits.Rate
		}
	}
	return nil
}

// auditBegin records an authorized call without its arguments.
func (g *Gateway) auditBegin(ctx context.Context, grant Grant, tool GrantTool, rec journal.Call) {
	g.audit.write(ctx, auditRecord{
		Time:       g.now(),
		Event:      "call.begin",
		Session:    grant.SessionID,
		Agent:      grant.Agent,
		Tool:       tool.Name,
		Owner:      tool.Owner,
		Placement:  string(tool.Placement),
		Key:        rec.Key,
		ArgsSHA256: rec.ArgsSHA256,
		Attempt:    rec.Attempts,
	})
}

// auditDone records a completed call without its result.
func (g *Gateway) auditDone(ctx context.Context, grant Grant, tool GrantTool, rec journal.Call, replayed bool) {
	g.audit.write(ctx, auditRecord{
		Time:        g.now(),
		Event:       "call.done",
		Session:     grant.SessionID,
		Agent:       grant.Agent,
		Tool:        tool.Name,
		Owner:       tool.Owner,
		Placement:   string(tool.Placement),
		Key:         rec.Key,
		ArgsSHA256:  rec.ArgsSHA256,
		Attempt:     rec.Attempts,
		DurationMS:  callDuration(rec, g.now()).Milliseconds(),
		IsError:     rec.IsError,
		Replayed:    replayed,
		Interrupted: rec.Interrupted,
	})
	if replayed {
		// A ledger replay answers the runtime without running the call again.
		return
	}
	result := CallOK
	switch {
	case rec.Interrupted:
		result = CallInterrupted
	case rec.IsError:
		result = CallError
	}
	g.observeCall(tool.Name, result)
}

// GrantFor builds one session's grant from its runtime config. Plugin tools
// come from the manifest copy in session.json; an HTTP mcp.servers entry
// without a tool list keeps today's "every tool of a selected server"
// behavior through a wildcard.
func GrantFor(cfg sessioncfg.Config, agent string, mcp []config.MCPServerConfig) Grant {
	grant := Grant{
		SessionID: cfg.SessionID,
		Agent:     agent,
		Tools:     map[string]GrantTool{},
		Wildcards: map[string]Placement{},
		MaxSteps:  cfg.MaxSteps,
	}
	for _, p := range cfg.Plugins {
		placement := Placement(p.Placement)
		if placement == "" {
			placement = Inline
		}
		for _, t := range p.Tools {
			name := plugin.ToolName(p.Name, t.Name)
			timeout := time.Duration(t.TimeoutSeconds) * time.Second
			if timeout <= 0 {
				timeout = defaultCallTimeout
			}
			maxOutput := p.MaxOutputBytes
			if maxOutput <= 0 {
				maxOutput = plugin.MaxOutputBytes
			}
			grant.Tools[name] = GrantTool{
				Name:       name,
				Owner:      p.Name,
				Remote:     t.Name,
				Placement:  placement,
				Idempotent: t.Idempotent,
				Timeout:    timeout,
				MaxOutput:  maxOutput,
			}
		}
	}
	for _, s := range mcp {
		placement := MCPStdio
		if s.Transport == "http" {
			placement = MCPHTTP
		}
		if len(s.Tools) == 0 {
			// No allowlist: the tools are whatever the server reports. An HTTP
			// server's list is discovered by the gateway; a stdio server's is
			// listed by the runtime's manager.
			grant.Wildcards[s.Name] = placement
			continue
		}
		for _, remote := range s.Tools {
			name := mcpToolName(s.Name, remote)
			grant.Tools[name] = GrantTool{
				Name:       name,
				Owner:      s.Name,
				Remote:     remote,
				Placement:  placement,
				Idempotent: containsName(s.IdempotentTools, remote),
				Timeout:    mcpCallTimeout,
				MaxOutput:  plugin.MaxOutputBytes,
			}
		}
	}
	return grant
}

// resultOf turns a ledger record into the runtime's result.
func resultOf(rec journal.Call, replayed bool) *Result {
	return &Result{
		Content:     rec.Content,
		IsError:     rec.IsError,
		Replayed:    replayed,
		Interrupted: rec.Interrupted,
	}
}

// callKey is the idempotency key of one tool step.
func callKey(session string, turn int64, step int) string {
	return fmt.Sprintf("%s/%d/%d", session, turn, step)
}

// stepLimit is the exclusive upper bound of a turn's step numbers: the user
// step, then per model round one model step and its tool calls.
func stepLimit(maxSteps int) int {
	if maxSteps <= 0 {
		maxSteps = 1
	}
	return 1 + maxSteps*(1+toolCallsPerRound)
}

// ArgsHash is the argument hash a call token carries. The shim verifies the
// same hash, so both sides must compute it with this function.
func ArgsHash(raw json.RawMessage) (string, error) { return argsHash(raw) }

// argsHash hashes the canonical JSON of the arguments. Decoding and
// re-encoding sorts map keys and keeps number literals, so two encodings of
// the same arguments hash alike.
func argsHash(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", errors.New("trailing data after JSON value")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// callDuration reports how long a call took. A replayed result keeps the
// duration the original call had.
func callDuration(rec journal.Call, now time.Time) time.Duration {
	if !rec.DoneAt.IsZero() && !rec.StartedAt.IsZero() {
		return rec.DoneAt.Sub(rec.StartedAt)
	}
	if !rec.StartedAt.IsZero() && now.After(rec.StartedAt) {
		return now.Sub(rec.StartedAt)
	}
	return 0
}

// truncate caps a tool result with the same message the runtime uses, so a
// result the gateway stored and one the runtime produced read alike.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n(truncated at %d bytes)", max)
}
