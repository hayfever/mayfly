// Package pluginhost starts and supervises the inline plugins of one session.
//
// A plugin is a stdio MCP server. The host gives it a private environment,
// verifies the tools the manifest declares, hides a tool the manifest does not
// declare, restarts a crashed process within the manifest's budget, and never
// repeats a call that may have reached the plugin.
package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hayfever/mayfly/internal/mcp"
	"github.com/hayfever/mayfly/internal/plugin"
	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// defaultObjectSchema is the input schema of a tool that declares none.
const defaultObjectSchema = `{"type":"object","properties":{}}`

// Options are the session facts a plugin process needs.
type Options struct {
	SessionID string
	// Agent is the agent name reported to the plugin in _meta.
	Agent string
	// WorkspaceDir is the session workspace, also the working directory of a
	// plugin whose manifest says workdir: workspace.
	WorkspaceDir string
	// ArtifactsDir holds the plugin stderr logs. When empty, stderr is only
	// logged and kept for error results.
	ArtifactsDir string
	Logger       *slog.Logger
}

// Tool is one declared plugin tool, ready for the model.
type Tool struct {
	// Name is the model-visible name: plugin_<plugin>_<tool>.
	Name        string
	Plugin      string
	Remote      string
	Description string
	InputSchema json.RawMessage
	Idempotent  bool
	Timeout     time.Duration
	MaxOutput   int
}

// Host owns the inline plugin processes of one session.
type Host struct {
	opts   Options
	logger *slog.Logger

	mu       sync.Mutex
	plugins  []*inline
	byName   map[string]Tool
	byPlugin map[string]*inline
	tools    []Tool
	hidden   map[string][]string
	required []error
	closed   bool
}

// inline is one plugin process and its restart budget.
type inline struct {
	def  sessioncfg.Plugin
	opts Options
	sink *stderrSink

	mu       sync.Mutex
	client   *mcp.Client
	restarts int
}

// Start launches every plugin that has a local directory. A plugin without
// one runs in another container (sidecar, service) and is skipped. Failures
// are returned per plugin; Start keeps the plugins that did start.
func Start(ctx context.Context, plugins []sessioncfg.Plugin, opts Options) (*Host, []error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h := &Host{
		opts:     opts,
		logger:   logger,
		byName:   make(map[string]Tool),
		byPlugin: make(map[string]*inline),
		hidden:   make(map[string][]string),
	}
	var errs []error
	for _, def := range plugins {
		if def.Dir == "" {
			logger.Debug("plugin has no local directory", "plugin", def.Name, "placement", def.Placement)
			continue
		}
		if err := h.add(ctx, def); err != nil {
			err = fmt.Errorf("plugin %q: %w", def.Name, err)
			errs = append(errs, err)
			if def.Required {
				h.required = append(h.required, err)
			}
		}
	}
	sort.Slice(h.tools, func(i, j int) bool { return h.tools[i].Name < h.tools[j].Name })
	return h, errs
}

// add starts one plugin and publishes its tools.
func (h *Host) add(ctx context.Context, def sessioncfg.Plugin) error {
	sink, err := newStderrSink(def.Name, h.opts.ArtifactsDir, def.StderrBytes, h.logger)
	if err != nil {
		return err
	}
	rt := &inline{def: def, opts: h.opts, sink: sink}
	reported, hidden, err := rt.start(ctx)
	if err != nil {
		rt.close()
		return err
	}
	tools := buildTools(def, reported)
	for _, tool := range tools {
		if other, ok := h.byName[tool.Name]; ok {
			rt.close()
			return fmt.Errorf("tool %q collides with plugin %q", tool.Name, other.Plugin)
		}
	}
	h.plugins = append(h.plugins, rt)
	h.byPlugin[def.Name] = rt
	h.hidden[def.Name] = hidden
	for _, tool := range tools {
		h.byName[tool.Name] = tool
		h.tools = append(h.tools, tool)
	}
	return nil
}

// RequiredErr joins the failures of required plugins, or returns nil.
func (h *Host) RequiredErr() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return errors.Join(h.required...)
}

// Tools returns every model-visible tool, sorted by name.
func (h *Host) Tools() []Tool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Tool, len(h.tools))
	copy(out, h.tools)
	return out
}

// Tool returns one tool by its model-visible name.
func (h *Host) Tool(name string) (Tool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tool, ok := h.byName[name]
	return tool, ok
}

// Hidden returns the tools a plugin reported but the manifest does not
// declare, by plugin name. They are not offered to the model.
func (h *Host) Hidden() map[string][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string][]string, len(h.hidden))
	for name, tools := range h.hidden {
		out[name] = append([]string(nil), tools...)
	}
	return out
}

// Call invokes one plugin tool. key is the gateway's idempotency key and
// token is the gateway call token of a sidecar plugin; an inline plugin is
// addressed through its stdin and needs no token.
//
// An error result (isErr true) means the tool ran or may have run and failed.
// A non-nil error means the host could not dispatch the call at all: the tool
// is unknown, or the plugin is down and its restart budget is spent.
func (h *Host) Call(ctx context.Context, name string, args json.RawMessage, key, token string) (string, bool, error) {
	_ = token
	h.mu.Lock()
	closed := h.closed
	tool, ok := h.byName[name]
	rt := h.byPlugin[tool.Plugin]
	h.mu.Unlock()
	if closed {
		return "", false, errors.New("plugin host is closed")
	}
	if !ok {
		return "", false, fmt.Errorf("unknown plugin tool %q", name)
	}
	if rt == nil {
		return "", false, fmt.Errorf("plugin %q is not running", tool.Plugin)
	}
	return rt.call(ctx, tool, args, key)
}

// Close stops every plugin in parallel and waits for them.
func (h *Host) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	plugins := append([]*inline(nil), h.plugins...)
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, rt := range plugins {
		wg.Add(1)
		go func(rt *inline) {
			defer wg.Done()
			rt.close()
		}(rt)
	}
	wg.Wait()
}

// start launches the process and verifies the tools it reports.
func (rt *inline) start(ctx context.Context) ([]mcp.Tool, []string, error) {
	env, err := baseEnv(rt.def, rt.opts.SessionID, rt.opts.WorkspaceDir)
	if err != nil {
		return nil, nil, err
	}
	command, err := resolveCommand(rt.def)
	if err != nil {
		return nil, nil, err
	}
	dir := rt.def.Dir
	if rt.def.Workdir == "workspace" && rt.opts.WorkspaceDir != "" {
		dir = rt.opts.WorkspaceDir
	}
	timeout := time.Duration(rt.def.InitTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	initCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client, err := mcp.NewStdioExact(initCtx, rt.def.Name, command, rt.def.Args, env, dir, rt.sink)
	if err != nil {
		return nil, nil, err
	}
	if err := client.Initialize(initCtx); err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	reported, err := client.ListTools(initCtx)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	hidden, err := verifyTools(rt.def, reported)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	rt.client = client
	return reported, hidden, nil
}

// close stops the process and the stderr sink.
func (rt *inline) close() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.client != nil {
		_ = rt.client.Close()
		rt.client = nil
	}
	rt.sink.Close()
}

// call runs one tool under its timeout. The caller holds no lock.
func (rt *inline) call(ctx context.Context, tool Tool, args json.RawMessage, key string) (string, bool, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if err := rt.ensure(ctx); err != nil {
		return "", false, err
	}
	meta := map[string]any{
		"mayfly.io/idempotency-key": key,
		"mayfly.io/session":         rt.opts.SessionID,
		"mayfly.io/agent":           rt.opts.Agent,
	}
	out, isErr, id, err := rt.dispatch(ctx, tool, args, meta)
	if err == nil {
		return truncate(out, tool.MaxOutput), isErr, nil
	}
	if ctx.Err() != nil {
		rt.cancel(id, "cancelled")
		return "", false, ctx.Err()
	}
	if isTimeout(ctx, err) {
		rt.cancel(id, "timeout")
		return timeoutResult(tool), true, nil
	}
	if errors.Is(err, mcp.ErrNotSent) {
		// The request never reached the plugin, so it cannot have taken
		// effect. Restart within the budget and send it once.
		rt.stopClient()
		if restartErr := rt.restart(ctx); restartErr != nil {
			return "", false, restartErr
		}
		out, isErr, id, err = rt.dispatch(ctx, tool, args, meta)
		if err == nil {
			return truncate(out, tool.MaxOutput), isErr, nil
		}
		if ctx.Err() != nil {
			rt.cancel(id, "cancelled")
			return "", false, ctx.Err()
		}
		if isTimeout(ctx, err) {
			rt.cancel(id, "timeout")
			return timeoutResult(tool), true, nil
		}
	}
	if rt.exited() {
		// The plugin exited while the call was in flight: the tool may have
		// run, so the call is not repeated.
		rt.stopClient()
		return exitResult(tool, strings.TrimSpace(rt.sink.Tail())), true, nil
	}
	// The plugin answered with an error of its own and is still running.
	return fmt.Sprintf("plugin %q: tool %q failed: %v", tool.Plugin, tool.Remote, err), true, nil
}

// exited reports whether the plugin process is gone.
func (rt *inline) exited() bool {
	if rt.client == nil {
		return true
	}
	exited := rt.client.Exited()
	if exited == nil {
		return false
	}
	select {
	case <-exited:
		return true
	default:
		return false
	}
}

// stopClient drops a client whose process is gone.
func (rt *inline) stopClient() {
	if rt.client != nil {
		_ = rt.client.Close()
		rt.client = nil
	}
}

// dispatch sends one tools/call under the tool's timeout.
func (rt *inline) dispatch(ctx context.Context, tool Tool, args json.RawMessage, meta map[string]any) (string, bool, int64, error) {
	timeout := tool.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return rt.client.CallToolMeta(callCtx, tool.Remote, args, meta)
}

// cancel tells the plugin that a request will not get a result.
func (rt *inline) cancel(id int64, reason string) {
	if rt.client == nil {
		return
	}
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	_ = rt.client.Cancel(ctx, id, reason)
}

// ensure restarts a plugin that exited before the call.
func (rt *inline) ensure(ctx context.Context) error {
	if rt.client == nil {
		return rt.restart(ctx)
	}
	select {
	case <-rt.client.Exited():
		rt.stopClient()
		return rt.restart(ctx)
	default:
		return nil
	}
}

// restart replaces an exited plugin while the budget allows it.
func (rt *inline) restart(ctx context.Context) error {
	if rt.restarts >= rt.def.MaxRestarts {
		tail := strings.TrimSpace(rt.sink.Tail())
		if tail != "" {
			return fmt.Errorf("plugin %q exited and the restart limit of %d is reached: %s", rt.def.Name, rt.def.MaxRestarts, tail)
		}
		return fmt.Errorf("plugin %q exited and the restart limit of %d is reached", rt.def.Name, rt.def.MaxRestarts)
	}
	rt.restarts++
	if _, _, err := rt.start(ctx); err != nil {
		return fmt.Errorf("plugin %q restart %d: %w", rt.def.Name, rt.restarts, err)
	}
	return nil
}

// isTimeout reports a call that ended because its own deadline passed, not
// because the runtime cancelled the work.
func isTimeout(parent context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil
}

// timeoutResult is the error result of a call that did not answer in time.
func timeoutResult(tool Tool) string {
	return fmt.Sprintf("plugin %q: tool %q did not answer within %s", tool.Plugin, tool.Remote, tool.Timeout)
}

// exitResult is the error result of a call whose plugin exited.
func exitResult(tool Tool, tail string) string {
	message := fmt.Sprintf("plugin %q exited during tool %q", tool.Plugin, tool.Remote)
	if tail != "" {
		message += ": " + tail
	}
	return message
}

// verifyTools checks the reported tools against the manifest's allowlist.
// A declared tool must be reported, with an object input schema. A reported
// tool the manifest does not declare is hidden.
func verifyTools(def sessioncfg.Plugin, reported []mcp.Tool) ([]string, error) {
	byRemote := make(map[string]mcp.Tool, len(reported))
	for _, tool := range reported {
		byRemote[tool.Name] = tool
	}
	declared := make(map[string]bool, len(def.Tools))
	for _, tool := range def.Tools {
		rep, ok := byRemote[tool.Name]
		if !ok {
			return nil, fmt.Errorf("declared tool %q is not reported by the plugin", tool.Name)
		}
		if !isObjectSchema(rep.InputSchema) {
			return nil, fmt.Errorf("tool %q: input schema is not a JSON object", tool.Name)
		}
		declared[tool.Name] = true
	}
	var hidden []string
	for name := range byRemote {
		if !declared[name] {
			hidden = append(hidden, name)
		}
	}
	sort.Strings(hidden)
	return hidden, nil
}

// buildTools turns the verified reported tools into model-visible tools.
func buildTools(def sessioncfg.Plugin, reported []mcp.Tool) []Tool {
	byRemote := make(map[string]mcp.Tool, len(reported))
	for _, tool := range reported {
		byRemote[tool.Name] = tool
	}
	tools := make([]Tool, 0, len(def.Tools))
	for _, want := range def.Tools {
		rep := byRemote[want.Name]
		schema := rep.InputSchema
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(defaultObjectSchema)
		}
		tools = append(tools, Tool{
			Name:        plugin.ToolName(def.Name, want.Name),
			Plugin:      def.Name,
			Remote:      want.Name,
			Description: rep.Description,
			InputSchema: schema,
			Idempotent:  want.Idempotent,
			Timeout:     time.Duration(want.TimeoutSeconds) * time.Second,
			MaxOutput:   def.MaxOutputBytes,
		})
	}
	return tools
}

// isObjectSchema reports whether raw is a JSON object, or empty so that the
// default schema applies.
func isObjectSchema(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var obj map[string]any
	return json.Unmarshal(raw, &obj) == nil
}

// resolveCommand applies the manifest's command rule: an absolute path is
// used as is, a path with a separator is relative to the plugin directory,
// and a bare name is looked up on the plugin's PATH.
func resolveCommand(def sessioncfg.Plugin) (string, error) {
	if filepath.IsAbs(def.Command) {
		return def.Command, nil
	}
	if strings.ContainsRune(def.Command, '/') {
		return filepath.Join(def.Dir, def.Command), nil
	}
	path := pluginPath(def.Dir)
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, def.Command)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("command %q not found on PATH %s", def.Command, path)
}

// truncate matches the runtime's tool output cap: the content keeps its first
// max bytes and gains one marker.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n(truncated at %d bytes)", max)
}
