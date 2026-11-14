package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	// localMCPPath is the path a local service shim serves.
	localMCPPath = "/mcp"
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

// clientEntry opens one upstream client lazily. A failed handshake is cached
// for the session, so one dead server cannot cost every call its init timeout.
type clientEntry struct {
	once   sync.Once
	client *remoteClient
	err    error
}

// discoveryEntry lists one upstream's tools once per session.
type discoveryEntry struct {
	once  sync.Once
	tools []mcp.Tool
	err   error
}

// remoteClient is one upstream connection. token is set before a request that
// needs a service call or discovery token; mu makes both the token and the
// request that carries it change together.
type remoteClient struct {
	client *mcp.Client
	key    []byte // service plugin gateway key; nil for an mcp.servers entry
	mu     sync.Mutex
	token  string
}

// setAuthorization adds the current call or discovery token to one request.
func (c *remoteClient) setAuthorization(r *http.Request) {
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// remoteClient returns the session's client for one upstream, opening it on
// first use. A session that finalized while the client was opening closes it
// instead of leaking it.
func (g *Gateway) remoteClient(ctx context.Context, session, owner string, placement Placement) (*remoteClient, error) {
	key := remoteKey{session: session, owner: owner}
	g.remote.mu.Lock()
	entry := g.remote.clients[key]
	if entry == nil {
		entry = &clientEntry{}
		g.remote.clients[key] = entry
	}
	g.remote.mu.Unlock()

	entry.once.Do(func() {
		initCtx, cancel := context.WithTimeout(ctx, remoteInitTimeout)
		defer cancel()
		entry.client, entry.err = g.openRemoteClient(initCtx, session, owner, placement)
	})
	if entry.err != nil {
		return nil, entry.err
	}

	g.remote.mu.Lock()
	current := g.remote.clients[key]
	g.remote.mu.Unlock()
	if current != entry {
		_ = entry.client.client.Close()
		return nil, errf(http.StatusNotFound, "session %q was finalized", session)
	}
	return entry.client, nil
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
		rc := &remoteClient{key: key, token: token}
		client, err := g.serviceClient(owner, url, rc.setAuthorization)
		if err != nil {
			return nil, err
		}
		rc.client = client
		if err := rc.client.Initialize(ctx); err != nil {
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
	client := &http.Client{Timeout: 5 * time.Minute}
	if g.opts.HTTPClient != nil {
		*client = *g.opts.HTTPClient
	}
	dialer := &net.Dialer{}
	client.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	return mcp.NewHTTPWithClient(owner, "http://plugin"+localMCPPath, nil, client, perRequest), nil
}

// remoteTools lists one upstream's tools once per session. The result, error
// included, is cached, so discovery runs once even when several tools are
// asked for.
func (g *Gateway) remoteTools(ctx context.Context, session, owner string, placement Placement) ([]mcp.Tool, error) {
	key := remoteKey{session: session, owner: owner}
	g.remote.mu.Lock()
	entry := g.remote.discovery[key]
	if entry == nil {
		entry = &discoveryEntry{}
		g.remote.discovery[key] = entry
	}
	g.remote.mu.Unlock()

	entry.once.Do(func() {
		entry.tools, entry.err = g.discoverUpstream(ctx, session, owner, placement)
	})
	return entry.tools, entry.err
}

// discoverUpstream opens the upstream and lists its tools. A service plugin
// authorizes the listing with a discovery token; an mcp.servers entry has no
// token because its credentials are the resolved headers.
func (g *Gateway) discoverUpstream(ctx context.Context, session, owner string, placement Placement) ([]mcp.Tool, error) {
	client, err := g.remoteClient(ctx, session, owner, placement)
	if err != nil {
		return nil, err
	}
	client.mu.Lock()
	defer client.mu.Unlock()
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
		client.token = token
	}
	initCtx, cancel := context.WithTimeout(ctx, remoteInitTimeout)
	defer cancel()
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
	content, isErr := g.callRemote(ctx, grant, tool, rec, args)
	done, err := g.ledger.Complete(ctx, grant.SessionID, rec.Key, truncate(content, tool.MaxOutput), isErr, false)
	g.limits.release(rec.Key)
	if err != nil {
		return BeginResponse{}, fmt.Errorf("record tool call: %w", err)
	}
	g.auditDone(ctx, grant, tool, done, false)
	return BeginResponse{Result: resultOf(done, false)}, nil
}

// callRemote performs one upstream tools/call under the tool timeout. The
// _meta block carries the ledger key and the session identity; a service
// plugin also gets a call token in Authorization.
func (g *Gateway) callRemote(ctx context.Context, grant Grant, tool GrantTool, rec journal.Call, args json.RawMessage) (string, bool) {
	client, err := g.remoteClient(ctx, grant.SessionID, tool.Owner, tool.Placement)
	if err != nil {
		return err.Error(), true
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, tool.Timeout)
	defer cancel()
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
			return err.Error(), true
		}
		client.token = token
	}

	meta := map[string]any{
		"mayfly.io/idempotency-key": rec.Key,
		"mayfly.io/session":         grant.SessionID,
		"mayfly.io/agent":           grant.Agent,
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	content, isErr, id, err := client.client.CallToolMeta(callCtx, tool.Remote, args, meta)
	timedOut := errors.Is(callCtx.Err(), context.DeadlineExceeded)
	cancel()
	if err == nil {
		return content, isErr
	}
	if timedOut {
		cancelCtx, stop := context.WithTimeout(context.Background(), cancelTimeout)
		_ = client.client.Cancel(cancelCtx, id, "tool call timed out")
		stop()
		return fmt.Sprintf("tool %s timed out after %s", tool.Name, tool.Timeout), true
	}
	if isAuthFailure(err) {
		return authErrorMessage, true
	}
	return err.Error(), true
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
		if entry.client != nil {
			closing = append(closing, entry.client)
		}
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
