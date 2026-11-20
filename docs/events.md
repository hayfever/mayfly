# Events

A session has one thread of events. The REST event endpoint and the WebSocket
deliver the same event object. The control plane assigns `seq` in increasing
order for one session.

## Event envelope

```json
{
  "seq": 7,
  "session_id": "abc123",
  "step_key": "1/2/result",
  "time": "2026-01-02T03:04:05Z",
  "type": "tool_result",
  "actor": "agent",
  "text": "hello",
  "data": {"tool": "echo", "call_id": "call_1", "error": false}
}
```

| Field | Type | Meaning |
| --- | --- | --- |
| `seq` | integer | Sequence number in the session thread. Always present. |
| `session_id` | string | Session that owns the event. |
| `step_key` | string | Idempotency key for a journaled step. The control plane drops a repeat. |
| `time` | string | RFC 3339 timestamp. Always present. |
| `type` | string | Event type. Always present. The set is open: ignore a type this build does not know. |
| `actor` | string | Writer: `user`, `agent`, `mayfly`, or a trigger name. |
| `text` | string | Human-readable text. |
| `data` | object | Payload. The schema depends on `type`. |

An event type without a payload has no `data` field.

## Event types

The table lists the types this build writes. The set is open: a client must
ignore a type this build does not know.

| `type` | Payload schema | Payload fields |
| --- | --- | --- |
| `user_message` | `UserMessageEventData` | `kind`, `name`, `event` |
| `assistant_message` | `AssistantMessageEventData` | `finish_reason`, `tool_calls` |
| `assistant_delta` | none | none |
| `tool_call` | `ToolCallEventData` | `tool`, `call_id`, `arguments` |
| `tool_result` | `ToolResultEventData` | `tool`, `call_id`, `error`, `interrupted` |
| `status` | `StatusEventData` | `status`, `reason`, `error`, `restart` |
| `error` | none | none |
| `system` | `SystemEventData` | `model`, `skills`, `mcp_servers`, `plugins`, `tool_count` |
| `artifact` | `ArtifactEventData` | `key`, `uri`, `size`, `sha256`, `partial` |
| `control` | `ControlEventData` | `action`, `reason` |

The schema names refer to `components/schemas` in
[`api/openapi.yaml`](../api/openapi.yaml).

### `user_message`

A human message or the trigger message that created the session.

| Field | Type | Meaning |
| --- | --- | --- |
| `kind` | string | Trigger kind. Known values are `api`, `generic`, `github`, `slack`, and `datadog`. The field is open: a client must ignore a value this build does not know. |
| `name` | string | Trigger name. |
| `event` | string | Trigger event name. |

A human message has no payload.

### `assistant_message`

One model reply. `finish_reason` is the model finish reason. `tool_calls` is
the number of tool calls in the reply.

### `assistant_delta`

A streamed piece of the model reply. The field `text` holds the piece. The
final `assistant_message` holds the complete text.

### `tool_call`

One tool invocation.

| Field | Type | Meaning |
| --- | --- | --- |
| `tool` | string | Tool name. |
| `call_id` | string | Model call identifier. |
| `arguments` | string | Arguments as a JSON-encoded string. |

### `tool_result`

The outcome of one tool invocation.

| Field | Type | Meaning |
| --- | --- | --- |
| `tool` | string | Tool name. |
| `call_id` | string | Model call identifier. |
| `error` | boolean | The tool reported an error. |
| `interrupted` | boolean | A restart cut the call short and the result is not a real output. |

The field `text` holds the tool output.

### `status`

A session lifecycle change. `status` is one of `pending`, `running`,
`closing`, `completed`, `failed`, or `expired`. `reason` names the cause.
`error` holds the failure message and is absent when there is no failure.
`restart` is the replacement attempt number after a container restart.

### `error`

An error that does not change the session status. The field `text` holds the
message.

### `system`

A runtime notice. The first `system` event reports the runtime setup: `model`,
`skills`, `mcp_servers`, `plugins`, and `tool_count`. A later notice uses only
`text`.

### `artifact`

An exported session bundle. `key`, `uri`, `size`, and `sha256` identify the
bundle. `partial` is `true` when the control plane exported the bundle after a
runtime failure.

### `control`

A control action for the session runtime. `action` is the action name, for
example `close`. `reason` is the cause.

## Delivery

`GET /api/v1/sessions/{id}/events?after=<seq>` returns stored events with
`seq` greater than `after`.

The WebSocket at `GET /api/v1/sessions/{id}/ws` sends server frames:

```json
{"type": "hello", "session": {...}}
{"type": "event", "event": {...}}
{"type": "error", "message": "..."}
```

The client sends `{"type": "user_message", "text": "..."}` and
`{"type": "ping"}`. Events are replayed from `after`. Reconnect with
`after=0` to rebuild the view.

## Compatibility

The event object is public API. These changes are additive:

- A new event type.
- A new optional payload field.
- A new value of an open string field.

These changes are breaking:

- A rename or a removal of an event type, a payload field, or an envelope
  field.
- A change to the type or the meaning of a field.

A client must ignore an unknown event type and an unknown field. Do not reject
an event because it has a new `type` or new payload keys. A new status value
follows the session status contract, not the event contract.

All producers use the typed payloads in `internal/model/events.go`. A producer
must not build an ad hoc `map[string]any` for event data.
