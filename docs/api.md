# HTTP API

The API paths are versioned (`/api/v1`, `/internal/v1`). During 0.x, request fields, response fields, and event types may change in minor releases. Pin a release and read the changelog before upgrading. The session runtime protocol is version 1 and should be treated as experimental until 1.0.

All admin endpoints require `Authorization: Bearer <MAYFLY_API_TOKEN>`. Internal endpoints require the session token. JSON bodies use `Content-Type: application/json`.

Error responses:

```json
{"error": "message"}
```

| Status | Meaning |
|---|---|
| 400 | Invalid request, unknown skill/server, bad TTL, bad tool turn or step |
| 401 | Missing or wrong token, invalid webhook signature |
| 403 | Tool is not granted to the session |
| 404 | Unknown session, trigger, artifact, job, or dead job; a tool route for a non-built-in harness |
| 409 | Session is closed, or a tool call key was reused with another tool or other arguments |
| 413 | Body or artifact too large |
| 429 | Trigger rate limit, `queue.max_jobs` reached, `sessions.max_concurrent` reached on the admin API, or a plugin rate limit; `Retry-After` is set |
| 500 | Startup, storage, or internal failure |
| 503 | Control plane is not ready (`/readyz`), the tool gateway's in-flight cap is reached, or the queue rejected the write |

## Public

### `GET /healthz`, `GET /readyz`

`/healthz` returns `{"status":"ok"}` whenever the process is alive. `/readyz` returns `200 {"status":"ready"}` once the lifecycle engine has started, and `503` before that or during shutdown. It checks local state only: the artifact store and other remote dependencies stay out of the probe, because this replica also serves live session traffic through the same Service. No authentication.

### `GET /`

Web UI. Static assets at `/app.js` and `/style.css`.

### `POST /hooks/{trigger}`

Receives a webhook for a configured trigger. The session is created after the response, by a queue worker, so the webhook never waits for Kubernetes.

Responses:

- Slack: `200 {"response_type":"in_channel","text":"Mayfly session queued."}`.
- Others: `202 {"accepted":true,"duplicate":false,"job_id":"...","delivery":"...","status":"queued"}`.
- Duplicate delivery (same `X-GitHub-Delivery`, Slack `trigger_id`, or `Idempotency-Key`): `202` with `"duplicate":true`. No second job is stored.
- Slack URL verification: `200 {"challenge":"..."}`.
- Session options the control plane cannot run (unknown agent, skill, or MCP server; model not in `llm.allow_models`; empty prompt): `400` with the reason. Nothing is queued.
- Over the trigger rate limit, or when `queue.max_jobs` is reached: `429` with `Retry-After`. The rate limit counts only requests that pass signature verification, so unauthenticated traffic cannot exhaust a trigger.

Find the session created for a delivery with `GET /api/v1/queue/jobs/{job_id}`: it reports `state` and, once the worker runs, `session_id`. The session also carries the same id in `trigger.job_id`. Jobs accepted without a delivery key can still be looked up by `job_id` from the response, for `queue.tombstone_ttl` after completion.

## Sessions

### `POST /api/v1/sessions`

```json
{
  "prompt": "Triage the 5xx spike",
  "agent": "triage",
  "title": "optional",
  "ttl": "30m",
  "model": "gpt-4.1-mini",
  "skills": ["incident-triage"],
  "mcp_servers": ["github"]
}
```

`prompt` is required. Empty `agent` selects the default agent. Empty `skills` or `mcp_servers` selects the agent defaults. `model` must be the default or in `llm.allow_models`.

Response `201`:

```json
{
  "session": { "...": "..." },
  "url": "http://localhost:8080/#/sessions/abc123"
}
```

If the pod fails to start, the response is `500` with both `error` and the failed `session`.

### `GET /api/v1/sessions`

`{"sessions":[...]}` newest first.

### `GET /api/v1/sessions/{id}`

`{"session":{...},"url":"..."}`.

### `DELETE /api/v1/sessions/{id}`

Force-stops the session, marks it failed, and keeps the record until `sessions.retention` expires. `204`.

### `GET /api/v1/sessions/{id}/events?after=<seq>`

Returns retained events with sequence greater than `after`.

```json
{"session_id":"abc123","events":[{"seq":4,"type":"assistant_message","text":"...","time":"..."}]}
```

### `POST /api/v1/sessions/{id}/messages`

```json
{"text":"also check the database","actor":"alice"}
```

Appends a human message and delivers it to the runtime. `202 {"event":{...}}`.

### `POST /api/v1/sessions/{id}/close`

```json
{"reason":"done"}
```

Requests a graceful close. The runtime exports the artifact and exits. `202`.

### `POST /api/v1/sessions/{id}/attach-ticket`

```json
{"actor":"alice"}
```

Returns a one-time WebSocket ticket:

```json
{"ticket":"...","expires_at":"...","ws_url":"ws://localhost:8080/api/v1/sessions/abc123/ws?ticket=..."}
```

### `GET /api/v1/sessions/{id}/ws?ticket=...&after=<seq>`

WebSocket attach. The ticket is consumed on connect.

Server messages:

```json
{"type":"hello","session":{...}}
{"type":"event","event":{...}}
{"type":"error","message":"..."}
```

Client messages:

```json
{"type":"user_message","text":"message text"}
```

The actor name comes from the ticket. Multiple clients share the thread. The server sends ping frames every 30 seconds. Events are replayed from `after` (default 0); reconnect with `after=0` to rebuild the view.

### `GET /api/v1/sessions/{id}/artifact`

Downloads the latest bundle as `application/gzip`. Returns `404` until finalize has stored it. Artifact metadata outlives `sessions.retention`, so the bundle stays downloadable after the session leaves the session list. The index itself is in memory and resets when the control plane restarts; read the store directly after that.

### `GET /api/v1/queue`

Accepted work that has not run yet.

```json
{"queue":{"pending":2,"leased":1,"dead":0,"oldest":"2026-11-11T13:41:49Z"}}
```

`pending` jobs are claimable now. `leased` jobs are not claimable yet: a worker holds them, or they are waiting out a retry backoff or a capacity delay. `dead` jobs exhausted `queue.max_attempts`.

### `GET /api/v1/queue/jobs/{id}`

One accepted job by id, including a completed job for `queue.tombstone_ttl`.

```json
{"job":{"id":"d-...","trigger":"github","state":"done","attempts":0,"accepted_at":"...","finished_at":"...","session_id":"abc123","prompt":"..."}}
```

`state` is `queued`, `leased`, `dead`, or `done`. A job that created a session reports `session_id`; the session reports the same id in `trigger.job_id`. Unknown or expired ids return `404`.

### `GET /api/v1/queue/dead?limit=50`

Buried jobs, newest first. `limit` defaults to 50 and is capped at 500. The prompt is omitted from the list; use the job lookup for the full record.

```json
{"dead":[{"id":"d-...","trigger":"generic","state":"dead","attempts":5,"accepted_at":"...","failed_at":"...","error":"unknown agent "triage""}]}
```

### `POST /api/v1/queue/dead/{id}/retry`

Puts a buried job back on the queue with its attempt count reset. `202 {"retried":true,"id":"..."}`. Unknown job: `404`.

### `DELETE /api/v1/queue/dead/{id}`

Drops a buried job without running it. `204`. Unknown job: `404`.

### `GET /api/v1/schedules`

The schedules compiled from agent files at startup.

```json
{"schedules":[{"name":"competitor-watch","agent":"competitor-watch","cron":"0 7 * * *","source":"competitor-pages"}]}
```

### `POST /api/v1/schedules/{name}/run`

Enqueues one manual run of the named schedule and returns `202 {"job_id":"s-..."}`. The run writes its artifact and creates a session when the change totals match the schedule trigger, the same as a cron slot. Manual runs use a random delivery key, so they do not consume or block a cron slot. Unknown schedule: `404`. Scheduler not started: `503`.

### `GET /api/v1/config`

UI metadata: default and allowed models, agents with effective harness/model/image/skills/MCP/limits, skills, MCP server names, trigger names, TTL limits.

## Internal (session runtime only)

These endpoints accept the scoped session token, never the admin token.

| Endpoint | Purpose |
|---|---|
| `POST /internal/v1/sessions/{id}/events` | Append runtime events. An event may include `step_key`; a repeated key is dropped |
| `GET /internal/v1/sessions/{id}/inbox?after=<seq>` | SSE stream of human messages and close controls |
| `POST /internal/v1/sessions/{id}/journal` | Append one completed turn step (`session_id`, `turn`, `step`, `name`, `input`, `output`, `err`, `conv_len`, `time`) |
| `GET /internal/v1/sessions/{id}/journal?turn=<n>` | Steps for one turn. Omit `turn` for the last turn, which also returns its number |
| `POST /internal/v1/sessions/{id}/artifacts` | Upload a tar.gz bundle. Requires `Content-Length` and `X-Mayfly-SHA256`. Optional `X-Mayfly-Partial: true` |
| `POST /internal/v1/sessions/{id}/finalize` | `{"status":"completed"\|"failed","reason":"...","error":"..."}` |
| `POST /internal/v1/llm/chat/completions` | OpenAI-compatible proxy. The model must be the one assigned to the session; an empty model field is filled with it |
| `GET /internal/v1/sessions/{id}/tools` | The session's tool grant, sidecar discovery tokens, and unavailable owners |
| `POST /internal/v1/sessions/{id}/tools/calls` | Authorize one tool call. The answer is a recorded result or a dispatch |
| `POST /internal/v1/sessions/{id}/tools/calls/complete` | Report the result of a locally dispatched call |
| `POST\|GET\|DELETE /internal/v1/mcp/{server}` | HTTP MCP proxy; server must be assigned to the session |

Inbox events use SSE `event: message` frames whose data is the event JSON. Comments (`: ping`) are keepalives. If `after` is greater than the control plane's current sequence (for example after a control-plane restart), the inbox sends `event: reset`; the runtime reconnects with `after=0`.

### Tool calls

The three tool routes serve the built-in harness only. A session whose agent selects another harness gets `404`.

`GET /internal/v1/sessions/{id}/tools`:

```json
{
  "tools": [
    {"name": "plugin_tickets_get_ticket", "owner": "tickets", "placement": "service",
     "description": "...", "input_schema": {"type": "object"}, "idempotent": true, "timeout_ms": 120000}
  ],
  "sidecars": {"notify": {"discovery_token": "v1...."}},
  "errors": [{"owner": "tickets", "required": false, "error": "no ready replica"}]
}
```

Remote tools carry the description and schema from gateway discovery. Local tools carry the grant fields, and the runtime fills the rest from the plugin's `tools/list`. A required owner in `errors` fails the session at start. A `wildcards` map names `mcp.servers` entries whose tools the grant does not list; the runtime resolves a stdio tool against its own list.

`POST /internal/v1/sessions/{id}/tools/calls` with
`{"turn":3,"step":4,"call_id":"call_ab","tool":"plugin_notify_post","arguments":{}}`
answers with exactly one of:

```json
{"result": {"content": "...", "is_error": false, "replayed": false, "interrupted": false}}
{"dispatch": {"key": "s1/3/4", "timeout_ms": 30000, "token": "v1...."}}
```

`token` is set only for a sidecar tool. `replayed` marks a result the ledger already held. `interrupted` marks a non-idempotent call that a container restart cut short and the gateway did not run again.

`POST /internal/v1/sessions/{id}/tools/calls/complete` with
`{"key":"s1/3/4","content":"...","is_error":false}` answers `204`. A repeat with identical content is also `204`; a different result for a completed key is `409`, and an unknown key is `404`.
