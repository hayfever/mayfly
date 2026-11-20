# Mayfly control plane API reference

Generated from api/openapi.yaml by `go run ./hack/apidoc`. Do not edit.

## sessions

| Method | Path | Operation | Scope | Summary |
| --- | --- | --- | --- | --- |
| `POST` | `/api/v1/sessions` | `createSession` | `sessions:write` | Create a session |
| `GET` | `/api/v1/sessions` | `listSessions` | `sessions:read` | List sessions |
| `GET` | `/api/v1/sessions/{id}` | `getSession` | `sessions:read` | Read one session |
| `DELETE` | `/api/v1/sessions/{id}` | `deleteSession` | `sessions:write` | Force-delete a session |
| `GET` | `/api/v1/sessions/{id}/events` | `listSessionEvents` | `sessions:read` | Read retained events |
| `POST` | `/api/v1/sessions/{id}/messages` | `postSessionMessage` | `sessions:write` | Append a human message |
| `POST` | `/api/v1/sessions/{id}/close` | `closeSession` | `sessions:write` | Request a graceful close |
| `POST` | `/api/v1/sessions/{id}/attach-ticket` | `createAttachTicket` | `sessions:write` | Issue a one-time WebSocket ticket |
| `GET` | `/api/v1/sessions/{id}/artifact` | `downloadSessionArtifact` | `sessions:read` | Download the session bundle |
| `GET` | `/api/v1/sessions/{id}/ws` | `attachSessionWebSocket` | none | Attach to the session thread |

### createSession

`POST /api/v1/sessions`

Request body: `CreateSessionRequest` (required). Content types: `application/json`.

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 201 | The session record exists and the runtime start was requested. | `SessionResponse` |
| 400 | The request is not valid. | `Error` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 429 | A rate limit or capacity limit is reached. `Retry-After` is set. | `Error` |
| 500 | The runtime did not start. If the session record already exists, `error.details.session_id` names it. | `Error` |

### listSessions

`GET /api/v1/sessions`

Newest first, with the session id as tie-break.

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| limit | query | no | `integer` |
| cursor | query | no | `string` |

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 200 | One page of sessions. | `SessionPage` |
| 400 | The request is not valid. | `Error` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |

### getSession

`GET /api/v1/sessions/{id}`

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| id | path | yes | `string` |

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 200 | The session and its web UI URL. | `SessionResponse` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 404 | The object or route does not exist. | `Error` |

### deleteSession

`DELETE /api/v1/sessions/{id}`

Stops the runtime and marks the session failed. A live session keeps its record until
`sessions.retention` expires; a session restored from the record store is removed at
once. Safe while an adoption retry is in flight: the retry cannot register the session
again after the delete.

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| id | path | yes | `string` |

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 204 | The session was removed. | - |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 404 | The object or route does not exist. | `Error` |
| 500 | An internal failure. The message never carries internals. | `Error` |

### listSessionEvents

`GET /api/v1/sessions/{id}/events`

Return events with a sequence greater than `after`. The event object and the payload
schema for each event type are documented in docs/events.md.

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| id | path | yes | `string` |
| after | query | no | `integer` |

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 200 | Retained events. | `object` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 404 | The object or route does not exist. | `Error` |

### postSessionMessage

`POST /api/v1/sessions/{id}/messages`

The message is stored in the thread and delivered to the runtime.

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| id | path | yes | `string` |

Request body: `object` (required). Content types: `application/json`.

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 202 | The message event was appended. | `object` |
| 400 | The request is not valid. | `Error` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 404 | The object or route does not exist. | `Error` |
| 409 | The request conflicts with the current state. | `Error` |
| 500 | An internal failure. The message never carries internals. | `Error` |

### closeSession

`POST /api/v1/sessions/{id}/close`

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| id | path | yes | `string` |

Request body: `object`. Content types: `application/json`.

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 202 | The close is in progress. | `object` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 404 | The object or route does not exist. | `Error` |
| 409 | The request conflicts with the current state. | `Error` |
| 500 | An internal failure. The message never carries internals. | `Error` |

### createAttachTicket

`POST /api/v1/sessions/{id}/attach-ticket`

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| id | path | yes | `string` |

Request body: `object`. Content types: `application/json`.

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 200 | The ticket and the WebSocket URL that consumes it. | `object` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 404 | The object or route does not exist. | `Error` |
| 500 | An internal failure. The message never carries internals. | `Error` |

### downloadSessionArtifact

`GET /api/v1/sessions/{id}/artifact`

Returns the latest bundle. The route answers `404` until finalize has stored it.
Artifact metadata outlives `sessions.retention`.

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| id | path | yes | `string` |

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 200 | The bundle archive. | `string` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 404 | The object or route does not exist. | `Error` |

### attachSessionWebSocket

`GET /api/v1/sessions/{id}/ws`

Upgrade with a ticket from `POST /api/v1/sessions/{id}/attach-ticket`. The ticket is
consumed on connect and supplies the actor name.

Server frames are `WebSocketServerMessage`. Client frames are `WebSocketClientMessage`.
Events are replayed after `after`. The server sends a ping control frame every 30
seconds. The event object and the compatibility rules are documented in docs/events.md.

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| id | path | yes | `string` |
| ticket | query | yes | `string` |
| after | query | no | `integer` |

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 101 | The connection is upgraded to WebSocket. | - |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 404 | The object or route does not exist. | `Error` |

## queue

| Method | Path | Operation | Scope | Summary |
| --- | --- | --- | --- | --- |
| `GET` | `/api/v1/queue` | `getQueueStats` | `queue:read` | Read queue counters |
| `GET` | `/api/v1/queue/jobs/{id}` | `getQueueJob` | `queue:read` | Read one accepted job |
| `GET` | `/api/v1/queue/dead` | `listDeadJobs` | `queue:read` | List buried jobs |
| `POST` | `/api/v1/queue/dead/{id}/retry` | `retryDeadJob` | `queue:write` | Requeue a buried job |
| `DELETE` | `/api/v1/queue/dead/{id}` | `deleteDeadJob` | `queue:write` | Drop a buried job |

### getQueueStats

`GET /api/v1/queue`

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 200 | Queue counters. | `object` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 503 | A dependency is not available. | `Error` |

### getQueueJob

`GET /api/v1/queue/jobs/{id}`

Includes the prompt and the session the job created. A stored payload this build cannot
decode answers 409 with code `conflict` and the decoder reason. The job record is not
changed.

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| id | path | yes | `string` |

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 200 | The job record. | `JobResponse` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 404 | The object or route does not exist. | `Error` |
| 409 | The request conflicts with the current state. | `Error` |
| 503 | A dependency is not available. | `Error` |

### listDeadJobs

`GET /api/v1/queue/dead`

Newest failure first, with the job id as tie-break. The prompt is omitted.

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| limit | query | no | `integer` |
| cursor | query | no | `string` |

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 200 | One page of buried jobs. | `JobPage` |
| 400 | The request is not valid. | `Error` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 503 | A dependency is not available. | `Error` |

### retryDeadJob

`POST /api/v1/queue/dead/{id}/retry`

The attempt count is reset.

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| id | path | yes | `string` |

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 202 | The job is back on the queue. | `Accepted` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 404 | The object or route does not exist. | `Error` |
| 503 | A dependency is not available. | `Error` |

### deleteDeadJob

`DELETE /api/v1/queue/dead/{id}`

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| id | path | yes | `string` |

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 204 | The job was removed. | - |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 404 | The object or route does not exist. | `Error` |
| 503 | A dependency is not available. | `Error` |

## schedules

| Method | Path | Operation | Scope | Summary |
| --- | --- | --- | --- | --- |
| `GET` | `/api/v1/schedules` | `listSchedules` | `schedules:read` | List schedules |
| `POST` | `/api/v1/schedules/{name}/run` | `runSchedule` | `schedules:write` | Enqueue one manual schedule run |

### listSchedules

`GET /api/v1/schedules`

Sorted by name.

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| limit | query | no | `integer` |
| cursor | query | no | `string` |

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 200 | One page of schedules. | `SchedulePage` |
| 400 | The request is not valid. | `Error` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |

### runSchedule

`POST /api/v1/schedules/{name}/run`

A manual run uses a random delivery key, so it neither consumes nor blocks a cron slot.

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| name | path | yes | `string` |

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 202 | The run was queued. | `Accepted` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |
| 404 | The object or route does not exist. | `Error` |
| 500 | An internal failure. The message never carries internals. | `Error` |
| 503 | A dependency is not available. | `Error` |

## config

| Method | Path | Operation | Scope | Summary |
| --- | --- | --- | --- | --- |
| `GET` | `/api/v1/config` | `getConfig` | `sessions:read` | Read UI metadata |

### getConfig

`GET /api/v1/config`

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 200 | Defaults, allowed models, agents, skills, MCP servers, triggers, and schedules. | `Config` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 403 | The token does not carry the scope the operation requires. | `Error` |

## webhooks

| Method | Path | Operation | Scope | Summary |
| --- | --- | --- | --- | --- |
| `POST` | `/hooks/{name}` | `receiveWebhook` | none | Receive a webhook for a configured trigger |

### receiveWebhook

`POST /hooks/{name}`

The control plane verifies the delivery, stores it as a job, and answers at once. A
worker creates the session later, so a slow cluster cannot make the sender retry.

A duplicate delivery returns `202` with `duplicate: true` and stores no second job. A
Slack URL verification request returns the challenge. A Slack trigger returns Slack's
acknowledgement body rather than the job record.

A body over the trigger limit (`server.webhook_max_body_bytes`, or
`triggers[].max_body_bytes` when set) returns `413 payload_too_large`. The check runs
before signature verification, so an oversize delivery does not spend a rate-limit
token.

Parameters:

| Name | In | Required | Type |
| --- | --- | --- | --- |
| name | path | yes | `string` |

Request body: `object` (required). Content types: `application/json`, `application/x-www-form-urlencoded`.

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 200 | Slack acknowledgement or Slack URL verification challenge. | `oneOf` |
| 202 | The delivery is queued. | `Accepted` |
| 400 | The request is not valid. | `Error` |
| 401 | The caller is not authenticated: a missing, unknown, or expired token, an invalid webhook signature, or an invalid attach ticket. | `Error` |
| 404 | The object or route does not exist. | `Error` |
| 413 | The body or artifact is over the configured limit. On `POST /hooks/{name}`, the limit is `server.webhook_max_body_bytes` or the trigger's `max_body_bytes`. | `Error` |
| 429 | A rate limit or capacity limit is reached. `Retry-After` is set. | `Error` |
| 500 | An internal failure. The message never carries internals. | `Error` |
| 503 | A dependency is not available. | `Error` |

## health

| Method | Path | Operation | Scope | Summary |
| --- | --- | --- | --- | --- |
| `GET` | `/healthz` | `getHealth` | none | Liveness probe |
| `GET` | `/readyz` | `getReady` | none | Readiness probe |

### getHealth

`GET /healthz`

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 200 | The process is alive. | `object` |

### getReady

`GET /readyz`

Reports local startup state only. The artifact store and other remote dependencies are
not checked, because removing the last replica from the Service would drop every live
session that reaches the control plane through it.

Responses:

| Status | Description | Schema |
| --- | --- | --- |
| 200 | The replica can serve traffic. | `object` |
| 503 | The replica is starting or shutting down. | `object` |

## Error codes

Every failed request returns `Error` with one of these `error.code` values:

| Code |
| --- |
| `invalid_request` |
| `unauthenticated` |
| `forbidden` |
| `not_found` |
| `method_not_allowed` |
| `conflict` |
| `payload_too_large` |
| `rate_limited` |
| `unavailable` |
| `internal` |

## WebSocket messages

Server messages, as `WebSocketServerMessage`:

| Message | `type` |
| --- | --- |
| `WebSocketHello` | `hello` |
| `WebSocketEvent` | `event` |
| `WebSocketError` | `error` |

Client messages, as `WebSocketClientMessage`:

| Message | `type` |
| --- | --- |
| `WebSocketUserMessage` | `user_message` |
| `WebSocketPing` | `ping` |
