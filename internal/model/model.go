// Package model defines the public data types shared by the control plane,
// the session runtime, and clients.
package model

import (
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
type ArtifactInfo struct {
	Key       string    `json:"key"`
	URI       string    `json:"uri"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	Partial   bool      `json:"partial"`
}

// Session is the primary unit of work. A session exists for a bounded period
// of time and always ends with an artifact export attempt.
type Session struct {
	ID           string        `json:"id"`
	Title        string        `json:"title"`
	Status       SessionStatus `json:"status"`
	Prompt       string        `json:"prompt"`
	Agent        string        `json:"agent"`
	Harness      string        `json:"harness"`
	Model        string        `json:"model"`
	Skills       []string      `json:"skills"`
	MCPServers   []string      `json:"mcp_servers"`
	Plugins      []string      `json:"plugins"`
	Trigger      TriggerInfo   `json:"trigger"`
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
	Error        string        `json:"error,omitempty"`
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

// Event is one thread item. Seq is assigned by the control plane.
type Event struct {
	Seq       int64          `json:"seq"`
	SessionID string         `json:"session_id,omitempty"`
	StepKey   string         `json:"step_key,omitempty"`
	Time      time.Time      `json:"time"`
	Type      EventType      `json:"type"`
	Actor     string         `json:"actor,omitempty"`
	Text      string         `json:"text,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// IsInbox reports whether an event must be delivered to the session runtime.
// Only human messages and control actions cross that boundary.
func (e Event) IsInbox() bool {
	return e.Type == EventUserMessage || e.Type == EventControl
}
