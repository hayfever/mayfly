package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/llm"
	"github.com/hayfever/mayfly/internal/mcp"
	"github.com/hayfever/mayfly/internal/platformclient"
	"github.com/hayfever/mayfly/internal/pluginhost"
)

const (
	maxReadBytes     = 512 << 10
	maxWriteBytes    = 4 << 20
	maxToolOutput    = 64 << 10
	maxListedEntries = 500
	mcpCallTimeout   = 120 * time.Second
)

// ArtifactReader reads a window of one input artifact.
type ArtifactReader interface {
	ReadArtifact(ctx context.Context, key string, offset, limit int64) (platformclient.ArtifactWindow, error)
}

// toolsetConfig is the set of resources one session's tools can reach. The
// grant is the tool table the gateway reported, keyed by model-visible name.
type toolsetConfig struct {
	Workspace    string
	AllowShell   bool
	ShellTimeout time.Duration
	MCP          *mcp.Manager
	Plugins      *pluginhost.Host
	Grant        map[string]gateway.DiscoveryTool
	// Wildcards names the mcp.servers owners whose tools the grant does not
	// list. A local stdio tool of such an owner is still a gateway call.
	Wildcards map[string]gateway.Placement
	Artifacts ArtifactReader
}

type toolset struct {
	root         *os.Root
	rootPath     string
	allowShell   bool
	shellTimeout time.Duration
	mcp          *mcp.Manager
	plugins      *pluginhost.Host
	grant        map[string]gateway.DiscoveryTool
	wildcards    map[string]gateway.Placement
	artifacts    ArtifactReader
}

func newToolset(cfg toolsetConfig) (*toolset, error) {
	root, err := os.OpenRoot(cfg.Workspace)
	if err != nil {
		return nil, fmt.Errorf("open workspace: %w", err)
	}
	return &toolset{
		root:         root,
		rootPath:     cfg.Workspace,
		allowShell:   cfg.AllowShell,
		shellTimeout: cfg.ShellTimeout,
		mcp:          cfg.MCP,
		plugins:      cfg.Plugins,
		grant:        cfg.Grant,
		wildcards:    cfg.Wildcards,
		artifacts:    cfg.Artifacts,
	}, nil
}

func (t *toolset) Close() error { return t.root.Close() }

// Granted returns the gateway's grant for one tool. An mcp.servers entry
// without an allowlist grants its tools by name prefix; the runtime can prove
// the tool exists because its own manager listed it.
func (t *toolset) Granted(name string) (gateway.DiscoveryTool, bool) {
	tool, ok := t.grant[name]
	if ok {
		return tool, true
	}
	if t.mcp == nil || len(t.wildcards) == 0 {
		return gateway.DiscoveryTool{}, false
	}
	for _, local := range t.mcp.Tools() {
		if local.Name != name {
			continue
		}
		if t.wildcards[local.Server] != gateway.MCPStdio {
			return gateway.DiscoveryTool{}, false
		}
		return gateway.DiscoveryTool{
			Name:      name,
			Owner:     local.Server,
			Placement: gateway.MCPStdio,
		}, true
	}
	return gateway.DiscoveryTool{}, false
}

// Tools returns the OpenAI tool declarations the model may call: built-ins,
// then the MCP servers this runtime reached directly, then the granted plugin
// tools.
func (t *toolset) Tools() []llm.Tool {
	tools := []llm.Tool{
		llm.NewTool("read_file", "Read a UTF-8 text file from the workspace.", json.RawMessage(`{
			"type":"object",
			"properties":{"path":{"type":"string","description":"Path relative to the workspace"}},
			"required":["path"]
		}`)),
		llm.NewTool("write_file", "Create or replace a file in the workspace. Parent directories are created.", json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"Path relative to the workspace"},
				"content":{"type":"string","description":"Full file content"}
			},
			"required":["path","content"]
		}`)),
		llm.NewTool("list_files", "List one directory in the workspace.", json.RawMessage(`{
			"type":"object",
			"properties":{"path":{"type":"string","description":"Directory relative to the workspace (default .)"}}
		}`)),
	}
	if t.allowShell {
		tools = append(tools, llm.NewTool("run_command", "Run a shell command inside the workspace. Returns exit code, stdout, and stderr.", json.RawMessage(`{
			"type":"object",
			"properties":{
				"command":{"type":"string","description":"Command passed to /bin/sh -c"},
				"timeout_seconds":{"type":"integer","description":"Optional timeout in seconds"}
			},
			"required":["command"]
		}`)))
	}
	if t.artifacts != nil {
		tools = append(tools, llm.NewTool("read_artifact", "Read a slice of a read-only input artifact. The result reports total bytes and next_offset; call again with offset=next_offset to continue.", json.RawMessage(`{
			"type":"object",
			"properties":{
				"key":{"type":"string","description":"Artifact key from the prompt"},
				"offset":{"type":"integer","description":"Byte offset to start reading (default 0)"},
				"limit":{"type":"integer","description":"Maximum bytes to return (default 24576)"}
			},
			"required":["key"]
		}`)))
	}
	if t.mcp != nil {
		for _, mt := range t.mcp.Tools() {
			schema := mt.InputSchema
			if len(schema) == 0 || string(schema) == "null" {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			desc := mt.Description
			if desc == "" {
				desc = "MCP tool " + mt.Name + " from server " + mt.Server
			}
			tools = append(tools, llm.NewTool(mt.Name, desc, schema))
		}
	}
	return append(tools, t.pluginTools()...)
}

// pluginTools declares the granted plugin tools, sorted by name. A local
// plugin supplies its own description and schema; a service tool carries the
// gateway's discovery copy. A granted tool with no local process and no remote
// placement is not offered, because the runtime could not run it.
func (t *toolset) pluginTools() []llm.Tool {
	names := make([]string, 0, len(t.grant))
	for name, tool := range t.grant {
		if tool.Placement == gateway.MCPStdio || tool.Placement == gateway.MCPHTTP {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	tools := make([]llm.Tool, 0, len(names))
	for _, name := range names {
		tool := t.grant[name]
		var desc string
		schema := tool.InputSchema
		if local, ok := t.localTool(name); ok {
			desc, schema = local.Description, local.InputSchema
		} else if tool.Placement != gateway.Service {
			continue
		}
		if desc == "" {
			desc = "Plugin tool " + name + " from plugin " + tool.Owner
		}
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, llm.NewTool(name, desc, schema))
	}
	return tools
}

// localTool returns the plugin host's view of one tool.
func (t *toolset) localTool(name string) (pluginhost.Tool, bool) {
	if t.plugins == nil {
		return pluginhost.Tool{}, false
	}
	return t.plugins.Tool(name)
}

// Call executes a built-in tool or reaches an MCP server the gateway does not
// grant. A granted tool goes through Runtime.callGranted instead, so the
// gateway sees it before and after.
func (t *toolset) Call(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	switch name {
	case "read_file":
		return t.readFile(args)
	case "write_file":
		return t.writeFile(args)
	case "list_files":
		return t.listFiles(args)
	case "run_command":
		if !t.allowShell {
			return "", false, errors.New("run_command is disabled for this session")
		}
		return t.runCommand(ctx, args)
	case "read_artifact":
		return t.readArtifact(ctx, args)
	}
	if t.mcp != nil {
		callCtx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
		defer cancel()
		out, isErr, err := t.mcp.Call(callCtx, name, args)
		if err != nil {
			return "", false, err
		}
		return truncate(out, maxToolOutput), isErr, nil
	}
	return "", false, fmt.Errorf("unknown tool %q", name)
}

// Dispatch runs one tool the gateway authorized. An inline or sidecar plugin
// runs in the plugin host with the ledger key and call token; a stdio MCP
// server runs through the manager. The caller bounds ctx by the gateway's
// timeout.
func (t *toolset) Dispatch(ctx context.Context, tool gateway.DiscoveryTool, args json.RawMessage, key, token string) (string, bool, error) {
	switch tool.Placement {
	case gateway.Inline, gateway.Sidecar:
		if t.plugins == nil {
			return "", false, fmt.Errorf("plugin %q is not running", tool.Owner)
		}
		return t.plugins.Call(ctx, tool.Name, args, key, token)
	case gateway.MCPStdio:
		if t.mcp == nil {
			return "", false, fmt.Errorf("MCP server %q is not connected", tool.Owner)
		}
		out, isErr, err := t.mcp.Call(ctx, tool.Name, args)
		if err != nil {
			return "", false, err
		}
		return truncate(out, maxToolOutput), isErr, nil
	}
	return "", false, fmt.Errorf("tool %q runs at the gateway, not in this runtime", tool.Name)
}

// toolGateway is the part of the platform client the runtime uses to
// authorize and record tool calls. *platformclient.Client implements it.
type toolGateway interface {
	Tools(ctx context.Context) (gateway.Discovery, error)
	BeginCall(ctx context.Context, req gateway.BeginRequest) (gateway.BeginResponse, error)
	CompleteCall(ctx context.Context, req gateway.CompleteRequest) error
}

// localDispatch performs the local half of a tool call the gateway
// authorized.
type localDispatch func(ctx context.Context, tool gateway.DiscoveryTool, args json.RawMessage, key, token string) (string, bool, error)

// toolResult is what one tool call produced for its journal step.
type toolResult struct {
	Content string
	IsError bool
	// Executed reports that this runtime ran the tool now.
	Executed bool
	// Replayed reports a result the gateway's ledger already held.
	Replayed bool
	// Interrupted reports a non-idempotent call a restart cut short. The
	// gateway did not run it again.
	Interrupted bool
}

// gatewayRetryBudget bounds one gateway request across its attempts, as
// modelRetryBudget bounds a model step. It covers a control-plane restart; a
// partition that outlasts it ends the process so a replacement resumes.
var gatewayRetryBudget = 5 * time.Minute

// gatewayAttemptTimeout bounds one attempt of a request the gateway answers
// from memory or the ledger.
const gatewayAttemptTimeout = 15 * time.Second

// gatewayDiscoveryAttempt bounds one attempt to read the tool grant. A
// session's first discovery handshakes with every remote owner, so it gets
// more room than a ledger read.
const gatewayDiscoveryAttempt = 2 * gatewayAttemptTimeout

// gatewayRemoteMargin extends a remote tool's begin attempt past the tool's
// own timeout: the gateway answers a remote call only after the upstream call
// finishes.
const gatewayRemoteMargin = 15 * time.Second

// gatewayRetryBackoff is the first wait between attempts. It doubles up to
// five seconds so a long outage does not hammer the control plane.
var gatewayRetryBackoff = 500 * time.Millisecond

// runtimeInstance names this runtime process in every begin request. The
// gateway re-issues a pending call that carries the same name instead of
// interrupting it: this process never received the answer, so the tool never
// ran. A replacement container is a new process with a new name.
var runtimeInstance = newRuntimeInstance()

// newRuntimeInstance returns a random nonce for this process.
func newRuntimeInstance() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(raw[:])
}

// beginAttempt bounds one attempt to authorize one call. A remote placement
// runs the upstream call inside the gateway's begin, so its attempt must
// outlast the tool's own timeout. A local placement is answered from the
// ledger or dispatched at once.
func beginAttempt(tool gateway.DiscoveryTool) time.Duration {
	if tool.Placement != gateway.Service && tool.Placement != gateway.MCPHTTP {
		return gatewayAttemptTimeout
	}
	timeout := time.Duration(tool.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = mcpCallTimeout
	}
	return timeout + gatewayRemoteMargin
}

// callTool runs one tool. A granted tool goes through the gateway; every other
// tool runs in the local toolset.
func (r *Runtime) callTool(ctx context.Context, turn int64, step int, call llm.ToolCall) (toolResult, error) {
	args := call.Function.Arguments
	if args == "" {
		args = "{}"
	}
	if tool, ok := r.tools.Granted(call.Function.Name); ok {
		return r.callGranted(ctx, turn, step, call, tool)
	}
	out, isErr, err := r.tools.Call(ctx, call.Function.Name, json.RawMessage(args))
	if err != nil {
		return toolResult{Content: "ERROR: " + err.Error(), IsError: true, Executed: true}, nil
	}
	return toolResult{Content: out, IsError: isErr, Executed: true}, nil
}

// callGranted asks the gateway to authorize one call, then either uses the
// result it already holds or runs the tool locally and reports the result
// back. The gateway records the call before the step reaches the journal, so a
// replacement container can answer the same step without running the tool
// again.
func (r *Runtime) callGranted(ctx context.Context, turn int64, step int, call llm.ToolCall, tool gateway.DiscoveryTool) (toolResult, error) {
	args := json.RawMessage(call.Function.Arguments)
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	resp, err := r.beginCall(ctx, tool, gateway.BeginRequest{
		Turn:       turn,
		Step:       step,
		CallID:     call.ID,
		Tool:       tool.Name,
		InstanceID: runtimeInstance,
		Arguments:  args,
	})
	switch {
	case err == nil:
	case errors.Is(err, platformclient.ErrToolNotGranted), errors.Is(err, platformclient.ErrToolRateLimited):
		// The gateway refused this call; the model gets the server's message
		// as an error result. A rate limit is never retried here: waiting is
		// what the limit exists to prevent.
		return toolResult{Content: err.Error(), IsError: true}, nil
	default:
		// A conflicting key, an unreserved turn, or a gateway that stayed
		// unreachable for the whole budget cannot be repaired in place.
		return toolResult{}, fatal(err)
	}
	if resp.Result != nil {
		return toolResult{
			Content:     resp.Result.Content,
			IsError:     resp.Result.IsError,
			Replayed:    resp.Result.Replayed,
			Interrupted: resp.Result.Interrupted,
		}, nil
	}
	if resp.Dispatch == nil {
		return toolResult{}, fatal(fmt.Errorf("gateway returned neither a result nor a dispatch for %q", tool.Name))
	}
	dispatch := resp.Dispatch
	timeout := time.Duration(dispatch.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = mcpCallTimeout
	}
	if r.dispatch == nil {
		return toolResult{}, fatal(fmt.Errorf("no local dispatcher for tool %q", tool.Name))
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	out, isErr, callErr := r.dispatch(callCtx, tool, args, dispatch.Key, dispatch.Token)
	cancel()
	if callErr != nil {
		out = "ERROR: " + callErr.Error()
		isErr = true
	}
	out = truncate(out, maxToolOutput)
	if err := r.completeCall(ctx, gateway.CompleteRequest{
		Key:     dispatch.Key,
		Content: out,
		IsError: isErr,
	}); err != nil {
		return toolResult{}, err
	}
	return toolResult{Content: out, IsError: isErr, Executed: true}, nil
}

// beginCall authorizes one call, retrying while the gateway is unreachable.
// A remote tool's attempt outlasts its own timeout: the gateway runs the
// upstream call inside this request.
func (r *Runtime) beginCall(ctx context.Context, tool gateway.DiscoveryTool, req gateway.BeginRequest) (gateway.BeginResponse, error) {
	attempt := beginAttempt(tool)
	return retryGateway(ctx, r.logger, "begin tool call "+req.Tool, attempt, gatewayRetryBudget+attempt,
		func(ctx context.Context) (gateway.BeginResponse, error) {
			return r.gateway.BeginCall(ctx, req)
		})
}

// completeCall records the result of a local dispatch, retrying while the
// gateway is unreachable. A call that ran must not be journaled without its
// record, or a replacement would run it again.
func (r *Runtime) completeCall(ctx context.Context, req gateway.CompleteRequest) error {
	_, err := retryGateway(ctx, r.logger, "complete tool call "+req.Key,
		gatewayAttemptTimeout, gatewayRetryBudget,
		func(ctx context.Context) (struct{}, error) {
			return struct{}{}, r.gateway.CompleteCall(ctx, req)
		})
	if err != nil {
		return fatal(err)
	}
	return nil
}

// retryGateway runs one idempotent gateway request until it succeeds, is
// rejected permanently, or the retry budget ends. A repeated request is safe:
// the gateway answers a call key from its ledger, and concurrent begins of one
// key collapse into one dispatch.
func retryGateway[T any](ctx context.Context, logger *slog.Logger, op string, attempt, budget time.Duration, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	backoff := gatewayRetryBackoff
	for {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, attempt)
		result, err := fn(attemptCtx)
		attemptCancel()
		if err == nil {
			return result, nil
		}
		// One attempt can time out while the budget still has room, which is
		// exactly the case a retry is for.
		if !retryableGatewayError(err) && !(errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil) {
			var zero T
			return zero, err
		}
		logger.Warn("tool gateway request failed; retrying", "request", op, "error", err)
		select {
		case <-ctx.Done():
			var zero T
			return zero, fmt.Errorf("%s: %w", op, err)
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// retryableGatewayError reports whether a gateway request can be repeated. A
// repeated begin answers from the ledger, so retrying is always safe; the
// statuses that cannot change are the ones the gateway uses to report a
// decision.
func retryableGatewayError(err error) bool {
	var tool *platformclient.ToolError
	if errors.As(err, &tool) {
		switch tool.Status {
		case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound,
			http.StatusConflict, http.StatusTooManyRequests:
			return false
		}
		return tool.Status >= 500 || tool.Status == http.StatusRequestTimeout
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// permanentGatewayError reports a gateway answer that another attempt cannot
// change, such as a session the gateway does not know. Everything else, an
// unreachable control plane included, is worth a replacement runtime.
func permanentGatewayError(err error) bool {
	var tool *platformclient.ToolError
	if !errors.As(err, &tool) {
		return false
	}
	switch tool.Status {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict:
		return true
	}
	return false
}

func (t *toolset) readFile(args json.RawMessage) (string, bool, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", false, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.Path == "" {
		return "", false, errors.New("path is required")
	}
	info, err := t.root.Stat(in.Path)
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return "", false, fmt.Errorf("%s is a directory", in.Path)
	}
	if info.Size() > maxReadBytes {
		return "", false, fmt.Errorf("file is too large (%d bytes, limit %d)", info.Size(), maxReadBytes)
	}
	data, err := t.root.ReadFile(in.Path)
	if err != nil {
		return "", false, err
	}
	return string(data), false, nil
}

func (t *toolset) writeFile(args json.RawMessage) (string, bool, error) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", false, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.Path == "" {
		return "", false, errors.New("path is required")
	}
	if len(in.Content) > maxWriteBytes {
		return "", false, fmt.Errorf("content is too large (%d bytes, limit %d)", len(in.Content), maxWriteBytes)
	}
	if dir := filepath.Dir(in.Path); dir != "." && dir != "" {
		if err := t.root.MkdirAll(dir, 0o755); err != nil {
			return "", false, err
		}
	}
	if err := t.root.WriteFile(in.Path, []byte(in.Content), 0o644); err != nil {
		return "", false, err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), false, nil
}

func (t *toolset) listFiles(args json.RawMessage) (string, bool, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", false, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.Path == "" {
		in.Path = "."
	}
	f, err := t.root.Open(in.Path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	entries, err := f.ReadDir(maxListedEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, err
	}
	type row struct {
		Name string `json:"name"`
		Size int64  `json:"size,omitempty"`
		Dir  bool   `json:"dir,omitempty"`
	}
	rows := make([]row, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		r := row{Name: filepath.ToSlash(filepath.Join(in.Path, e.Name())), Dir: e.IsDir()}
		if !e.IsDir() {
			r.Size = info.Size()
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	out, _ := json.MarshalIndent(rows, "", "  ")
	if len(entries) > maxListedEntries {
		return string(out) + "\n(truncated)", false, nil
	}
	return string(out), false, nil
}

func (t *toolset) runCommand(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var in struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", false, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(in.Command) == "" {
		return "", false, errors.New("command is required")
	}
	timeout := t.shellTimeout
	if in.TimeoutSeconds > 0 {
		requested := time.Duration(in.TimeoutSeconds) * time.Second
		if requested < timeout {
			timeout = requested
		}
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command("/bin/sh", "-c", in.Command)
	cmd.Dir = t.rootPath
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout := newCapBuffer(maxToolOutput)
	stderr := newCapBuffer(maxToolOutput)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return "", false, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		waitErr = <-done
	}
	exitCode := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if waitErr != nil {
		return "", false, waitErr
	}
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d", exitCode)
	if runCtx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(&b, "\ntimed_out: true")
	}
	b.WriteString("\nstdout:\n")
	b.WriteString(stdout.String())
	if stdout.Truncated() {
		b.WriteString("\n(stdout truncated)")
	}
	b.WriteString("\nstderr:\n")
	b.WriteString(stderr.String())
	if stderr.Truncated() {
		b.WriteString("\n(stderr truncated)")
	}
	return b.String(), exitCode != 0, nil
}

// capBuffer stores at most max bytes and remembers truncation.
type capBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func newCapBuffer(max int) *capBuffer { return &capBuffer{max: max} }

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.max - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *capBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n(truncated at %d bytes)", max)
}

// artifactWindowBytes bounds one read so the JSON envelope stays under the
// tool output cap even when the payload needs escaping.
const artifactWindowBytes = 24 << 10

func (t *toolset) readArtifact(ctx context.Context, args json.RawMessage) (string, bool, error) {
	if t.artifacts == nil {
		return "", false, errors.New("this session has no input artifacts")
	}
	var in struct {
		Key    string `json:"key"`
		Offset int64  `json:"offset"`
		Limit  int64  `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", false, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(in.Key) == "" {
		return "", false, errors.New("key is required")
	}
	if in.Offset < 0 {
		return "", false, errors.New("offset must not be negative")
	}
	limit := in.Limit
	if limit <= 0 || limit > artifactWindowBytes {
		limit = artifactWindowBytes
	}
	window, err := t.artifacts.ReadArtifact(ctx, in.Key, in.Offset, limit)
	if err != nil {
		return "", true, err
	}
	envelope := map[string]any{
		"key":          window.Key,
		"content_type": window.ContentType,
		"offset":       window.Offset,
		"bytes":        window.Bytes,
		"total":        window.Total,
		"next_offset":  window.NextOffset,
	}
	content := window.Content
	for {
		envelope["content"] = content
		envelope["bytes"] = int64(len(content))
		if len(content) == 0 || window.NextOffset == nil {
			envelope["next_offset"] = nil
		} else {
			next := window.Offset + int64(len(content))
			envelope["next_offset"] = next
		}
		out, err := json.Marshal(envelope)
		if err != nil {
			return "", false, err
		}
		if len(out) <= maxToolOutput {
			return string(out), false, nil
		}
		if len(content) < 2 {
			return "", false, errors.New("artifact window does not fit the tool output limit")
		}
		content = content[:len(content)/2]
	}
}
