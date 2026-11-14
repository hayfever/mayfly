package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/llm"
	"github.com/hayfever/mayfly/internal/mcp"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/platformclient"
	"github.com/hayfever/mayfly/internal/pluginhost"
	"github.com/hayfever/mayfly/internal/sessioncfg"
	"github.com/hayfever/mayfly/internal/version"
)

// Runtime is the in-container agent session.
type Runtime struct {
	cfg        sessioncfg.Config
	token      string
	logger     *slog.Logger
	platform   *platformclient.Client
	gateway    toolGateway
	llm        *llm.Client
	mcp        *mcp.Manager
	plugins    *pluginhost.Host
	tools      *toolset
	toolsList  []llm.Tool
	conv       *conversation
	journal    journal.Store
	trace      *tracer
	events     *emitter
	system     string
	messages   atomic.Int64
	toolCalls  atomic.Int64
	finalizeMu sync.Mutex
	finalized  bool
	finalErr   error

	// dispatch runs the local half of a granted call. Run points it at the
	// toolset; unit tests answer calls without starting a plugin process.
	dispatch localDispatch

	// resumeMu guards resumeEv, the inbox event the runtime replayed from the
	// journal. Its redelivery is skipped by identity, not by sequence number:
	// sequence numbers are reused after a control-plane restart.
	resumeMu sync.Mutex
	resumeEv *model.Event
	// fatalErr receives the first failure that cannot be handled in place. Run
	// then exits without finalizing so the control plane starts a replacement
	// that resumes from the journal.
	fatalCh chan error
}

// New prepares a runtime. It does not start any network or process activity.
func New(cfg sessioncfg.Config, token string, logger *slog.Logger) (*Runtime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Harness != "" && cfg.Harness != "mayfly" {
		return nil, fmt.Errorf("harness %q requires the mayfly-harness dispatcher; do not run mayfly-agent directly", cfg.Harness)
	}
	if logger == nil {
		logger = slog.Default()
	}
	for _, dir := range []string{cfg.WorkspaceDir, cfg.ArtifactsDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	platform := platformclient.New(cfg.PlatformURL, cfg.SessionID, token)
	platform.SetLogger(logger)

	conv, err := openConversation(filepath.Join(cfg.ArtifactsDir, "conversation.jsonl"))
	if err != nil {
		return nil, err
	}
	trace, err := openTracer(filepath.Join(cfg.ArtifactsDir, "traces.jsonl"))
	if err != nil {
		_ = conv.Close()
		return nil, err
	}
	events, err := newEmitter(platform, filepath.Join(cfg.ArtifactsDir, "events.jsonl"), logger)
	if err != nil {
		_ = conv.Close()
		_ = trace.Close()
		return nil, err
	}
	return &Runtime{
		cfg:      cfg,
		token:    token,
		logger:   logger,
		platform: platform,
		gateway:  platform,
		llm:      llm.New(cfg.LLMBaseURL, token, cfg.Model),
		conv:     conv,
		journal:  platform,
		trace:    trace,
		events:   events,
		system:   systemPrompt(cfg),
		fatalCh:  make(chan error, 1),
	}, nil
}

// Run starts MCP servers, consumes the inbox, and finalizes the session.
func (r *Runtime) Run(ctx context.Context) error {
	r.logger.Info("session starting",
		"session", r.cfg.SessionID,
		"model", r.cfg.Model,
		"skills", len(r.cfg.Skills),
		"mcp_servers", len(r.cfg.MCPServers),
		"plugins", len(r.cfg.Plugins),
	)

	r.mcp = mcp.Start(ctx, r.cfg.MCPServers, r.logger)
	for _, err := range r.mcp.Errors() {
		r.events.Emit(ctx, model.EventSystem, "mayfly", "MCP server unavailable: "+err.Error(), nil)
	}
	if err := r.startPlugins(ctx); err != nil {
		r.stopChildren()
		return r.finalize(context.Background(), "failed", err)
	}
	grant, wildcards, sidecarTokens, err := r.discoverTools(ctx)
	if err != nil {
		if !permanentGatewayError(err) {
			// The control plane is unreachable, not wrong. Exit without
			// finalizing so a replacement starts a fresh runtime.
			r.closeLocal()
			return err
		}
		r.stopChildren()
		return r.finalize(context.Background(), "failed", err)
	}
	// A sidecar needs the discovery token the gateway issued above, so it
	// starts after the grant is known and before the model sees its tools.
	if err := r.startSidecars(ctx, sidecarTokens); err != nil {
		r.stopChildren()
		return r.finalize(context.Background(), "failed", err)
	}
	r.reportHidden(ctx)
	var artifacts ArtifactReader
	if len(r.cfg.Artifacts) > 0 {
		artifacts = r.platform
	}
	tools, err := newToolset(toolsetConfig{
		Workspace:    r.cfg.WorkspaceDir,
		AllowShell:   r.cfg.AllowShell,
		ShellTimeout: time.Duration(r.cfg.ShellTimeoutSecs) * time.Second,
		MCP:          r.mcp,
		Plugins:      r.plugins,
		Grant:        grant,
		Wildcards:    wildcards,
		Artifacts:    artifacts,
	})
	if err != nil {
		return r.finalize(context.Background(), "failed", err)
	}
	r.tools = tools
	r.dispatch = tools.Dispatch
	r.toolsList = tools.Tools()

	skillNames := make([]string, 0, len(r.cfg.Skills))
	for _, s := range r.cfg.Skills {
		skillNames = append(skillNames, s.Name)
	}
	mcpNames := make([]string, 0, len(r.cfg.MCPServers))
	for _, s := range r.cfg.MCPServers {
		mcpNames = append(mcpNames, s.Name)
	}
	pluginNames := make([]string, 0, len(r.cfg.Plugins))
	for _, p := range r.cfg.Plugins {
		pluginNames = append(pluginNames, p.Name)
	}
	r.events.Emit(ctx, model.EventSystem, "mayfly", "Session started. Attach to interact.", map[string]any{
		"model":       r.cfg.Model,
		"skills":      skillNames,
		"mcp_servers": mcpNames,
		"plugins":     pluginNames,
		"tool_count":  len(r.toolsList),
	})

	userCh := make(chan model.Event, 32)
	type controlMsg struct {
		action string
		reason string
	}
	controlCh := make(chan controlMsg, 4)
	workCh := make(chan model.Event, 32)

	turnCtx, cancelTurns := context.WithCancel(context.Background())
	turnDone := make(chan struct{})
	turnFinished := make(chan struct{}, 32)
	go func() {
		defer close(turnDone)
		for {
			select {
			case <-turnCtx.Done():
				return
			case ev := <-workCh:
				r.handleTurn(turnCtx, ev)
				// Every started turn reports exactly once. Blocking here is
				// safe because shutdown cancels turnCtx before waiting.
				select {
				case turnFinished <- struct{}{}:
				case <-turnCtx.Done():
					return
				}
			}
		}
	}()

	// The journal is the only source of truth for an interrupted turn. A
	// replacement container needs no flag to consult it, and a stale config
	// cannot make it skip the replay.
	var pending turnCounter
	resumeAfter := int64(0)
	after, resumed, err := r.prepareResume(ctx)
	if err != nil {
		cancelTurns()
		return r.finalize(context.Background(), "failed", err)
	}
	resumeAfter = after
	if resumed != nil {
		r.armResume(*resumed)
		workCh <- *resumed
		pending.start()
	}

	inboxCtx, cancelInbox := context.WithCancel(ctx)
	go func() {
		err := r.platform.Inbox(inboxCtx, resumeAfter, func(ev model.Event) {
			switch ev.Type {
			case model.EventUserMessage:
				if r.skipResumeEvent(ev) {
					return
				}
				select {
				case userCh <- ev:
				default:
					r.events.Emit(ctx, model.EventError, "mayfly", "Message queue is full; message dropped.", nil)
				}
			case model.EventControl:
				action, _ := ev.Data["action"].(string)
				reason, _ := ev.Data["reason"].(string)
				select {
				case controlCh <- controlMsg{action: action, reason: reason}:
				default:
				}
			}
		}, r.clearResumeEvent)
		if err != nil && !errors.Is(err, context.Canceled) && inboxCtx.Err() == nil {
			r.events.Emit(ctx, model.EventError, "mayfly", "Inbox stream failed: "+err.Error(), nil)
		}
	}()

	var idleC <-chan time.Time
	var idleTimer *time.Timer
	if r.cfg.IdleTimeoutSeconds > 0 {
		idleTimer = time.NewTimer(time.Duration(r.cfg.IdleTimeoutSeconds) * time.Second)
		idleC = idleTimer.C
	}
	var ttlC <-chan time.Time
	var ttlTimer *time.Timer
	if r.cfg.TTLSeconds > 0 {
		ttlTimer = time.NewTimer(time.Duration(r.cfg.TTLSeconds+60) * time.Second)
		ttlC = ttlTimer.C
	}
	resetIdle := func() {
		if idleTimer == nil {
			return
		}
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(time.Duration(r.cfg.IdleTimeoutSeconds) * time.Second)
	}

	reason := "closed"
	closed := false
	var stopErr error
	for {
		select {
		case ev := <-userCh:
			resetIdle()
			select {
			case workCh <- ev:
				pending.start()
			default:
				r.events.Emit(ctx, model.EventError, "mayfly", "Agent is busy; message dropped.", nil)
			}
		case <-turnFinished:
			pending.finish()
			// A long turn is not an idle session. The idle timer measures
			// silence from the user, not from the runtime.
			resetIdle()
		case msg := <-controlCh:
			if msg.action == "close" {
				closed = true
				if msg.reason != "" {
					reason = msg.reason
				}
				r.logger.Info("close requested", "reason", reason)
				goto shutdown
			}
		case <-idleC:
			if pending.active() {
				// A retrying or queued turn must not be finalized as idle.
				resetIdle()
				break
			}
			reason = "idle"
			goto shutdown
		case <-ttlC:
			reason = "ttl"
			goto shutdown
		case err := <-r.fatalCh:
			stopErr = err
			goto shutdown
		case <-ctx.Done():
			// A close event and the signal can arrive together; the close is
			// the control plane's authoritative instruction.
			select {
			case msg := <-controlCh:
				if msg.action == "close" {
					closed = true
					if msg.reason != "" {
						reason = msg.reason
					}
				}
			default:
			}
			if !closed {
				// SIGTERM is how Kubernetes drains, evicts, and deletes a
				// pod. A resumable session is not over: exit non-zero without
				// finalizing so the control plane replaces the container and
				// the replacement resumes the journaled turn. One that cannot
				// resume keeps its full bundle by finalizing here.
				signalReason, signalErr := signalStop(r.cfg.Resumable, ctx.Err())
				if signalReason != "" {
					reason = signalReason
				}
				stopErr = signalErr
			}
			goto shutdown
		}
	}

shutdown:
	cancelInbox()
	cancelTurns()
	select {
	case <-turnDone:
	case <-time.After(15 * time.Second):
		r.logger.Warn("agent turn did not stop before shutdown deadline")
	}
	if idleTimer != nil {
		idleTimer.Stop()
	}
	if ttlTimer != nil {
		ttlTimer.Stop()
	}
	if r.tools != nil {
		_ = r.tools.Close()
	}
	r.stopChildren()
	if stopErr != nil {
		r.logger.Warn("session stopped without finalizing", "error", stopErr)
		r.closeLogs()
		return stopErr
	}
	return r.finalize(context.Background(), reason, nil)
}

// startPlugins launches the session's inline plugins. A required plugin that
// did not start fails the session; an optional one is reported as a system
// event and its tools are not offered.
func (r *Runtime) startPlugins(ctx context.Context) error {
	host, errs := pluginhost.Start(ctx, r.cfg.Plugins, pluginhost.Options{
		SessionID:    r.cfg.SessionID,
		Agent:        r.cfg.Agent,
		WorkspaceDir: r.cfg.WorkspaceDir,
		ArtifactsDir: r.cfg.ArtifactsDir,
		Logger:       r.logger,
	})
	r.plugins = host
	for _, err := range errs {
		r.events.Emit(ctx, model.EventSystem, "mayfly", "Plugin unavailable: "+err.Error(), nil)
	}
	return host.RequiredErr()
}

// startSidecars connects the session's sidecar plugins with the discovery
// tokens the gateway issued. A required sidecar that did not connect fails the
// session; an optional one is reported as a system event.
func (r *Runtime) startSidecars(ctx context.Context, tokens map[string]gateway.SidecarToken) error {
	discovery := make(map[string]string, len(tokens))
	for name, token := range tokens {
		discovery[name] = token.DiscoveryToken
	}
	for _, err := range r.plugins.StartSidecars(ctx, discovery) {
		r.events.Emit(ctx, model.EventSystem, "mayfly", "Plugin unavailable: "+err.Error(), nil)
	}
	return r.plugins.RequiredErr()
}

// reportHidden names the tools a plugin exposes but its manifest does not
// declare, once per plugin.
func (r *Runtime) reportHidden(ctx context.Context) {
	hidden := r.plugins.Hidden()
	names := make([]string, 0, len(hidden))
	for name := range hidden {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r.events.Emit(ctx, model.EventSystem, "mayfly", fmt.Sprintf(
			"Plugin %s exposes tools that the manifest does not declare; they are hidden: %s",
			name, strings.Join(hidden[name], ", ")), nil)
	}
}

// discoverTools reads the session's tool grant from the gateway. Every granted
// tool and every tool of a wildcard stdio server goes through the gateway;
// anything else keeps its local path. The wildcard owners and sidecar discovery
// tokens are returned with the grant.
func (r *Runtime) discoverTools(ctx context.Context) (map[string]gateway.DiscoveryTool, map[string]gateway.Placement, map[string]gateway.SidecarToken, error) {
	discovery, err := retryGateway(ctx, r.logger, "list tools",
		gatewayDiscoveryAttempt, gatewayRetryBudget,
		func(ctx context.Context) (gateway.Discovery, error) {
			return r.gateway.Tools(ctx)
		})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read tool grant: %w", err)
	}
	var required []error
	for _, failure := range discovery.Errors {
		err := fmt.Errorf("%s: %s", failure.Owner, failure.Error)
		if failure.Required {
			required = append(required, err)
			continue
		}
		r.events.Emit(ctx, model.EventSystem, "mayfly", "Plugin unavailable: "+err.Error(), nil)
	}
	if len(required) > 0 {
		return nil, nil, nil, errors.Join(required...)
	}
	grant := make(map[string]gateway.DiscoveryTool, len(discovery.Tools))
	for _, tool := range discovery.Tools {
		grant[tool.Name] = tool
	}
	return grant, discovery.Wildcards, discovery.Sidecars, nil
}

// turnCounter counts turns that are queued or running. The idle timer must not
// finalize a session while any of them can still write to the conversation.
type turnCounter struct{ n int }

func (c *turnCounter) start() { c.n++ }

func (c *turnCounter) finish() {
	if c.n > 0 {
		c.n--
	}
}

func (c *turnCounter) active() bool { return c.n > 0 }

// signalStop decides what a signal without a close event means. A resumable
// session leaves its turn to a replacement; one that cannot resume finalizes
// with the full bundle instead.
func signalStop(resumable bool, cause error) (reason string, stopErr error) {
	if !resumable {
		return "terminated", nil
	}
	return "", fmt.Errorf("terminated before the control plane closed the session: %w", cause)
}

func (r *Runtime) handleTurn(ctx context.Context, ev model.Event) {
	if ctx.Err() != nil {
		return
	}
	turn := ev.Seq
	if turn <= 0 {
		r.failTurn(ctx, fatal(fmt.Errorf("inbox event %q has no sequence number", ev.Type)))
		return
	}
	entries, err := r.journal.Turn(ctx, r.cfg.SessionID, turn)
	if err != nil {
		r.failTurn(ctx, fatal(fmt.Errorf("turn journal read failed: %w", err)))
		return
	}
	if len(entries) > 0 {
		// The sequence is a key, not an identity. A turn that already has
		// entries must belong to this exact message before it is replayed or
		// extended.
		if err := verifyTurn(entries, ev); err != nil {
			r.failTurn(ctx, fatal(err))
			return
		}
	}

	step := 0
	userInput, _ := json.Marshal(ev)
	if err := checkStepName(entryAt(entries, step), "user"); err != nil {
		r.failTurn(ctx, fatal(err))
		return
	}
	if _, _, err := r.runOrReplay(ctx, turn, step, userInput, entryAt(entries, step),
		func() (stepRun, error) {
			msg := userMessage(ev)
			out, err := json.Marshal(userStepOutput{Message: msg})
			return stepRun{name: "user", message: msg, output: out}, err
		}, r.applyUser); err != nil {
		r.failTurn(ctx, err)
		return
	}
	r.messages.Add(1)
	step++

	for round := 0; round < r.cfg.MaxSteps; round++ {
		if ctx.Err() != nil {
			return
		}
		messages := r.conv.MessagesForModel(r.system, r.cfg.MaxHistoryMessages)
		modelInput, _ := json.Marshal(modelStepInput{
			Model:    r.cfg.Model,
			Messages: len(messages),
			Tools:    len(r.toolsList),
		})
		if existing := entryAt(entries, step); existing != nil && existing.Name != "llm" && existing.Name != "assistant" {
			r.failTurn(ctx, fatal(fmt.Errorf("journal step %d is %q, want llm or assistant", step, existing.Name)))
			return
		}
		entry, _, err := r.runOrReplay(ctx, turn, step, modelInput, entryAt(entries, step),
			func() (stepRun, error) { return r.executeModel(ctx, turn, step, round, messages) },
			r.applyModel)
		if err != nil {
			r.failTurn(ctx, err)
			return
		}
		var out modelStepOutput
		if err := json.Unmarshal(entry.Output, &out); err != nil {
			r.failTurn(ctx, fatal(fmt.Errorf("decode journaled model step %d: %w", step, err)))
			return
		}
		r.messages.Add(1)
		step++
		if len(out.Message.ToolCalls) == 0 {
			return
		}
		for _, call := range out.Message.ToolCalls {
			if ctx.Err() != nil {
				return
			}
			args := call.Function.Arguments
			if args == "" {
				args = "{}"
			}
			toolInput, _ := json.Marshal(toolStepInput{
				Tool:      call.Function.Name,
				CallID:    call.ID,
				Arguments: args,
			})
			if existing := entryAt(entries, step); existing != nil && existing.Name != "tool:"+call.Function.Name {
				r.failTurn(ctx, fatal(fmt.Errorf("journal step %d is %q, want tool:%s", step, existing.Name, call.Function.Name)))
				return
			}
			entry, _, err := r.runOrReplay(ctx, turn, step, toolInput, entryAt(entries, step),
				func() (stepRun, error) { return r.executeTool(ctx, turn, step, call) },
				r.applyTool)
			if err != nil {
				r.failTurn(ctx, err)
				return
			}
			var toolOut toolStepOutput
			if err := json.Unmarshal(entry.Output, &toolOut); err != nil {
				r.failTurn(ctx, fatal(fmt.Errorf("decode journaled tool step %d: %w", step, err)))
				return
			}
			if toolOut.Executed {
				r.toolCalls.Add(1)
			}
			step++
		}
	}
	r.events.Emit(ctx, model.EventError, "mayfly", fmt.Sprintf("Step limit (%d) reached; stopping this turn.", r.cfg.MaxSteps), nil)
}

// checkStepName rejects a recorded step of the wrong kind before it is
// applied to the conversation.
func checkStepName(entry *journal.Entry, want string) error {
	if entry == nil || entry.Name == want {
		return nil
	}
	return fmt.Errorf("journal step %d is %q, want %q", entry.Step, entry.Name, want)
}

// executeModel runs one model request. The assistant message is published only
// when the step reaches the journal, so a crash between the model response and
// the append cannot leave a visible reply that the conversation does not hold.
func (r *Runtime) executeModel(ctx context.Context, turn int64, step, round int, messages []llm.Message) (stepRun, error) {
	llmTimeout := time.Duration(r.cfg.LLMTimeoutSeconds) * time.Second
	if llmTimeout <= 0 {
		llmTimeout = 180 * time.Second
	}
	// One step gets one overall budget. A partition that outlasts it must end
	// the process so a replacement resumes from the journal, instead of
	// retrying until the idle or TTL timer finalizes the session.
	retryCtx, retryCancel := context.WithTimeout(ctx, modelRetryBudget)
	defer retryCancel()
	started := time.Now()
	r.trace.Write("llm_request", map[string]any{
		"step":     round,
		"messages": len(messages),
		"tools":    len(r.toolsList),
		"model":    r.cfg.Model,
	})
	var msg llm.Message
	var finish string
	var err error
	deadlineFailures := 0
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		attemptCtx, attemptCancel := context.WithTimeout(retryCtx, llmTimeout)
		msg, finish, err = r.llm.Complete(attemptCtx, messages, r.toolsList, func(delta string) {
			if ctx.Err() == nil {
				r.events.Emit(ctx, model.EventAssistantDelta, "agent", delta, nil)
			}
		})
		attemptCancel()
		if err == nil {
			break
		}
		if retryCtx.Err() != nil {
			break
		}
		if errors.Is(err, context.DeadlineExceeded) {
			deadlineFailures++
			if deadlineFailures >= 3 {
				break
			}
		} else if !retryableModelError(err) {
			break
		}
		r.trace.Write("llm_retry", map[string]any{
			"step":     round,
			"attempt":  attempt + 1,
			"error":    err.Error(),
			"wait_ms":  backoff.Milliseconds(),
			"duration": time.Since(started).Milliseconds(),
		})
		r.logger.Warn("model request failed; retrying", "turn_step", step, "attempt", attempt+1, "error", err)
		select {
		case <-retryCtx.Done():
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
	if err != nil {
		r.trace.Write("llm_error", map[string]any{"step": round, "error": err.Error(), "duration_ms": time.Since(started).Milliseconds()})
		if retryCtx.Err() != nil && ctx.Err() == nil {
			return stepRun{}, fatal(fmt.Errorf("model request retry budget exhausted: %w", err))
		}
		return stepRun{}, fmt.Errorf("model request failed: %w", err)
	}
	r.trace.Write("llm_response", map[string]any{
		"step":          round,
		"finish_reason": finish,
		"content":       msg.Content,
		"tool_calls":    len(msg.ToolCalls),
		"duration_ms":   time.Since(started).Milliseconds(),
	})
	msg.Role = "assistant"
	for i := range msg.ToolCalls {
		if msg.ToolCalls[i].Type == "" {
			msg.ToolCalls[i].Type = "function"
		}
		if msg.ToolCalls[i].ID == "" {
			msg.ToolCalls[i].ID = fmt.Sprintf("call_%d_%d", round, i)
		}
	}
	name := modelStepName(msg)
	var events []journal.StepEvent
	switch {
	case len(msg.ToolCalls) == 0:
		events = append(events, journal.StepEvent{
			Type:  string(model.EventAssistantMessage),
			Actor: "agent",
			Text:  msg.Content,
			Data:  map[string]any{"finish_reason": finish},
		})
	case msg.Content != "":
		events = append(events, journal.StepEvent{
			Type:  string(model.EventAssistantMessage),
			Actor: "agent",
			Text:  msg.Content,
			Data:  map[string]any{"tool_calls": len(msg.ToolCalls)},
		})
	}
	out, err := json.Marshal(modelStepOutput{Message: msg, FinishReason: finish, Events: events})
	if err != nil {
		return stepRun{}, err
	}
	return stepRun{name: name, message: msg, output: out}, nil
}

// executeTool runs one tool call. The call itself is emitted before it runs so
// a slow tool is visible while it works; it is deterministic once the model
// step is journaled, and its step key drops the copy published from the
// journal. The result is published only when the step reaches the journal, so
// a crash cannot show a result the conversation does not contain.
func (r *Runtime) executeTool(ctx context.Context, turn int64, step int, call llm.ToolCall) (stepRun, error) {
	name := call.Function.Name
	args := call.Function.Arguments
	if args == "" {
		args = "{}"
	}
	callKey := stepEventKey(turn, step, "call")
	if !json.Valid([]byte(args)) {
		ev := journal.StepEvent{
			Type:  string(model.EventToolCall),
			Actor: "agent",
			Text:  "Invalid JSON arguments for " + name,
			Data: map[string]any{
				"tool":      name,
				"call_id":   call.ID,
				"arguments": args,
			},
		}
		r.events.EmitRecord(ctx, callKey, ev)
		msg := llm.Message{Role: "tool", ToolCallID: call.ID, Content: "ERROR: invalid JSON arguments"}
		out, err := json.Marshal(toolStepOutput{Message: msg, Events: []journal.StepEvent{ev}})
		if err != nil {
			return stepRun{}, err
		}
		return stepRun{name: "tool:" + name, message: msg, output: out, errMsg: "invalid JSON arguments"}, nil
	}
	callEvent := journal.StepEvent{
		Type:  string(model.EventToolCall),
		Actor: "agent",
		Text:  name,
		Data: map[string]any{
			"tool":      name,
			"call_id":   call.ID,
			"arguments": args,
		},
	}
	r.events.EmitRecord(ctx, callKey, callEvent)
	started := time.Now()
	outcome, err := r.callTool(ctx, turn, step, call)
	if err != nil {
		return stepRun{}, err
	}
	out := truncate(outcome.Content, maxToolOutput)
	isErr := outcome.IsError
	if outcome.Interrupted {
		// The gateway refused to repeat a non-idempotent call that a restart
		// cut short. Its message is the whole result, so the model reads why.
		r.events.Emit(ctx, model.EventSystem, "mayfly", out, nil)
	}
	resultData := map[string]any{
		"tool":    name,
		"call_id": call.ID,
		"error":   isErr,
	}
	if outcome.Interrupted {
		resultData["interrupted"] = true
	}
	resultEvent := journal.StepEvent{
		Type:  string(model.EventToolResult),
		Actor: "agent",
		Text:  truncate(out, 8<<10),
		Data:  resultData,
	}
	r.trace.Write("tool_result", map[string]any{
		"tool":        name,
		"call_id":     call.ID,
		"arguments":   args,
		"is_error":    isErr,
		"executed":    outcome.Executed,
		"replayed":    outcome.Replayed,
		"interrupted": outcome.Interrupted,
		"output":      out,
		"duration_ms": time.Since(started).Milliseconds(),
	})
	msg := llm.Message{Role: "tool", ToolCallID: call.ID, Content: out}
	if isErr && !strings.HasPrefix(out, "ERROR:") {
		msg.Content = "ERROR: " + out
	}
	raw, err := json.Marshal(toolStepOutput{
		Message:     msg,
		IsError:     isErr,
		Executed:    outcome.Executed,
		Replayed:    outcome.Replayed,
		Interrupted: outcome.Interrupted,
		Events:      []journal.StepEvent{callEvent, resultEvent},
	})
	if err != nil {
		return stepRun{}, err
	}
	errMsg := ""
	if isErr {
		errMsg = truncate(msg.Content, 2<<10)
	}
	return stepRun{name: "tool:" + name, message: msg, output: raw, errMsg: errMsg}, nil
}

// prepareResume truncates a partially applied conversation and returns the
// turn that must be replayed. The event itself is rebuilt from step zero's
// recorded Input, so recovery does not depend on the control plane still
// holding the old inbox event.
func (r *Runtime) prepareResume(ctx context.Context) (int64, *model.Event, error) {
	last, err := r.journal.LastTurn(ctx, r.cfg.SessionID)
	if err != nil {
		return 0, nil, fmt.Errorf("read last journal turn: %w", err)
	}
	if last <= 0 {
		return 0, nil, nil
	}
	entries, err := r.journal.Turn(ctx, r.cfg.SessionID, last)
	if err != nil {
		return 0, nil, fmt.Errorf("read journal turn %d: %w", last, err)
	}
	if len(entries) == 0 {
		return last - 1, nil, nil
	}
	// Validate the turn before touching the conversation: a journal entry that
	// does not describe a user turn must not shorten the recorded history.
	var ev model.Event
	if err := json.Unmarshal(entries[0].Input, &ev); err != nil {
		return last - 1, nil, fmt.Errorf("decode journal turn event: %w", err)
	}
	if ev.Seq == 0 {
		ev.Seq = last
	}
	if ev.Seq != last || ev.Type != model.EventUserMessage {
		return last - 1, nil, fmt.Errorf("journal turn %d does not start with its user event (seq %d type %q)", last, ev.Seq, ev.Type)
	}
	lastConv := entries[len(entries)-1].ConvLen
	if r.conv.Size() > lastConv {
		if err := r.conv.Truncate(lastConv); err != nil {
			return 0, nil, err
		}
	}
	r.logger.Info("resuming journaled turn", "turn", last, "steps", len(entries), "conversation_bytes", r.conv.Size())
	return last - 1, &ev, nil
}

// armResume remembers the inbox event the runtime replayed from the journal.
func (r *Runtime) armResume(ev model.Event) {
	r.resumeMu.Lock()
	defer r.resumeMu.Unlock()
	r.resumeEv = &ev
}

// skipResumeEvent reports whether ev is the redelivered event the runtime has
// already replayed from the journal. Identity decides, not the sequence
// number: a sequence assigned before a control-plane restart is handed out
// again, and skipping it would silently drop a new message.
func (r *Runtime) skipResumeEvent(ev model.Event) bool {
	r.resumeMu.Lock()
	defer r.resumeMu.Unlock()
	if r.resumeEv == nil || !sameTurnEvent(*r.resumeEv, ev) {
		return false
	}
	r.resumeEv = nil
	return true
}

// clearResumeEvent drops the pending skip. An inbox reset rewinds the stream to
// zero, so the event may never arrive again; the journal decides replay.
func (r *Runtime) clearResumeEvent() {
	r.resumeMu.Lock()
	defer r.resumeMu.Unlock()
	r.resumeEv = nil
}

// sameTurnEvent reports whether two deliveries describe the same message. The
// sequence number is ephemeral and shared across control-plane restarts, so
// the content is compared as well.
func sameTurnEvent(a, b model.Event) bool {
	return a.Seq == b.Seq && a.Type == b.Type && a.Actor == b.Actor &&
		a.Text == b.Text && a.Time.Equal(b.Time)
}

// verifyTurn rejects a journaled turn that does not describe this event or has
// gaps between its steps. The conversation is replayed in step order, so a gap
// would make a later step execute again and overwrite its entry.
func verifyTurn(entries []journal.Entry, ev model.Event) error {
	for i := range entries {
		if entries[i].Step != i {
			return fmt.Errorf("journal turn %d has step %d at position %d", ev.Seq, entries[i].Step, i)
		}
	}
	var recorded model.Event
	if err := json.Unmarshal(entries[0].Input, &recorded); err != nil {
		return fmt.Errorf("decode journal turn %d event: %w", ev.Seq, err)
	}
	if !sameTurnEvent(recorded, ev) {
		return fmt.Errorf("journal turn %d belongs to a different message", ev.Seq)
	}
	return nil
}

// failTurn ends the turn. A fatal failure also ends the process: the control
// plane starts a replacement, which resumes from the journal instead of
// continuing on a conversation the journal does not describe.
func (r *Runtime) failTurn(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	if isFatal(err) {
		r.logger.Error("agent turn cannot continue", "error", err)
		select {
		case r.fatalCh <- err:
		default:
		}
		return
	}
	r.turnFailed(ctx, err)
}

// stopChildren closes the MCP clients and plugin processes. A plugin's stderr
// is flushed here, so a bundle built afterwards holds all of it.
func (r *Runtime) stopChildren() {
	if r.mcp != nil {
		r.mcp.Close()
	}
	if r.plugins != nil {
		r.plugins.Close()
	}
}

// closeLocal releases the processes and logs of a runtime that will not
// finalize. A replacement starts its own.
func (r *Runtime) closeLocal() {
	r.stopChildren()
	r.closeLogs()
}

// closeLogs closes the local logs without exporting an artifact. A runtime
// stopped by a signal leaves the bundle to its replacement.
func (r *Runtime) closeLogs() {
	if err := r.conv.Close(); err != nil {
		r.logger.Debug("close conversation log", "error", err)
	}
	if err := r.trace.Close(); err != nil {
		r.logger.Debug("close trace log", "error", err)
	}
	if err := r.events.Close(); err != nil {
		r.logger.Debug("close event log", "error", err)
	}
}

func (r *Runtime) turnFailed(ctx context.Context, err error) {
	r.logger.Warn("agent turn failed", "error", err)
	if ctx.Err() != nil {
		return
	}
	r.events.Emit(ctx, model.EventError, "agent", err.Error(), nil)
}

// modelRetryBudget bounds one model step across all its attempts. It is long
// enough to cover a control-plane restart and short enough to hand the turn to
// a replacement container when the network does not come back.
var modelRetryBudget = 5 * time.Minute

// retryableModelError keeps a turn alive across a control-plane replacement,
// where the broker connection and 5xx responses are transient. A provider
// rejecting the request is not retried.
func retryableModelError(err error) bool {
	var status *llm.StatusError
	if errors.As(err, &status) {
		return status.Code >= 500 || status.Code == 408 || status.Code == 429
	}
	return !errors.Is(err, context.Canceled)
}

// Version returns the runtime version string.
func Version() string { return version.Version }
