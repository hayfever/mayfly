// Package model defines the public data types shared by the control plane,
// the session runtime, and clients.
package model

import (
	"encoding/json"
	"math"
	"time"
)

// SessionStatus is the lifecycle state of a session.
type SessionStatus string

const (
	StatusPending   SessionStatus = "pending"
	StatusRunning   SessionStatus = "running"
	StatusClosing   SessionStatus = "closing"
	StatusCompleted SessionStatus = "completed"
	StatusFailed    SessionStatus = "failed"
	StatusExpired   SessionStatus = "expired"
)

// TriggerInfo describes what caused a session to be created.
type TriggerInfo struct {
	Kind     string `json:"kind"` // api, generic, github, slack, datadog
	Name     string `json:"name,omitempty"`
	Event    string `json:"event,omitempty"`
	Delivery string `json:"delivery,omitempty"`
	JobID    string `json:"job_id,omitempty"`
	Actor    string `json:"actor,omitempty"`
}

// ArtifactInfo describes an exported session bundle in the configured store.
// URI is the store location; the HTTP API exposes the bundle through its
// download route and keeps URI internal.
type ArtifactInfo struct {
	Key       string    `json:"key"`
	URI       string    `json:"uri"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	Partial   bool      `json:"partial"`
}

// LLMUsage counts the model calls of one session. PromptTokens and
// CompletionTokens include estimated usage; EstimatedCalls counts the calls
// whose usage the provider did not report.
type LLMUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	Requests         int64 `json:"requests"`
	EstimatedCalls   int64 `json:"estimated_calls"`
}

// Total returns prompt plus completion tokens, saturated at MaxInt64.
func (u LLMUsage) Total() int64 {
	if u.PromptTokens > math.MaxInt64-u.CompletionTokens {
		return math.MaxInt64
	}
	return u.PromptTokens + u.CompletionTokens
}

// LLMBudget is the resolved model budget of one session. Zero in either field
// means no limit.
type LLMBudget struct {
	MaxTokens   int64 `json:"max_tokens"`
	MaxRequests int64 `json:"max_requests"`
}

// Session is the primary unit of work. A session exists for a bounded period
// of time and always ends with an artifact export attempt.
type Session struct {
	ID         string        `json:"id"`
	Title      string        `json:"title"`
	Status     SessionStatus `json:"status"`
	Prompt     string        `json:"prompt"`
	Agent      string        `json:"agent"`
	Harness    string        `json:"harness"`
	Model      string        `json:"model"`
	Skills     []string      `json:"skills"`
	MCPServers []string      `json:"mcp_servers"`
	Plugins    []string      `json:"plugins"`
	Trigger    TriggerInfo   `json:"trigger"`
	// Backend and PodName describe the runtime placement. They are internal
	// to the control plane and are not part of the public session object.
	Backend      string        `json:"backend"`
	PodName      string        `json:"pod_name,omitempty"`
	TTLSeconds   int           `json:"ttl_seconds"`
	CreatedAt    time.Time     `json:"created_at"`
	StartedAt    *time.Time    `json:"started_at,omitempty"`
	ExpiresAt    time.Time     `json:"expires_at"`
	ClosedAt     *time.Time    `json:"closed_at,omitempty"`
	LastActivity time.Time     `json:"last_activity"`
	MessageCount int           `json:"message_count"`
	Artifact     *ArtifactInfo `json:"artifact,omitempty"`
	// LLMUsage and LLMBudget report model consumption. A budgeted session
	// publishes both from creation, with usage at zeros. A session without a
	// budget publishes usage after its first model call and no budget.
	LLMUsage  *LLMUsage  `json:"llm_usage,omitempty"`
	LLMBudget *LLMBudget `json:"llm_budget,omitempty"`
	// LLMPendingTokens is the durable reservation of an in-flight model call.
	// It is not part of the public session API; the control plane uses it to
	// charge a call that a restart interrupts.
	LLMPendingTokens int64  `json:"llm_pending_tokens,omitempty"`
	Error            string `json:"error,omitempty"`
}

// EventType identifies an item in the session thread.
type EventType string

const (
	EventUserMessage      EventType = "user_message"
	EventAssistantMessage EventType = "assistant_message"
	EventAssistantDelta   EventType = "assistant_delta"
	EventToolCall         EventType = "tool_call"
	EventToolResult       EventType = "tool_result"
	EventStatus           EventType = "status"
	EventError            EventType = "error"
	EventSystem           EventType = "system"
	EventArtifact         EventType = "artifact"
	EventControl          EventType = "control"
)

// Event is one thread item. Seq is assigned by the control plane. Data is
// the payload that belongs to Type; see DecodeEventData.
type Event struct {
	Seq       int64     `json:"seq"`
	SessionID string    `json:"session_id,omitempty"`
	StepKey   string    `json:"step_key,omitempty"`
	Time      time.Time `json:"time"`
	Type      EventType `json:"type"`
	Actor     string    `json:"actor,omitempty"`
	Text      string    `json:"text,omitempty"`
	Data      EventData `json:"data,omitempty"`
}

// UnmarshalJSON decodes an event and the typed payload that belongs to its
// type. Clients ignore an unknown event type and an unknown payload field.
func (e *Event) UnmarshalJSON(b []byte) error {
	type event Event
	var raw struct {
		*event
		Data json.RawMessage `json:"data"`
	}
	raw.event = (*event)(e)
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	data, err := DecodeEventData(e.Type, raw.Data)
	if err != nil {
		return err
	}
	e.Data = data
	return nil
}

// IsInbox reports whether an event must be delivered to the session runtime.
// Only human messages and control actions cross that boundary.
func (e Event) IsInbox() bool {
	return e.Type == EventUserMessage || e.Type == EventControl
}
