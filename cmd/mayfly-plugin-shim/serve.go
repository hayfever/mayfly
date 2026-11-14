package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/version"
)

const (
	// protocolVersion is the MCP revision the shim speaks to the child and to
	// its clients. It matches internal/mcp.
	protocolVersion = "2025-06-18"
	// mcpPath is the endpoint of the streamable HTTP MCP server.
	mcpPath = "/mcp"
	// readinessPath answers a probe. It needs no token, because a Kubernetes
	// probe has none.
	readinessPath = "/readyz"
	// defaultMaxConcurrency bounds in-flight tool calls. It matches the
	// manifest default for service.concurrency.
	defaultMaxConcurrency = 8
	// maxMaxConcurrency is the manifest's upper bound for concurrency.
	maxMaxConcurrency = 256
	// defaultInitTimeout bounds starting, initializing, and listing the child.
	defaultInitTimeout = 30 * time.Second
	// defaultProbeTimeout bounds one probe request.
	defaultProbeTimeout = 2 * time.Second
	// replayCacheSize is how many completed call keys keep their result.
	replayCacheSize = 1024
	// restartDelay is the first wait before a child restart; it doubles up to
	// restartMaxDelay.
	restartDelay    = time.Second
	restartMaxDelay = 30 * time.Second
	// childStderrTail is the child stderr kept for a failure message.
	childStderrTail = 8 << 10
	// maxRequestBody bounds one JSON-RPC request.
	maxRequestBody = 8 << 20
	// shutdownTimeout bounds the HTTP server drain.
	shutdownTimeout = 5 * time.Second
	// sessionHeader carries the MCP session id.
	sessionHeader = "Mcp-Session-Id"
)

// JSON-RPC error codes the shim uses itself.
const (
	codeInvalidParams  = -32602
	codeMethodNotFound = -32601
)

// childEnvAllowList holds the MAYFLY_* variables the plugin may see. Every
// other MAYFLY_* variable stays in the shim, so a session token or the
// control-plane config never reaches the plugin.
var childEnvAllowList = map[string]bool{
	"MAYFLY_PLUGIN_NAME":   true,
	"MAYFLY_PLUGIN_DIR":    true,
	"MAYFLY_SESSION_ID":    true,
	"MAYFLY_WORKSPACE_DIR": true,
}

// serveOptions is one parsed serve command line.
type serveOptions struct {
	listen         string
	keyFile        string
	session        string
	plugin         string
	maxConcurrency int
	initTimeout    time.Duration
	command        []string
	stderr         io.Writer
}

// runServe serves until the process receives SIGINT or SIGTERM.
func runServe(args []string, stderr io.Writer) int {
	opts, err := parseServe(args)
	if err != nil {
		fmt.Fprintf(stderr, "mayfly-plugin-shim: serve: %v\n", err)
		return 2
	}
	key, err := readKey(opts.keyFile)
	if err != nil {
		fmt.Fprintf(stderr, "mayfly-plugin-shim: serve: %v\n", err)
		return 1
	}
	server := newServeServer(opts, key)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.run(ctx); err != nil {
		fmt.Fprintf(stderr, "mayfly-plugin-shim: serve: %v\n", err)
		return 1
	}
	return 0
}

// parseServe reads the flags before "--" and the plugin command after it.
func parseServe(args []string) (serveOptions, error) {
	opts := serveOptions{
		maxConcurrency: defaultMaxConcurrency,
		initTimeout:    defaultInitTimeout,
		plugin:         os.Getenv("MAYFLY_PLUGIN_NAME"),
	}
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.listen, "listen", "", "unix:PATH or tcp:ADDR to serve on")
	flags.StringVar(&opts.keyFile, "key-file", "", "file holding the gateway key")
	flags.StringVar(&opts.session, "session", "", "session id that call tokens must name")
	flags.IntVar(&opts.maxConcurrency, "max-concurrency", defaultMaxConcurrency, "in-flight tool calls")
	flags.DurationVar(&opts.initTimeout, "init-timeout", defaultInitTimeout, "deadline for starting and initializing the child")
	if err := flags.Parse(args); err != nil {
		return serveOptions{}, err
	}
	opts.command = flags.Args()

	if _, _, err := parseListen(opts.listen); err != nil {
		return serveOptions{}, fmt.Errorf("--listen: %w", err)
	}
	if opts.keyFile == "" {
		return serveOptions{}, errors.New("--key-file is required")
	}
	if len(opts.command) == 0 {
		return serveOptions{}, errors.New("a plugin command is required after --")
	}
	if opts.maxConcurrency < 1 || opts.maxConcurrency > maxMaxConcurrency {
		return serveOptions{}, fmt.Errorf("--max-concurrency must be in 1..%d", maxMaxConcurrency)
	}
	if opts.initTimeout <= 0 {
		return serveOptions{}, errors.New("--init-timeout must be positive")
	}
	return opts, nil
}

// runProbe reports whether the shim at --listen has a ready child.
func runProbe(args []string, stderr io.Writer) int {
	listen := ""
	timeout := defaultProbeTimeout
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&listen, "listen", "", "unix:PATH or tcp:ADDR of a running serve")
	flags.DurationVar(&timeout, "timeout", defaultProbeTimeout, "deadline for the probe")
	if err := flags.Parse(args); err != nil {
		fmt.Fprintf(stderr, "mayfly-plugin-shim: probe: %v\n", err)
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "mayfly-plugin-shim: probe: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if _, _, err := parseListen(listen); err != nil {
		fmt.Fprintf(stderr, "mayfly-plugin-shim: probe: --listen: %v\n", err)
		return 2
	}
	if timeout <= 0 {
		fmt.Fprint(stderr, "mayfly-plugin-shim: probe: --timeout must be positive\n")
		return 2
	}
	if err := probe(context.Background(), listen, timeout); err != nil {
		fmt.Fprintf(stderr, "mayfly-plugin-shim: probe: %v\n", err)
		return 1
	}
	return 0
}

// probe asks a running shim whether its child has initialized and listed its
// tools.
func probe(ctx context.Context, listen string, timeout time.Duration) error {
	network, addr, err := parseListen(listen)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://plugin"+readinessPath, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("%s: %w", listen, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s: %s", listen, response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// serveServer is a streamable HTTP MCP endpoint over one stdio child. Every
// client shares the child session; calls carry a token that names the tool,
// the arguments, and the idempotency key.
type serveServer struct {
	opts serveOptions
	key  []byte
	spec childSpec

	mu    sync.Mutex
	child *child
	init  json.RawMessage // the child's initialize result
	ready bool

	calls   *callTable
	cancels *cancelTable
	slots   chan struct{}

	sessions atomic.Int64
	logMu    sync.Mutex
}

// newServeServer builds a server that starts listening when run is called.
func newServeServer(opts serveOptions, key []byte) *serveServer {
	return &serveServer{
		opts: opts,
		key:  key,
		spec: childSpec{
			command:     opts.command,
			env:         childEnv(os.Environ()),
			stderr:      opts.stderr,
			initTimeout: opts.initTimeout,
		},
		calls:   newCallTable(),
		cancels: newCancelTable(),
		slots:   make(chan struct{}, opts.maxConcurrency),
	}
}

// run serves until ctx is done or the listener fails.
func (s *serveServer) run(ctx context.Context) error {
	network, addr, err := parseListen(s.opts.listen)
	if err != nil {
		return err
	}
	listener, err := listen(network, addr)
	if err != nil {
		return err
	}
	defer removeSocket(network, addr)

	httpServer := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()
	supervisorDone := make(chan struct{})
	go func() {
		defer close(supervisorDone)
		s.supervise(runCtx)
	}()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			s.shutdown(httpServer)
			<-supervisorDone
			return err
		}
	}
	stop()
	s.shutdown(httpServer)
	<-supervisorDone
	return nil
}

// shutdown drains the HTTP server.
func (s *serveServer) shutdown(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = server.Shutdown(ctx)
}

// handler routes the two endpoints. Everything else is 404, so a pod that
// probes another path fails instead of looking unhealthy by accident.
func (s *serveServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(mcpPath, s.handleMCP)
	mux.HandleFunc(readinessPath, s.handleReady)
	return mux
}

// handleReady answers a probe.
func (s *serveServer) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.isReady() {
		http.Error(w, "plugin child is not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleMCP serves one JSON-RPC message.
func (s *serveServer) handleMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		// One child session serves every client, so deleting a client
		// session only forgets its id.
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		w.Header().Set("Allow", "DELETE, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		http.Error(w, "read request: "+err.Error(), http.StatusBadRequest)
		return
	}
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &message); err != nil {
		http.Error(w, "request is not a JSON-RPC message", http.StatusBadRequest)
		return
	}
	claims, err := s.authorize(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mayfly-plugin-shim"`)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	session := r.Header.Get(sessionHeader)

	switch message.Method {
	case "initialize":
		if claims.Tool != "*" {
			writeUnauthorized(w, "call token is not a discovery token")
			return
		}
		s.handleInitialize(w, message.ID)
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "notifications/cancelled":
		s.handleCancel(session, message.Params)
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		writeRPCResult(w, message.ID, json.RawMessage(`{}`))
	case "tools/list":
		if claims.Tool != "*" {
			writeUnauthorized(w, "call token is not a discovery token")
			return
		}
		s.handleList(w, r, message.ID, message.Params)
	case "tools/call":
		s.handleCall(w, r, message.ID, message.Params, claims, session)
	default:
		writeRPCError(w, message.ID, codeMethodNotFound, "method not found: "+message.Method)
	}
}

// authorize verifies the token's signature, expiry, and the scopes the shim
// knows from its own command line.
func (s *serveServer) authorize(r *http.Request) (gateway.Claims, error) {
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return gateway.Claims{}, errors.New("invalid call token: missing token")
	}
	claims, err := gateway.VerifyToken(s.key, token, gateway.TokenScope{
		SessionID: s.opts.session,
		Plugin:    s.opts.plugin,
	}, time.Now())
	if err != nil {
		return gateway.Claims{}, err
	}
	return claims, nil
}

// handleInitialize answers a client handshake with the child's capabilities.
// The child sees one long session, so the shim does not forward this
// handshake.
func (s *serveServer) handleInitialize(w http.ResponseWriter, id json.RawMessage) {
	s.mu.Lock()
	init := s.init
	ready := s.ready
	s.mu.Unlock()
	if !ready || len(init) == 0 {
		http.Error(w, "plugin is not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set(sessionHeader, "shim-"+strconv.FormatInt(s.sessions.Add(1), 10))
	writeRPCResult(w, id, init)
}

// handleList forwards tools/list to the child.
func (s *serveServer) handleList(w http.ResponseWriter, r *http.Request, id, params json.RawMessage) {
	child := s.currentChild()
	if child == nil {
		http.Error(w, "plugin is not ready", http.StatusServiceUnavailable)
		return
	}
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	result, rpcErr, err := child.request(r.Context(), "tools/list", params, nil)
	switch {
	case err != nil:
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case len(rpcErr) > 0:
		writeRPCRawError(w, id, rpcErr)
	default:
		writeRPCResult(w, id, result)
	}
}

// handleCall authorizes and runs one tool call. The child runs it at most
// once per idempotency key: a replay returns the stored result and a
// concurrent duplicate is refused.
func (s *serveServer) handleCall(w http.ResponseWriter, r *http.Request, id, params json.RawMessage, claims gateway.Claims, session string) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		writeRPCError(w, id, codeInvalidParams, "tools/call params are not valid JSON")
		return
	}
	hash, err := gateway.ArgsHash(call.Arguments)
	if err != nil {
		writeRPCError(w, id, codeInvalidParams, "tools/call arguments are not valid JSON")
		return
	}
	if claims.Tool == "*" || claims.Tool != call.Name {
		writeUnauthorized(w, "call token is for another tool")
		return
	}
	if claims.Key == "" || claims.ArgsSHA256 != hash {
		writeUnauthorized(w, "call token is for other arguments")
		return
	}

	result, state := s.calls.start(claims.Key, time.Now())
	switch state {
	case callCached:
		writeRPCResult(w, id, result)
		return
	case callDuplicate:
		http.Error(w, "call key "+claims.Key+" is already in flight", http.StatusConflict)
		return
	}
	if !s.acquire() {
		s.calls.fail(claims.Key)
		http.Error(w, "too many tool calls are in flight", http.StatusServiceUnavailable)
		return
	}
	defer s.release()

	child := s.currentChild()
	if child == nil {
		s.calls.fail(claims.Key)
		http.Error(w, "plugin is not ready", http.StatusServiceUnavailable)
		return
	}

	clientID, hasID := numericID(id)
	key := cancelKey{session: session, id: clientID}
	if hasID {
		defer s.cancels.remove(key)
	}
	result, rpcErr, err := child.request(r.Context(), "tools/call", params, func(childID int64) {
		if hasID {
			s.cancels.add(key, childID)
		}
	})

	switch {
	case err != nil:
		s.calls.fail(claims.Key)
		if r.Context().Err() != nil {
			return
		}
		writeToolFailure(w, id, err.Error())
	case len(rpcErr) > 0:
		s.calls.fail(claims.Key)
		writeRPCRawError(w, id, rpcErr)
	default:
		s.calls.complete(claims.Key, claims.Expires, result)
		writeRPCResult(w, id, result)
	}
}

// handleCancel forwards a client's cancellation with the child's request id.
func (s *serveServer) handleCancel(session string, params json.RawMessage) {
	var notice struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    string          `json:"reason"`
	}
	if err := json.Unmarshal(params, &notice); err != nil {
		return
	}
	id, ok := numericID(notice.RequestID)
	if !ok {
		return
	}
	childID, ok := s.cancels.take(cancelKey{session: session, id: id})
	if !ok {
		return
	}
	child := s.currentChild()
	if child == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/cancelled",
		"params":  map[string]any{"requestId": childID, "reason": notice.Reason},
	})
	if err != nil {
		return
	}
	_ = child.write(payload)
}

// acquire takes one concurrency slot without waiting.
func (s *serveServer) acquire() bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// release returns one concurrency slot.
func (s *serveServer) release() { <-s.slots }

// currentChild returns the running child, or nil while it restarts.
func (s *serveServer) currentChild() *child {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.child
}

// isReady reports whether the child has initialized and listed its tools.
func (s *serveServer) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// setChild records a freshly initialized child.
func (s *serveServer) setChild(child *child, init json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.child = child
	s.init = init
	s.ready = true
}

// clearChild drops readiness and the child reference.
func (s *serveServer) clearChild() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.child = nil
	s.init = nil
	s.ready = false
}

// logf writes one diagnostic line to the container's stderr.
func (s *serveServer) logf(format string, args ...any) {
	if s.opts.stderr == nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	fmt.Fprintf(s.opts.stderr, "mayfly-plugin-shim: serve: "+format+"\n", args...)
}

// supervise keeps one initialized child running. A child that fails to start
// or exits is replaced after a back-off, and readiness stays false until the
// replacement has initialized and listed its tools.
func (s *serveServer) supervise(ctx context.Context) {
	delay := restartDelay
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		child, init, err := startChild(ctx, s.spec)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logf("child did not start: %v", err)
			if !sleepContext(ctx, delay) {
				return
			}
			delay = nextDelay(delay)
			continue
		}
		s.setChild(child, init)
		select {
		case <-child.done:
			s.clearChild()
			s.logf("child exited: %v", child.exitError())
			if time.Since(started) > restartMaxDelay {
				delay = restartDelay
			}
			if !sleepContext(ctx, delay) {
				return
			}
			delay = nextDelay(delay)
		case <-ctx.Done():
			s.clearChild()
			child.stop()
			return
		}
	}
}

// nextDelay doubles a restart delay up to the cap.
func nextDelay(delay time.Duration) time.Duration {
	delay *= 2
	if delay > restartMaxDelay {
		return restartMaxDelay
	}
	return delay
}

// sleepContext waits for d unless ctx ends first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// childSpec is how one plugin process is started and initialized.
type childSpec struct {
	command     []string
	env         []string
	stderr      io.Writer
	initTimeout time.Duration
}

// child is one stdio plugin process. Requests carry ids from a shim counter,
// so many clients can use the one process at the same time.
type child struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	tail  *tailWriter
	done  chan struct{}

	mu      sync.Mutex
	pending map[int64]chan rpcReply
	nextID  int64

	writeMu sync.Mutex

	waitErr error
}

// rpcReply is the part of one child response the shim needs.
type rpcReply struct {
	result json.RawMessage
	err    json.RawMessage
}

// errChildExited marks a request that failed because the child process ended.
var errChildExited = errors.New("plugin child exited")

// startChild starts the child and performs the one handshake every client
// shares: initialize, the initialized notification, and tools/list.
func startChild(ctx context.Context, spec childSpec) (*child, json.RawMessage, error) {
	child, err := spawnChild(spec)
	if err != nil {
		return nil, nil, err
	}
	initCtx, cancel := context.WithTimeout(ctx, spec.initTimeout)
	defer cancel()

	params, err := json.Marshal(map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "mayfly-plugin-shim", "version": version.Version},
	})
	if err != nil {
		child.stop()
		return nil, nil, err
	}
	result, rpcErr, err := child.request(initCtx, "initialize", params, nil)
	switch {
	case err != nil:
		child.stop()
		return nil, nil, fmt.Errorf("initialize: %w", err)
	case len(rpcErr) > 0:
		child.stop()
		return nil, nil, fmt.Errorf("initialize: %s", rpcErr)
	}
	var handshake struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(result, &handshake); err != nil || len(handshake.Capabilities) == 0 {
		child.stop()
		return nil, nil, errors.New("initialize: child reported no capabilities")
	}
	if err := child.notify("notifications/initialized", nil); err != nil {
		child.stop()
		return nil, nil, fmt.Errorf("initialized notification: %w", err)
	}
	if _, rpcErr, err := child.request(initCtx, "tools/list", json.RawMessage(`{}`), nil); err != nil {
		child.stop()
		return nil, nil, fmt.Errorf("tools/list: %w", err)
	} else if len(rpcErr) > 0 {
		child.stop()
		return nil, nil, fmt.Errorf("tools/list: %s", rpcErr)
	}
	return child, result, nil
}

// spawnChild starts the process and its reader. The child gets its own
// process group, so stop reaches whatever it spawned.
func spawnChild(spec childSpec) (*child, error) {
	cmd := exec.Command(spec.command[0], spec.command[1:]...)
	cmd.Env = spec.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	tail := newTailWriter(childStderrTail)
	if spec.stderr != nil {
		cmd.Stderr = io.MultiWriter(tail, spec.stderr)
	} else {
		cmd.Stderr = tail
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.command[0], err)
	}
	child := &child{
		cmd:     cmd,
		stdin:   stdin,
		tail:    tail,
		done:    make(chan struct{}),
		pending: map[int64]chan rpcReply{},
	}
	go func() {
		child.read(stdout)
		child.waitErr = cmd.Wait()
		close(child.done)
	}()
	return child, nil
}

// read dispatches the child's responses to their waiters. A notification or
// a server-to-client request is dropped: the shim offers no client
// capabilities, and the child has no other client.
func (c *child) read(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		if len(message.ID) == 0 || message.Method != "" {
			continue
		}
		id, ok := numericID(message.ID)
		if !ok {
			continue
		}
		c.mu.Lock()
		waiter := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if waiter != nil {
			waiter <- rpcReply{result: message.Result, err: message.Error}
		}
	}
}

// request sends one JSON-RPC request and waits for its response. started,
// when not nil, runs with the child's request id before the write, so a
// cancellation can name it.
func (c *child) request(ctx context.Context, method string, params json.RawMessage, started func(id int64)) (json.RawMessage, json.RawMessage, error) {
	var zero json.RawMessage
	select {
	case <-c.done:
		return zero, zero, c.exitError()
	default:
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	waiter := make(chan rpcReply, 1)
	c.pending[id] = waiter
	c.mu.Unlock()
	if started != nil {
		started(id)
	}

	payload, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		c.forget(id)
		return zero, zero, err
	}
	if err := c.write(payload); err != nil {
		c.forget(id)
		return zero, zero, fmt.Errorf("send %s: %w", method, err)
	}

	select {
	case reply := <-waiter:
		return reply.result, reply.err, nil
	case <-ctx.Done():
		c.forget(id)
		return zero, zero, ctx.Err()
	case <-c.done:
		c.forget(id)
		return zero, zero, c.exitError()
	}
}

// notify sends one JSON-RPC notification.
func (c *child) notify(method string, params json.RawMessage) error {
	payload, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return c.write(payload)
}

// write sends one newline-delimited message.
func (c *child) write(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(append(payload, '\n')); err != nil {
		return err
	}
	return nil
}

// forget drops one waiter.
func (c *child) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// exitError describes a child that is gone, with the end of its stderr.
func (c *child) exitError() error {
	err := c.waitErr
	if err == nil {
		err = errors.New("no exit status")
	}
	tail := strings.TrimSpace(c.tail.String())
	if tail == "" {
		return fmt.Errorf("%w: %v", errChildExited, err)
	}
	return fmt.Errorf("%w: %v: %s", errChildExited, err, tail)
}

// stop ends the child: stdin first, then SIGTERM to the process group, then
// SIGKILL. Each step waits two seconds before the next.
func (c *child) stop() {
	if c.cmd.Process == nil {
		return
	}
	_ = c.stdin.Close()
	if waitClosed(c.done, 2*time.Second) {
		return
	}
	_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
	if waitClosed(c.done, 2*time.Second) {
		return
	}
	_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	<-c.done
}

// waitClosed reports whether done closed within d.
func waitClosed(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// tailWriter keeps the last bytes written to it.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

// newTailWriter returns a writer that keeps its last max bytes.
func newTailWriter(max int) *tailWriter { return &tailWriter{max: max} }

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

// String returns the kept bytes.
func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// callState is what callTable.start decided for one key.
type callState int

const (
	// callRun means the caller owns the key and must run the tool.
	callRun callState = iota
	// callCached means a completed result is available.
	callCached
	// callDuplicate means the key is in flight.
	callDuplicate
)

// cachedResult is one completed call kept for a replay.
type cachedResult struct {
	expires int64
	result  json.RawMessage
}

// callTable remembers the keys in flight and the last completed results. A
// replay returns the stored result instead of running the tool again.
type callTable struct {
	mu       sync.Mutex
	inflight map[string]bool
	results  map[string]cachedResult
	order    []string
}

// newCallTable returns an empty table.
func newCallTable() *callTable {
	return &callTable{inflight: map[string]bool{}, results: map[string]cachedResult{}}
}

// start claims a key, or reports the cached result or the duplicate.
func (t *callTable) start(key string, now time.Time) (json.RawMessage, callState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry, ok := t.results[key]; ok {
		if now.Unix() < entry.expires {
			return entry.result, callCached
		}
		delete(t.results, key)
	}
	if t.inflight[key] {
		return nil, callDuplicate
	}
	t.inflight[key] = true
	return nil, callRun
}

// complete stores one result and releases its key.
func (t *callTable) complete(key string, expires int64, result json.RawMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.inflight, key)
	t.results[key] = cachedResult{expires: expires, result: result}
	t.order = append(t.order, key)
	for len(t.order) > replayCacheSize {
		oldest := t.order[0]
		t.order = t.order[1:]
		delete(t.results, oldest)
	}
}

// fail releases a key without storing a result.
func (t *callTable) fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.inflight, key)
}

// cancelKey names one client request: ids are unique per MCP session only.
type cancelKey struct {
	session string
	id      int64
}

// cancelTable maps a client request to the child request id.
type cancelTable struct {
	mu  sync.Mutex
	ids map[cancelKey]int64
}

// newCancelTable returns an empty table.
func newCancelTable() *cancelTable {
	return &cancelTable{ids: map[cancelKey]int64{}}
}

// add records the child id of one client request.
func (t *cancelTable) add(key cancelKey, childID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ids[key] = childID
}

// take removes and returns the child id of one client request.
func (t *cancelTable) take(key cancelKey) (int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.ids[key]
	delete(t.ids, key)
	return id, ok
}

// remove drops one client request.
func (t *cancelTable) remove(key cancelKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.ids, key)
}

// writeRPCResult writes a JSON-RPC result.
func writeRPCResult(w http.ResponseWriter, id, result json.RawMessage) {
	writeRPC(w, struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Result  json.RawMessage `json:"result"`
	}{JSONRPC: "2.0", ID: id, Result: result})
}

// writeRPCRawError writes a JSON-RPC error object the child produced.
func writeRPCRawError(w http.ResponseWriter, id, rpcErr json.RawMessage) {
	writeRPC(w, struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Error   json.RawMessage `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: rpcErr})
}

// writeRPCError writes a JSON-RPC error the shim produced.
func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeRPCRawError(w, id, json.RawMessage(fmt.Sprintf(`{"code":%d,"message":%s}`, code, strconv.Quote(message))))
}

// writeRPC writes one JSON response body.
func writeRPC(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// writeToolFailure reports a call the shim could not run as an MCP error
// result, so the model reads the reason instead of losing the step.
func writeToolFailure(w http.ResponseWriter, id json.RawMessage, text string) {
	result, err := json.Marshal(struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}{
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "text", Text: text}},
		IsError: true,
	})
	if err != nil {
		http.Error(w, text, http.StatusInternalServerError)
		return
	}
	writeRPCResult(w, id, result)
}

// writeUnauthorized refuses a request whose token does not cover it.
func writeUnauthorized(w http.ResponseWriter, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="mayfly-plugin-shim"`)
	http.Error(w, reason, http.StatusUnauthorized)
}

// bearerToken returns the token of an Authorization header.
func bearerToken(header string) string {
	token, ok := strings.CutPrefix(strings.TrimSpace(header), "Bearer ")
	if !ok {
		if token, ok := strings.CutPrefix(strings.TrimSpace(header), "bearer "); ok {
			return strings.TrimSpace(token)
		}
		return ""
	}
	return strings.TrimSpace(token)
}

// numericID parses a JSON-RPC id.
func numericID(raw json.RawMessage) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return id, err == nil
}

// parseListen splits a --listen value into a network and an address.
func parseListen(spec string) (network, addr string, err error) {
	network, addr, ok := strings.Cut(spec, ":")
	if !ok || (network != "unix" && network != "tcp") || addr == "" {
		return "", "", fmt.Errorf("%q is not unix:PATH or tcp:ADDR", spec)
	}
	return network, addr, nil
}

// listen binds the address. A unix socket is created with mode 0660 so the
// agent and the shim can share it.
func listen(network, addr string) (net.Listener, error) {
	if network != "unix" {
		return net.Listen(network, addr)
	}
	if dir := filepath.Dir(addr); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create socket directory: %w", err)
		}
	}
	// A restarted shim finds its previous socket file.
	if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	listener, err := net.Listen("unix", addr)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(addr, 0o660); err != nil {
		listener.Close()
		return nil, fmt.Errorf("set socket mode: %w", err)
	}
	return listener, nil
}

// removeSocket deletes a unix socket the shim created.
func removeSocket(network, addr string) {
	if network == "unix" {
		_ = os.Remove(addr)
	}
}

// readKey reads the gateway key. A Secret file holds the raw 32 bytes; a key
// written by hand usually ends with a newline, which is dropped unless the
// file is exactly one key long.
func readKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --key-file: %w", err)
	}
	if len(raw) != 32 {
		raw = []byte(strings.TrimRight(string(raw), "\r\n"))
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("read --key-file: %s is empty", path)
	}
	return raw, nil
}

// childEnv returns the container environment without Mayfly's own variables,
// except the plugin identity the child is meant to read.
func childEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "MAYFLY_") && !childEnvAllowList[name] {
			continue
		}
		out = append(out, entry)
	}
	return out
}
