package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/hayfever/mayfly/internal/mcp"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// sidecar is one plugin that runs in its own container. The host reaches it
// over the session's socket volume with the tokens the gateway issued: a
// discovery token for initialize and tools/list, and one call token per call.
//
// The plugin process belongs to the container, so a failure here is reported
// and never restarted: Kubernetes owns the sidecar's lifecycle.
type sidecar struct {
	def  sessioncfg.Plugin
	opts Options

	// mu guards client and token, and serializes the requests that carry them:
	// the transport reads token for each request it sends, so a token must not
	// change while a request is in flight.
	mu        sync.Mutex
	client    *mcp.Client
	discovery string
	token     string
}

// start connects one sidecar plugin and verifies the tools its manifest
// declares. It reports the tools the plugin lists, and the ones the manifest
// does not declare.
func (s *sidecar) start(ctx context.Context, discoveryToken string) ([]mcp.Tool, []string, error) {
	if s.client != nil {
		_ = s.client.Close()
		s.client = nil
	}
	if s.def.Socket == "" {
		return nil, nil, errors.New("socket is not set")
	}
	if discoveryToken == "" {
		return nil, nil, errors.New("the gateway issued no discovery token")
	}
	timeout := time.Duration(s.def.InitTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	initCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := mcp.NewUnixHTTP(s.def.Name, s.def.Socket, s.authorization)
	s.mu.Lock()
	s.discovery, s.token = discoveryToken, discoveryToken
	s.mu.Unlock()

	if err := client.Initialize(initCtx); err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	reported, err := client.ListTools(initCtx)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	hidden, err := verifyTools(s.def, reported)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
	return reported, hidden, nil
}

// call runs one tool with its call token. A timeout is reported as an error
// result, because the plugin may have run the tool; an unreachable socket is a
// dispatch error, because the call never arrived.
func (s *sidecar) call(ctx context.Context, tool Tool, args json.RawMessage, key, token string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return "", false, fmt.Errorf("plugin %q is not connected", s.def.Name)
	}
	if token == "" {
		return "", false, fmt.Errorf("plugin %q: call token is missing", s.def.Name)
	}
	s.token = token
	defer func() { s.token = s.discovery }()

	meta := map[string]any{
		"mayfly.io/idempotency-key": key,
		"mayfly.io/session":         s.opts.SessionID,
		"mayfly.io/agent":           s.opts.Agent,
	}
	timeout := tool.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, isErr, id, err := s.client.CallToolMeta(callCtx, tool.Remote, args, meta)
	if err == nil {
		return truncate(out, tool.MaxOutput), isErr, nil
	}
	if isTimeout(ctx, err) {
		s.cancel(id, "timeout")
		return timeoutResult(tool), true, nil
	}
	if ctx.Err() != nil {
		s.cancel(id, "cancelled")
		return "", false, ctx.Err()
	}
	return "", false, err
}

// close drops the client. The container keeps running.
func (s *sidecar) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		_ = s.client.Close()
		s.client = nil
	}
}

// authorization adds the current token to one request. The caller holds mu for
// the whole request, so the field cannot change underneath it.
func (s *sidecar) authorization(r *http.Request) {
	if s.token != "" {
		r.Header.Set("Authorization", "Bearer "+s.token)
	}
}

// cancel tells the shim that a request will not get a result.
func (s *sidecar) cancel(id int64, reason string) {
	if s.client == nil {
		return
	}
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	_ = s.client.Cancel(ctx, id, reason)
}
