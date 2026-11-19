package model

import (
	"encoding/json"
	"fmt"
)

// EventData is the payload of one event. Every event type that carries data
// has one concrete payload type. The interface is sealed so a producer uses
// the payload that belongs to the event type. The JSON field names below are
// part of the API: renames and removals are breaking, new optional fields are
// additive.
type EventData interface {
	isEventData()
}

// UserMessageData describes the message that started a session.
type UserMessageData struct {
	Kind  string `json:"kind"`  // trigger kind: api, generic, github, slack, datadog
	Name  string `json:"name"`  // trigger name
	Event string `json:"event"` // trigger event
}

// AssistantMessageData describes the model reply of one round.
type AssistantMessageData struct {
	FinishReason string `json:"finish_reason,omitempty"`
	ToolCalls    int    `json:"tool_calls,omitempty"`
}

// ToolCallData describes one tool invocation.
type ToolCallData struct {
	Tool      string `json:"tool"`
	CallID    string `json:"call_id"`
	Arguments string `json:"arguments"`
}

// ToolResultData describes the outcome of one tool invocation.
type ToolResultData struct {
	Tool        string `json:"tool"`
	CallID      string `json:"call_id"`
	Error       bool   `json:"error"`
	Interrupted bool   `json:"interrupted,omitempty"`
}

// StatusData describes a session lifecycle change.
type StatusData struct {
	Status  SessionStatus `json:"status"`
	Reason  string        `json:"reason,omitempty"`
	Error   string        `json:"error,omitempty"`
	Restart int           `json:"restart,omitempty"`
}

// SystemData describes the runtime that a session started.
type SystemData struct {
	Model      string   `json:"model"`
	Skills     []string `json:"skills"`
	MCPServers []string `json:"mcp_servers"`
	Plugins    []string `json:"plugins"`
	ToolCount  int      `json:"tool_count"`
}

// ArtifactData describes an exported session bundle.
type ArtifactData struct {
	Key     string `json:"key"`
	URI     string `json:"uri"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Partial bool   `json:"partial,omitempty"`
}

// ControlData describes a control action for the session runtime.
type ControlData struct {
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

func (UserMessageData) isEventData()      {}
func (AssistantMessageData) isEventData() {}
func (ToolCallData) isEventData()         {}
func (ToolResultData) isEventData()       {}
func (StatusData) isEventData()           {}
func (SystemData) isEventData()           {}
func (ArtifactData) isEventData()         {}
func (ControlData) isEventData()          {}

// DataAs returns the payload as T. It reports false when the event carries a
// different payload or none.
func DataAs[T EventData](d EventData) (T, bool) {
	v, ok := d.(T)
	return v, ok
}

// DecodeEventData decodes the data object of an event type. A type without a
// payload, or a type this build does not know, returns a nil payload.
func DecodeEventData(typ EventType, raw []byte) (EventData, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	switch typ {
	case EventUserMessage:
		return decodeEventData[UserMessageData](typ, raw)
	case EventAssistantMessage:
		return decodeEventData[AssistantMessageData](typ, raw)
	case EventToolCall:
		return decodeEventData[ToolCallData](typ, raw)
	case EventToolResult:
		return decodeEventData[ToolResultData](typ, raw)
	case EventStatus:
		return decodeEventData[StatusData](typ, raw)
	case EventSystem:
		return decodeEventData[SystemData](typ, raw)
	case EventArtifact:
		return decodeEventData[ArtifactData](typ, raw)
	case EventControl:
		return decodeEventData[ControlData](typ, raw)
	default:
		return nil, nil
	}
}

func decodeEventData[T EventData](typ EventType, raw []byte) (EventData, error) {
	var d T
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("decode %s event data: %w", typ, err)
	}
	return d, nil
}
