// Package gateway is the control plane's choke point for every non-built-in
// tool call. It holds each session's tool grant, records each call in the
// ledger, enforces timeouts, rate limits, and the in-flight cap, mints call
// tokens, and writes the audit log. It never runs tool code.
//
// The request and response types in this file are shared with
// internal/platformclient, so they import nothing from the server side.
package gateway

import (
	"encoding/json"
	"fmt"
	"time"
)

// BeginRequest is one runtime request to call a granted tool. Arguments are
// sent so the gateway can hash them; they are never stored or audited.
type BeginRequest struct {
	Turn   int64  `json:"turn"`
	Step   int    `json:"step"`
	CallID string `json:"call_id,omitempty"`
	Tool   string `json:"tool"`
	// InstanceID names the runtime process that asks. A begin that repeats a
	// pending call of the same instance re-issues it: the runtime never got
	// the answer, so the tool never ran.
	InstanceID string          `json:"instance_id,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
}

// CompleteRequest reports the result of a locally dispatched call.
type CompleteRequest struct {
	Key     string `json:"key"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// Result is a recorded tool result. Replayed marks a result the ledger
// already held; Interrupted marks a non-idempotent call a container restart
// cut short, which the gateway did not run again.
type Result struct {
	Content     string `json:"content"`
	IsError     bool   `json:"is_error"`
	Replayed    bool   `json:"replayed"`
	Interrupted bool   `json:"interrupted"`
}

// Dispatch tells the runtime to call a local tool itself. Key is the ledger
// key the runtime echoes to Complete. Token is set only for sidecar tools.
type Dispatch struct {
	Key       string `json:"key"`
	TimeoutMS int64  `json:"timeout_ms"`
	Token     string `json:"token,omitempty"`
}

// BeginResponse carries exactly one of Result and Dispatch.
type BeginResponse struct {
	Result   *Result   `json:"result,omitempty"`
	Dispatch *Dispatch `json:"dispatch,omitempty"`
}

// Discovery is the tool grant as the runtime sees it.
type Discovery struct {
	Tools []DiscoveryTool `json:"tools"`
	// Wildcards names the mcp.servers owners whose tool list the grant does
	// not fix. The runtime resolves a local stdio tool against its own list;
	// the gateway resolves the rest when the call begins.
	Wildcards map[string]Placement    `json:"wildcards,omitempty"`
	Sidecars  map[string]SidecarToken `json:"sidecars,omitempty"`
	Errors    []DiscoveryError        `json:"errors,omitempty"`
}

// DiscoveryTool is one tool the session may call. Remote tools carry the
// upstream description and schema; local tools carry grant fields only.
type DiscoveryTool struct {
	Name        string          `json:"name"`
	Owner       string          `json:"owner"`
	Placement   Placement       `json:"placement"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Idempotent  bool            `json:"idempotent"`
	TimeoutMS   int64           `json:"timeout_ms"`
}

// SidecarToken is the discovery token one sidecar plugin accepts for
// initialize and tools/list.
type SidecarToken struct {
	DiscoveryToken string `json:"discovery_token"`
}

// DiscoveryError reports an owner whose tools are unavailable. A required
// owner makes the runtime fail the session at start.
type DiscoveryError struct {
	Owner    string `json:"owner"`
	Required bool   `json:"required"`
	Error    string `json:"error"`
}

// Error is a begin or complete failure. Status is the HTTP status the tool
// route returns, so the runtime maps one value to one outcome. RetryAfter is
// set when waiting would let the call proceed.
type Error struct {
	Status     int
	Message    string
	RetryAfter time.Duration
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// errf builds a gateway error with an HTTP status.
func errf(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}
