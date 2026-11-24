package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/mcp"
	"github.com/hayfever/mayfly/internal/plugin"
)

const (
	// remoteInitTimeout bounds the MCP handshake and tool listing of one
	// upstream. The broker's client bounds connection setup; this bounds the
	// whole exchange.
	remoteInitTimeout = 30 * time.Second
	// cancelTimeout bounds the notifications/cancelled request a timed-out
	// call sends after its context is gone.
	cancelTimeout = 5 * time.Second
	// authErrorMessage replaces an upstream's own text on 401 and 403, so a
	// credential failure reads the same over every transport.
	authErrorMessage = "upstream rejected credentials"
	// remoteFailureTTL bounds how long one failed handshake or listing
	// answers later callers. A session discovery retries at once; this keeps
	// a dead upstream from costing every call its init timeout in between.
	remoteFailureTTL = 30 * time.Second
)

// remoteKey identifies one upstream MCP server in one session. The gateway
// keeps one client and one discovery result for each pair.
type remoteKey struct {
	session string
	owner   string
}

// remoteState holds the upstream clients and their discovery results. Both
// are in memory: a control-plane restart reopens them on the next call.
type remoteState struct {
	mu        sync.Mutex
	clients   map[remoteKey]*clientEntry
	discovery map[remoteKey]*discoveryEntry
}

// clientEntry opens one upstream client lazily. opening is set while a
// handshake is in flight, so concurrent callers wait for it instead of
// repeating it. err is kept for remoteFailureTTL: one dead server must not
// cost every call its init timeout, and one dropped request must not take the
// server out for the session.
type clientEntry struct {
	mu      sync.Mutex
	client  *remoteClient
	opening chan struct{}
	err     error
	errAt   time.Time
}

// discoveryEntry lists one upstream's tools once per session, with the same
// waiting and failure rules as clientEntry.
type discoveryEntry struct {
	mu      sync.Mutex
	loaded  bool
	tools   []mcp.Tool
	opening chan struct{}
	err     error
	errAt   time.Time
}

// resetFailure forgets a cached handshake failure so the next call retries.
func (e *clientEntry) resetFailure() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err = nil
}

// resetFailure forgets a cached listing failure so the next discovery
// retries.
func (e *discoveryEntry) resetFailure() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err == nil {
		return
	}
	e.loaded, e.tools, e.err = false, nil, nil
}

// remoteClient is one upstream connection. key is a service plugin's gateway
// key, nil for an mcp.servers entry. The credential of one request travels in
// that request's context, so concurrent calls share the connection without
// waiting for one another.
type remoteClient struct {
	client *mcp.Client
	key    []byte
}

// requestTokenKey carries one request's upstream credential.
type requestTokenKey struct{}

// withRequestToken attaches the bearer token one upstream request carries.
func withRequestToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, requestTokenKey{}, token)
}

// addRequestToken sets the bearer token carried by the request's context. It
// is the per-request hook of a service client.
func addRequestToken(r *http.Request) {
	token, _ := r.Context().Value(requestTokenKey{}).(string)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
}

// remoteClient returns the session's client for one upstream, opening it on
// first use. The handshake runs on a context detached from the caller's
// request, so a runtime that disconnects mid-call cannot poison the entry.
func (g *Gateway) remoteClient(ctx context.Context, session, owner string, placement Placement) (*remoteClient, error) {
	key := remoteKey{session: session, owner: owner}
	g.remote.mu.Lock()
	entry := g.remote.clients[key]
	if entry == nil {
		entry = &clientEntry{}
		g.remote.clients[key] = entry
	}
	g.remote.mu.Unlock()

	var opened *remoteClient
	for {
		entry.mu.Lock()
		if entry.client != nil {
			opened = entry.client
			entry.mu.Unlock()
			break
		}
		if entry.err != nil && g.now().Before(entry.errAt.Add(remoteFailureTTL)) {
			err := entry.err
			entry.mu.Unlock()
			return nil, err
		}
		if opening := entry.opening; opening != nil {
			entry.mu.Unlock()
			select {
			case <-opening:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		entry.opening = make(chan struct{})
		entry.err = nil
		entry.mu.Unlock()

		initCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), remoteInitTimeout)
		client, err := g.openRemoteClient(initCtx, session, owner, placement)
		cancel()

		entry.mu.Lock()
		entry.client, entry.err = client, err
		if err != nil {
			entry.errAt = g.now()
		}
		close(entry.opening)
		entry.opening = nil
		entry.mu.Unlock()
		return client, err
	}

	g.remote.mu.Lock()
	current := g.remote.clients[key]
	g.remote.mu.Unlock()
	if current != entry {
		_ = opened.client.Close()
		return nil, errf(http.StatusNotFound, "session %q was finalized", session)
	}
	return opened, nil
}

// openRemoteClient handshakes with one upstream. An HTTP mcp.servers entry
// uses its resolved headers; a service plugin gets a discovery token signed
// with its own gateway key.
func (g *Gateway) openRemoteClient(ctx context.Context, session, owner string, placement Placement) (*remoteClient, error) {
	switch placement {
	case MCPHTTP:
		def, ok := g.mcpServer(owner)
		if !ok {
			return nil, fmt.Errorf("mcp server %q is not configured", owner)
		}
		if def.Transport != "http" {
			return nil, fmt.Errorf("mcp server %q is not an HTTP server", owner)
		}
		client := mcp.NewHTTPWithClient(owner, def.URL, def.ResolvedHeaders, g.opts.HTTPClient, nil)
		client.SetContentLimits(g.opts.ContentLimits)
		if err := client.Initialize(ctx); err != nil {
			_ = client.Close()
			return nil, err
		}
		return &remoteClient{client: client}, nil

	case Service:
		if def, ok := g.serviceEndpoint(owner); ok {
			if def.Transport != "http" {
				return nil, fmt.Errorf("mcp server %q is not an HTTP server", def.Name)
			}
			client := mcp.NewHTTPWithClient(owner, def.URL, def.ResolvedHeaders, g.opts.HTTPClient, nil)
			client.SetContentLimits(g.opts.ContentLimits)
			if err := client.Initialize(ctx); err != nil {
				_ = client.Close()
				return nil, err
			}
			return &remoteClient{client: client}, nil
		}
		if g.opts.ServiceURL == nil {
			return nil, errors.New("service plugins are not configured")
		}
		url, key, err := g.opts.ServiceURL(owner)
		if err != nil {
			return nil, err
		}
		token, err := SignToken(key, Claims{
			SessionID: session,
			Plugin:    owner,
			Tool:      "*",
			Expires:   g.now().Add(discoveryTokenTTL).Unix(),
		})
		if err != nil {
			return nil, err
		}
		rc := &remoteClient{key: key}
		client, err := g.serviceClient(owner, url, addRequestToken)
		if err != nil {
			return nil, err
		}
		client.SetContentLimits(g.opts.ContentLimits)
		rc.client = client
		if err := rc.client.Initialize(withRequestToken(ctx, token)); err != nil {
			_ = rc.client.Close()
			return nil, err
		}
		return rc, nil

	default:
		return nil, fmt.Errorf("tool owner %q has no remote transport", owner)
	}
}

// serviceEndpoint returns the mcp.servers entry that a service plugin points
// at. Such a plugin renders no workload; the gateway calls that server under
// the plugin's tool names.
func (g *Gateway) serviceEndpoint(owner string) (config.MCPServerConfig, bool) {
	if g.opts.Manifests == nil {
		return config.MCPServerConfig{}, false
	}
	for _, manifest := range g.opts.Manifests() {
		if manifest.Name != owner || manifest.Placement != plugin.Service {
			continue
		}
		if manifest.Service == nil || manifest.Service.MCPServer == "" {
			continue
		}
		return g.mcpServer(manifest.Service.MCPServer)
	}
	return config.MCPServerConfig{}, false
}

// serviceClient builds the MCP client of one service workload. A local
// backend service listens on a Unix socket, which the URL marks with a
// "unix:" prefix; the shim always serves the fixed /mcp path there.
func (g *Gateway) serviceClient(owner, url string, perRequest func(*http.Request)) (*mcp.Client, error) {
	if !strings.HasPrefix(url, "unix:") {
		return mcp.NewHTTPWithClient(owner, url, nil, g.opts.HTTPClient, perRequest), nil
	}
	socket := strings.TrimPrefix(url, "unix:")
	if socket == "" {
		return nil, errors.New("service socket path is empty")
	}
	return mcp.NewUnixHTTP(owner, socket, perRequest), nil
}

// remoteTools lists one upstream's tools once per session. The result is
// cached; a failure is cached for remoteFailureTTL, and a later discovery
// retries it at once.
func (g *Gateway) remoteTools(ctx context.Context, session, owner string, placement Placement) ([]mcp.Tool, error) {
	key := remoteKey{session: session, owner: owner}
	g.remote.mu.Lock()
	entry := g.remote.discovery[key]
	if entry == nil {
		entry = &discoveryEntry{}
		g.remote.discovery[key] = entry
	}
	g.remote.mu.Unlock()

	for {
		entry.mu.Lock()
		if entry.loaded && (entry.err == nil || g.now().Before(entry.errAt.Add(remoteFailureTTL))) {
			tools, err := entry.tools, entry.err
			entry.mu.Unlock()
			return tools, err
		}
		if opening := entry.opening; opening != nil {
			entry.mu.Unlock()
			select {
			case <-opening:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		entry.opening = make(chan struct{})
		entry.mu.Unlock()

		tools, err := g.discoverUpstream(context.WithoutCancel(ctx), session, owner, placement)

		entry.mu.Lock()
		entry.tools, entry.err, entry.loaded = tools, err, true
		if err != nil {
			entry.errAt = g.now()
		}
		close(entry.opening)
		entry.opening = nil
		entry.mu.Unlock()
		return tools, err
	}
}

// discoverUpstream opens the upstream and lists its tools. A service plugin
// authorizes the listing with a discovery token; an mcp.servers entry has no
// token because its credentials are the resolved headers.
func (g *Gateway) discoverUpstream(ctx context.Context, session, owner string, placement Placement) ([]mcp.Tool, error) {
	client, err := g.remoteClient(ctx, session, owner, placement)
	if err != nil {
		return nil, err
	}
	initCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), remoteInitTimeout)
	defer cancel()
	if client.key != nil {
		token, err := SignToken(client.key, Claims{
			SessionID: session,
			Plugin:    owner,
			Tool:      "*",
			Expires:   g.now().Add(discoveryTokenTTL).Unix(),
		})
		if err != nil {
			return nil, err
		}
		initCtx = withRequestToken(initCtx, token)
	}
	return client.client.ListTools(initCtx)
}

// wildcardTool resolves a model-visible name against the mcp.servers entries
// that have no allowlist. HTTP entries resolve against the tools the server
// reports; a stdio entry resolves by its name prefix because the gateway
// never speaks to the process.
func (g *Gateway) wildcardTool(ctx context.Context, grant Grant, name string) (GrantTool, bool) {
	owners := make([]string, 0, len(grant.Wildcards))
	for owner := range grant.Wildcards {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	for _, owner := range owners {
		def, ok := g.mcpServer(owner)
		if !ok {
			continue
		}
		switch grant.Wildcards[owner] {
		case MCPHTTP:
			tools, err := g.remoteTools(ctx, grant.SessionID, owner, MCPHTTP)
			if err != nil {
				continue
			}
			for _, tool := range tools {
				if mcp.ToolName(owner, tool.Name) != name {
					continue
				}
				return wildcardGrant(owner, name, tool.Name, MCPHTTP, def), true
			}
		case MCPStdio:
			if !strings.HasPrefix(name, mcp.ToolPrefix(owner)) {
				continue
			}
			if remote, ok := allowedRemote(def, owner, name); ok {
				return wildcardGrant(owner, name, remote, MCPStdio, def), true
			}
		}
	}
	return GrantTool{}, false
}

// wildcardGrant builds the call entry of an mcp.servers tool that the bundle
// did not name individually. A stdio entry has no remote name, so its
// idempotency is matched by the model-visible name the allowlist would build.
func wildcardGrant(owner, name, remote string, placement Placement, def config.MCPServerConfig) GrantTool {
	idempotent := containsName(def.IdempotentTools, remote)
	if placement == MCPStdio {
		idempotent = false
		for _, candidate := range def.IdempotentTools {
			if mcp.ToolName(owner, candidate) == name {
				idempotent = true
				break
			}
		}
	}
	return GrantTool{
		Name:       name,
		Owner:      owner,
		Remote:     remote,
		Placement:  placement,
		Idempotent: idempotent,
		Timeout:    mcpCallTimeout,
		MaxOutput:  plugin.MaxOutputBytes,
	}
}

// allowedRemote returns the server's own tool name for a model-visible name.
// A stdio server without an allowlist reports a remote name the gateway
// cannot reconstruct, so the empty string tells the caller there is none.
func allowedRemote(def config.MCPServerConfig, owner, name string) (string, bool) {
	if len(def.Tools) == 0 {
		return "", true
	}
	for _, tool := range def.Tools {
		if mcp.ToolName(owner, tool) == name {
			return tool, true
		}
	}
	return "", false
}

// beginRemote calls one upstream tool, records its result, and returns it.
// Every upstream failure becomes an error result: it is recorded done, so a
// resume replays the failure instead of calling the tool again.
func (g *Gateway) beginRemote(ctx context.Context, grant Grant, tool GrantTool, rec journal.Call, args json.RawMessage) (BeginResponse, error) {
	res := g.callRemote(ctx, grant, tool, rec, args)
	res = g.admissibleResult(grant.SessionID, rec.Key, res)
	done, err := g.ledger.Complete(ctx, grant.SessionID, rec.Key, recordedResult(res, tool.MaxOutput))
	g.limits.release(rec.Key)
	if err != nil {
		return BeginResponse{}, fmt.Errorf("record tool call: %w", err)
	}
	g.auditDone(ctx, grant, tool, done, false)
	return BeginResponse{Result: resultOf(done, false)}, nil
}

// callRemote performs one upstream tools/call under the tool timeout. A
// service plugin that rejects its call token may have picked up a new gateway
// key: the cached connection is dropped, so the reconciler is asked for the
// endpoint again, and the call is retried once.
func (g *Gateway) callRemote(ctx context.Context, grant Grant, tool GrantTool, rec journal.Call, args json.RawMessage) mcp.ToolResult {
	client, err := g.remoteClient(ctx, grant.SessionID, tool.Owner, tool.Placement)
	if err != nil {
		return mcp.TextResult(err.Error(), true)
	}
	res, timedOut, err := g.callUpstream(ctx, client, grant, tool, rec, args)
	if err != nil && isAuthFailure(err) && client.key != nil {
		g.forgetRemoteClient(grant.SessionID, tool.Owner)
		if fresh, reopenErr := g.remoteClient(ctx, grant.SessionID, tool.Owner, tool.Placement); reopenErr == nil {
			res, timedOut, err = g.callUpstream(ctx, fresh, grant, tool, rec, args)
		}
	}
	switch {
	case err == nil:
		return res
	case timedOut:
		return mcp.TextResult(fmt.Sprintf("tool %s timed out after %s", tool.Name, tool.Timeout), true)
	case isAuthFailure(err):
		return mcp.TextResult(authErrorMessage, true)
	}
	return mcp.TextResult(err.Error(), true)
}

// callUpstream performs one tools/call on one connection. The call runs on a
// context detached from the begin request and bounded by the tool's timeout:
// a runtime that disconnects no longer aborts a call that may already have
// taken effect upstream. The _meta block carries the ledger key and the
// session identity; a service plugin also gets a call token in Authorization.
func (g *Gateway) callUpstream(ctx context.Context, client *remoteClient, grant Grant, tool GrantTool, rec journal.Call, args json.RawMessage) (mcp.ToolResult, bool, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tool.Timeout)
	if client.key != nil {
		token, err := SignToken(client.key, Claims{
			SessionID:  grant.SessionID,
			Plugin:     tool.Owner,
			Tool:       tool.Remote,
			Key:        rec.Key,
			ArgsSHA256: rec.ArgsSHA256,
			Expires:    g.now().Add(tool.Timeout + sidecarTokenGrace).Unix(),
		})
		if err != nil {
			cancel()
			return mcp.ToolResult{}, false, err
		}
		callCtx = withRequestToken(callCtx, token)
	}
	meta := map[string]any{
		"mayfly.io/idempotency-key": rec.Key,
		"mayfly.io/session":         grant.SessionID,
		"mayfly.io/agent":           grant.Agent,
	}
	res, id, err := client.client.CallToolResultMeta(callCtx, tool.Remote, args, meta)
	timedOut := errors.Is(callCtx.Err(), context.DeadlineExceeded)
	cancel()
	if timedOut {
		cancelCtx, stop := context.WithTimeout(context.Background(), cancelTimeout)
		_ = client.client.Cancel(cancelCtx, id, "tool call timed out")
		stop()
	}
	return res, timedOut, err
}

// isAuthFailure reports a 401 or 403 from an upstream. The MCP HTTP transport
// reports a non-2xx status as text, so the status is matched in the message.
func isAuthFailure(err error) bool {
	message := err.Error()
	return strings.Contains(message, "mcp http status 401") ||
		strings.Contains(message, "mcp http status 403")
}

// dropRemote closes one session's upstream clients and forgets its discovery
// results. A client still being opened closes itself when it sees that its
// entry is gone.
func (g *Gateway) dropRemote(session string) {
	g.remote.mu.Lock()
	var closing []*remoteClient
	for key, entry := range g.remote.clients {
		if key.session != session {
			continue
		}
		delete(g.remote.clients, key)
		entry.mu.Lock()
		if entry.client != nil {
			closing = append(closing, entry.client)
		}
		entry.mu.Unlock()
	}
	for key := range g.remote.discovery {
		if key.session == session {
			delete(g.remote.discovery, key)
		}
	}
	g.remote.mu.Unlock()
	for _, client := range closing {
		_ = client.client.Close()
	}
}

// forgetRemoteClient closes one session's cached upstream connection and
// forgets it, so the next call opens a new one. A service upstream that
// rejects a credential may have rotated its gateway key, and reopening asks
// the reconciler for the current one.
func (g *Gateway) forgetRemoteClient(session, owner string) {
	key := remoteKey{session: session, owner: owner}
	g.remote.mu.Lock()
	entry := g.remote.clients[key]
	delete(g.remote.clients, key)
	g.remote.mu.Unlock()
	if entry == nil {
		return
	}
	entry.mu.Lock()
	client := entry.client
	entry.mu.Unlock()
	if client != nil {
		_ = client.client.Close()
	}
}

// retryRemoteFailures forgets one session's cached handshake and listing
// failures. A discovery is the retry point for an owner that was unavailable
// when the runtime first asked for it.
func (g *Gateway) retryRemoteFailures(session string) {
	g.remote.mu.Lock()
	var clients []*clientEntry
	for key, entry := range g.remote.clients {
		if key.session == session {
			clients = append(clients, entry)
		}
	}
	var listings []*discoveryEntry
	for key, entry := range g.remote.discovery {
		if key.session == session {
			listings = append(listings, entry)
		}
	}
	g.remote.mu.Unlock()
	for _, entry := range clients {
		entry.resetFailure()
	}
	for _, entry := range listings {
		entry.resetFailure()
	}
}

// mcpServer returns one configured mcp.servers entry.
func (g *Gateway) mcpServer(name string) (config.MCPServerConfig, bool) {
	if g.opts.MCPServer == nil {
		return config.MCPServerConfig{}, false
	}
	return g.opts.MCPServer(name)
}

// mcpToolName returns the model-visible name of one MCP server tool. The
// gateway and the runtime must build the same name from the same server.
func mcpToolName(server, tool string) string { return mcp.ToolName(server, tool) }

// containsName reports whether names holds name.
func containsName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}
