package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hayden/mayfly/internal/llm"
	"github.com/hayden/mayfly/internal/mcp"
	"github.com/hayden/mayfly/internal/model"
	"github.com/hayden/mayfly/internal/platformclient"
	"github.com/hayden/mayfly/internal/sessioncfg"
	"github.com/hayden/mayfly/internal/version"
)

// Runtime is the in-container agent session.
type Runtime struct {
	cfg        sessioncfg.Config
	token      string
	logger     *slog.Logger
	platform   *platformclient.Client
	llm        *llm.Client
	mcp        *mcp.Manager
	tools      *toolset
	toolsList  []llm.Tool
	conv       *conversation
	trace      *tracer
	events     *emitter
	system     string
	messages   atomic.Int64
	toolCalls  atomic.Int64
	finalizeMu sync.Mutex
	finalized  bool
	finalErr   error
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
		llm:      llm.New(cfg.LLMBaseURL, token, cfg.Model),
		conv:     conv,
		trace:    trace,
		events:   events,
		system:   systemPrompt(cfg),
	}, nil
}

// Run starts MCP servers, consumes the inbox, and finalizes the session.
func (r *Runtime) Run(ctx context.Context) error {
	r.logger.Info("session starting",
		"session", r.cfg.SessionID,
		"model", r.cfg.Model,
		"skills", len(r.cfg.Skills),
		"mcp_servers", len(r.cfg.MCPServers),
	)

	r.mcp = mcp.Start(ctx, r.cfg.MCPServers, r.token, r.logger)
	for _, err := range r.mcp.Errors() {
		r.events.Emit(ctx, model.EventSystem, "mayfly", "MCP server unavailable: "+err.Error(), nil)
	}
	tools, err := newToolset(r.cfg.WorkspaceDir, r.cfg.AllowShell, time.Duration(r.cfg.ShellTimeoutSecs)*time.Second, r.mcp)
	if err != nil {
		return r.finalize(context.Background(), "failed", err)
	}
	r.tools = tools
	r.toolsList = tools.Tools()

	skillNames := make([]string, 0, len(r.cfg.Skills))
	for _, s := range r.cfg.Skills {
		skillNames = append(skillNames, s.Name)
	}
	mcpNames := make([]string, 0, len(r.cfg.MCPServers))
	for _, s := range r.cfg.MCPServers {
		mcpNames = append(mcpNames, s.Name)
	}
	r.events.Emit(ctx, model.EventSystem, "mayfly", "Session started. Attach to interact.", map[string]any{
		"model":       r.cfg.Model,
		"skills":      skillNames,
		"mcp_servers": mcpNames,
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
	go func() {
		defer close(turnDone)
		for {
			select {
			case <-turnCtx.Done():
				return
			case ev := <-workCh:
				r.handleTurn(turnCtx, ev)
			}
		}
	}()

	inboxCtx, cancelInbox := context.WithCancel(ctx)
	go func() {
		err := r.platform.Inbox(inboxCtx, 0, func(ev model.Event) {
			switch ev.Type {
			case model.EventUserMessage:
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
		})
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
	for {
		select {
		case ev := <-userCh:
			resetIdle()
			select {
			case workCh <- ev:
			default:
				r.events.Emit(ctx, model.EventError, "mayfly", "Agent is busy; message dropped.", nil)
			}
		case msg := <-controlCh:
			if msg.action == "close" {
				if msg.reason != "" {
					reason = msg.reason
				}
				r.logger.Info("close requested", "reason", reason)
				goto shutdown
			}
		case <-idleC:
			reason = "idle"
			goto shutdown
		case <-ttlC:
			reason = "ttl"
			goto shutdown
		case <-ctx.Done():
			reason = "terminated"
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
	if r.mcp != nil {
		r.mcp.Close()
	}
	return r.finalize(context.Background(), reason, nil)
}

func (r *Runtime) handleTurn(ctx context.Context, ev model.Event) {
	text := ev.Text
	if ev.Actor != "" && ev.Actor != "trigger" {
		text = "[" + ev.Actor + "] " + text
	}
	if err := r.conv.Append(llm.Message{Role: "user", Content: text}); err != nil {
		r.logger.Warn("append user message", "error", err)
	}
	r.messages.Add(1)

	for step := range r.cfg.MaxSteps {
		if ctx.Err() != nil {
			return
		}
		messages := r.conv.MessagesForModel(r.system, r.cfg.MaxHistoryMessages)
		llmTimeout := time.Duration(r.cfg.LLMTimeoutSeconds) * time.Second
		if llmTimeout <= 0 {
			llmTimeout = 180 * time.Second
		}
		llmCtx, cancel := context.WithTimeout(ctx, llmTimeout)
		started := time.Now()
		r.trace.Write("llm_request", map[string]any{
			"step":     step,
			"messages": len(messages),
			"tools":    len(r.toolsList),
			"model":    r.cfg.Model,
		})
		msg, finish, err := r.llm.Complete(llmCtx, messages, r.toolsList, func(delta string) {
			if ctx.Err() == nil {
				r.events.Emit(ctx, model.EventAssistantDelta, "agent", delta, nil)
			}
		})
		cancel()
		if err != nil {
			r.trace.Write("llm_error", map[string]any{"step": step, "error": err.Error(), "duration_ms": time.Since(started).Milliseconds()})
			r.events.Emit(ctx, model.EventError, "agent", "Model request failed: "+err.Error(), nil)
			return
		}
		r.trace.Write("llm_response", map[string]any{
			"step":          step,
			"finish_reason": finish,
			"content":       msg.Content,
			"tool_calls":    len(msg.ToolCalls),
			"duration_ms":   time.Since(started).Milliseconds(),
		})
		msg.Role = "assistant"

		if len(msg.ToolCalls) == 0 {
			if err := r.conv.Append(msg); err != nil {
				r.logger.Warn("append assistant message", "error", err)
			}
			r.messages.Add(1)
			r.events.Emit(ctx, model.EventAssistantMessage, "agent", msg.Content, map[string]any{"finish_reason": finish})
			return
		}

		if msg.Content != "" {
			r.events.Emit(ctx, model.EventAssistantMessage, "agent", msg.Content, map[string]any{"tool_calls": len(msg.ToolCalls)})
		}
		for i := range msg.ToolCalls {
			if msg.ToolCalls[i].Type == "" {
				msg.ToolCalls[i].Type = "function"
			}
			if msg.ToolCalls[i].ID == "" {
				msg.ToolCalls[i].ID = fmt.Sprintf("call_%d_%d", step, i)
			}
		}
		if err := r.conv.Append(msg); err != nil {
			r.logger.Warn("append assistant tool call", "error", err)
		}
		r.messages.Add(1)

		for _, call := range msg.ToolCalls {
			if ctx.Err() != nil {
				return
			}
			args := call.Function.Arguments
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				r.events.Emit(ctx, model.EventToolCall, "agent", "Invalid JSON arguments for "+call.Function.Name, map[string]any{
					"tool":      call.Function.Name,
					"call_id":   call.ID,
					"arguments": args,
				})
				r.conv.Append(llm.Message{Role: "tool", ToolCallID: call.ID, Content: "ERROR: invalid JSON arguments"})
				continue
			}
			r.events.Emit(ctx, model.EventToolCall, "agent", call.Function.Name, map[string]any{
				"tool":      call.Function.Name,
				"call_id":   call.ID,
				"arguments": args,
			})
			started := time.Now()
			out, isErr, err := r.tools.Call(ctx, call.Function.Name, json.RawMessage(args))
			if err != nil {
				out = "ERROR: " + err.Error()
				isErr = true
			}
			out = truncate(out, maxToolOutput)
			r.toolCalls.Add(1)
			r.events.Emit(ctx, model.EventToolResult, "agent", truncate(out, 8<<10), map[string]any{
				"tool":    call.Function.Name,
				"call_id": call.ID,
				"error":   isErr,
			})
			r.trace.Write("tool_result", map[string]any{
				"tool":        call.Function.Name,
				"call_id":     call.ID,
				"arguments":   args,
				"is_error":    isErr,
				"output":      out,
				"duration_ms": time.Since(started).Milliseconds(),
			})
			content := out
			if isErr && !strings.HasPrefix(content, "ERROR:") {
				content = "ERROR: " + content
			}
			if err := r.conv.Append(llm.Message{Role: "tool", ToolCallID: call.ID, Content: content}); err != nil {
				r.logger.Warn("append tool result", "error", err)
			}
		}
	}
	r.events.Emit(ctx, model.EventError, "mayfly", fmt.Sprintf("Step limit (%d) reached; stopping this turn.", r.cfg.MaxSteps), nil)
}

// Version returns the runtime version string.
func Version() string { return version.Version }
