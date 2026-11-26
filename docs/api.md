# HTTP API

[`api/openapi.yaml`](../api/openapi.yaml) is the source of truth for this API: every route, request field, response field, error code, and WebSocket message type. This page explains the same surface for people, and it covers behavior that a schema cannot show, such as when a worker creates a session.

[api-reference.md](api-reference.md) is the generated reference: one table for every tag, then each operation with its parameters, request body, responses, and error codes. `go run ./hack/apidoc` writes that file from the spec, and a stale copy fails `go test ./...`.

The admin API at `/api/v1` and the webhook receivers at `/hooks/{name}` are the public API. A change to either starts in the spec, and a pull request that breaks a client fails the `api-diff` CI job.

`/internal/v1` is the session runtime protocol. It is versioned with the runtime image and is **outside** this API promise. The sections below that describe `/internal/v1` are notes for runtime authors, not a compatibility commitment.

Admin endpoints accept a named API token as `Authorization: Bearer <token>` or the web UI session cookie from `POST /api/v1/auth/session`. Each operation in the spec lists the scope the credential needs, and [security.md](security.md#api-authentication) maps every route to its scope. A missing, unknown, or expired credential gets `401`; a credential without the scope of the route gets `403`. When a request carries a bearer token, the server checks only that token and ignores any cookie. A cookie-authenticated mutating request must also send `X-Mayfly-CSRF`. Internal endpoints require the session token. JSON bodies use `Content-Type: application/json`.

## Errors

Every failed request to `/api/v1` or `/hooks/{name}` returns one envelope:

```json
{"error": {"code": "invalid_request", "message": "ttl must be a duration such as 30m"}}
```

`code` is stable: match on it. `message` is for people: do not match on it. `details` holds optional, code-specific context, and it always carries `request_id`. The same value is in the `X-Request-Id` response header and in the request log line. See [observability.md](observability.md).

| Code | Status | Meaning |
|---|---|---|
| `invalid_request` | 400 | Bad JSON, an unknown request field, a bad `limit` or `cursor`, an unknown agent, skill, or MCP server, a bad TTL, a bad tool turn or step, or an unverifiable webhook body |
| `unauthenticated` | 401 | Missing, unknown, or expired API token, invalid webhook signature, or invalid attach ticket |
| `forbidden` | 403 | The credential does not carry the scope of the route, a cookie-authenticated mutation failed the CSRF check, a web sign-in came from another origin, or the session may not use the requested tool |
| `not_found` | 404 | Unknown route, session, trigger, artifact, job, dead job, or schedule; a tool route for a non-built-in harness |
| `method_not_allowed` | 405 | The route exists with another method; `Allow` names the method |
| `conflict` | 409 | The session is closed, a stored queue job cannot be decoded by this build, or a tool call key was reused with another tool or other arguments |
| `payload_too_large` | 413 | Body or artifact over the configured limit |
| `request_timeout` | 408 | The request body did not arrive within `server.body_read_timeout`. The server answers and closes the connection |
| `rate_limited` | 429 | Trigger rate limit, `queue.max_jobs` reached, `sessions.max_concurrent` reached on the admin API, a plugin rate limit, or too many failed authentications from one client; `Retry-After` is set |
| `unavailable` | 503 | A dependency is not available: the queue rejected the write, the queue store is down, or schedules are not configured |
| `internal` | 500 | A server fault. The message carries no internals; the details are in the control-plane log |

A panic in a handler returns `500` with `code: internal`.

If `POST /api/v1/sessions` fails after the session record exists, the response is `500` with `error.details.session_id`. The record stays readable and can be deleted.

## Pagination

`GET /api/v1/sessions`, `GET /api/v1/queue/dead`, and `GET /api/v1/schedules` return one page:

```json
{"items": [], "next_cursor": "eyJ0IjoiMjAyNi0xMS0xOFQxODowMDowMFoiLCJpZCI6ImFiYzEyMyJ9"}
```

- `limit` defaults to 50 and accepts 1 to 500. Any other value is `400 invalid_request`.
- `cursor` is opaque. Send back the `next_cursor` of the previous page unchanged.
- The last page omits `next_cursor`.
- Sessions are newest first, with the session id as tie-break.
- Dead jobs are newest failure first, with the job id as tie-break.
- Schedules are sorted by name.

A new session, or a job that changes state, can move between pages while you read them. A cursor points at one item, so a page never repeats an item and never skips one that is older than the cursor.

## Public

These routes do not require an API token, on purpose:

| Route | Authentication |
|---|---|
| `GET /healthz`, `GET /readyz` | None. Probes must work before a token is loaded. |
| `GET /`, `/app.js`, `/style.css` | None. The UI shell holds no session data; every data request still needs a token. |
| `POST /hooks/{trigger}` | The trigger verifies the sender: a signature or a shared token, and `allow_unsigned` when an operator disables it. An unverifiable delivery is `401`. |
| `GET /api/v1/sessions/{id}/ws` | A one-time ticket from `POST /api/v1/sessions/{id}/attach-ticket`. |
| `GET /metrics` | A token with the `admin` scope on the main listener unless `server.metrics.require_auth` is false. A separate `server.metrics.address` listener is open by default. |

Every other `/api/v1` route requires a named API token and the scope named in the spec.

### `GET /healthz`, `GET /readyz`

`/healthz` returns `{"status":"ok"}` whenever the process is alive. `/readyz` returns `200 {"status":"ready"}` once the lifecycle engine has started, and `503` before that or during shutdown. It checks local state only: the artifact store and other remote dependencies stay out of the probe, because this replica also serves live session traffic through the same Service. No authentication.

### `GET /`

Web UI. Static assets at `/app.js` and `/style.css`.

### `POST /hooks/{trigger}`

Receives a webhook for a configured trigger. The session is created after the response, by a queue worker, so the webhook never waits for Kubernetes.

Responses:

- Slack: `200 {"response_type":"in_channel","text":"Mayfly session queued."}`.
- Others: `202 {"accepted":true,"duplicate":false,"job_id":"...","delivery":"...","status":"queued"}`. The manual schedule run and the dead-job retry return the same object.
- Duplicate delivery (same `X-GitHub-Delivery`, Slack `trigger_id`, or `Idempotency-Key`): `202` with `"duplicate":true`. No second job is stored.
- Slack URL verification: `200 {"challenge":"..."}`.
- Body over the trigger limit (`server.webhook_max_body_bytes`, or `triggers[].max_body_bytes` when set): `413` with `code: payload_too_large`. The check runs before signature verification, so an oversize delivery does not spend a rate-limit token.
- Session options the control plane cannot run (unknown agent, skill, or MCP server; model not in `llm.allow_models`; empty prompt): `400` with `code: invalid_request` and the reason. Nothing is queued.
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

`prompt` is required. Empty `agent` selects the default agent. Empty `skills` or `mcp_servers` selects the agent defaults. `model` must be the default or in `llm.allow_models`. A `title` longer than 200 bytes is truncated rune-safely at the limit, and a title derived from the first line of the prompt gets the same bound.

A request body with a field this build does not know returns `400 invalid_request`. A typo in an option is therefore visible, not silently ignored. The same rule applies to the message, close, and attach-ticket bodies.

The session object holds the id, title, status, prompt, agent, harness, model, skills, MCP servers, plugins, trigger, TTL, timestamps, message count, model usage, model budget, artifact metadata, and error. `llm_usage` reports prompt tokens, completion tokens, forwarded model calls, and how many calls were estimated. `llm_budget` reports the resolved token and request limits, and it is absent when the session has no limit. The durable reservation of an in-flight call is not exposed. The runtime backend, the pod name, and the artifact store URI are control-plane internals and never appear in a response. Download the bundle through `GET /api/v1/sessions/{id}/artifact`.

Response `201`:

```json
{
  "session": { "...": "..." },
  "url": "http://localhost:8080/#/sessions/abc123"
}
```

The control plane checks the request before it stores the session, so a bad agent, skill, model, or TTL is `400 invalid_request`. If the runtime does not start, the response is `500` with `error.details.session_id` naming the failed record.

### `GET /api/v1/sessions?limit=50&cursor=...`

One page of sessions, newest first.

```json
{"items":[{"id":"abc123","status":"running","...":"..."}],"next_cursor":"..."}
```

### `GET /api/v1/sessions/{id}`

`{"session":{...},"url":"..."}`.

### `DELETE /api/v1/sessions/{id}`

Force-stops the session and marks it failed. A live session keeps its record until `sessions.retention` expires; a session restored from the record store is removed at once. `204`.

DELETE is safe while an adoption retry is in flight. The retry either registers first, and DELETE stops that session, or DELETE removes it first, and the retry refuses to register it. The session is not listed again after the `204`.

### `GET /api/v1/sessions/{id}/events?after=<seq>`

Returns retained events with sequence greater than `after`. The event object
and every payload schema are documented in [events.md](events.md). The
`type` field is open: a new event type and a new optional payload field are
additive, and a client must ignore a type this build does not know.

```json
{"session_id":"abc123","events":[{"seq":4,"type":"assistant_message","text":"...","time":"..."}]}
```

### `POST /api/v1/sessions/{id}/messages`

```json
{"text":"also check the database","actor":"alice"}
```

Appends a human message and delivers it to the runtime. `202 {"event":{...}}`.

The `202` is a durability promise: the control plane writes the message to a
pending-inbox record in the journal before it returns, using the event `seq` as
the turn key. If the control plane restarts before the runtime starts the turn,
adoption redelivers the message under that same sequence. The record is removed
once the runtime journals step 0 of the turn. A journal write failure returns
`500` and no `202`. The initial prompt of a session, from the API or a webhook,
follows the same rule.

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
{"type":"ping"}
```

The actor name comes from the ticket. Multiple clients share the thread. The
server sends ping frames every 30 seconds. Events are replayed from `after`
(default 0); reconnect with `after=0` to rebuild the view. The event object and
the compatibility rules are documented in [events.md](events.md).

The message types are `WebSocketServerMessage` and `WebSocketClientMessage` in the spec.

### `GET /api/v1/sessions/{id}/artifact`

Downloads the latest bundle as `application/gzip`. Returns `404 not_found` until finalize has stored it. Artifact metadata outlives `sessions.retention` and the control-plane process, so the bundle stays downloadable after the session leaves the session list or the control plane restarts.

### `GET /api/v1/queue`

Accepted work that has not run yet.

```json
{"queue":{"pending":2,"leased":1,"dead":0,"oldest":"2026-11-11T13:41:49Z"}}
```

`pending` jobs are claimable now. `leased` jobs are not claimable yet: a worker holds them, or they are waiting out a retry backoff or a capacity delay. `dead` jobs exhausted `queue.max_attempts`.

### `GET /api/v1/queue/jobs/{id}`

One accepted job by id, including a completed job for `queue.tombstone_ttl`.
The stored job is the versioned job envelope described in
[queue-jobs.md](queue-jobs.md).

```json
{"job":{"id":"d-...","trigger":"github","state":"done","attempts":0,"accepted_at":"...","finished_at":"...","session_id":"abc123","prompt":"..."}}
```

`state` is `queued`, `leased`, `dead`, or `done`. A job that created a session reports `session_id`; the session reports the same id in `trigger.job_id`. Unknown or expired ids return `404 not_found`. A stored payload that this build cannot decode returns `409 conflict`, with the decoder reason in `message`; the job stays in place.

### `GET /api/v1/queue/dead?limit=50&cursor=...`

Buried jobs, newest failure first. The prompt is omitted from the list; use the job lookup for the full record.

```json
{"items":[{"id":"d-...","trigger":"generic","state":"dead","attempts":5,"accepted_at":"...","failed_at":"...","error":"unknown agent \"triage\""}],"next_cursor":"..."}
```

The queue store has no cursor, so this route reads the buried set and cuts the page in the control plane. Buried jobs are operator-facing and few; a store-level cursor is a follow-up for a deployment that accumulates many.

### `POST /api/v1/queue/dead/{id}/retry`

Puts a buried job back on the queue with its attempt count reset. `202` with the accepted-job object: `{"accepted":true,"duplicate":false,"job_id":"d-...","delivery":"...","status":"queued"}`. Unknown job: `404 not_found`.

### `DELETE /api/v1/queue/dead/{id}`

Drops a buried job without running it. `204`. Unknown job: `404 not_found`.

### `GET /api/v1/schedules?limit=50&cursor=...`

The schedules compiled from agent files at startup, sorted by name.

```json
{"items":[{"name":"page-watch","agent":"page-watch","cron":"0 7 * * *","source":"watch-pages"}],"next_cursor":"..."}
```

A schedule with a `timezone` adds `"timezone":"America/Chicago"`: the IANA zone the cron fields are read in. The field is omitted when the bundle omits it. An omitted or empty value means UTC. See [sources.md](sources.md#schedules).

### `POST /api/v1/schedules/{name}/run`

Enqueues one manual run of the named schedule and returns `202` with the accepted-job object: `{"accepted":true,"duplicate":false,"job_id":"s-...","delivery":"manual:...","status":"queued"}`. The run writes its artifact and creates a session when the change totals match the schedule trigger, the same as a cron slot. Manual runs use a random delivery key, so they do not consume or block a cron slot. Unknown schedule: `404 not_found`. Scheduler not started, or the queue is full: `503 unavailable`.

### `POST /api/v1/auth/session`

Exchanges an API token for a browser session. Body: `{"token":"<secret>"}` with `Content-Type: application/json`; any other content type returns `400 bad_request`. A request whose `Origin` header names another site returns `403 forbidden` with message `cross-site sign-in refused`. `201` with `{"name":"ci","scopes":["sessions:read"],"expires_at":"2026-11-20T00:00:00Z"}`. Sets `mayfly_session` (HttpOnly, SameSite=Strict, Secure over https) and `mayfly_csrf` (readable by the page). The session lasts 12 hours, or less when the token's `expires_at` is earlier. An unknown or expired token returns `401 unauthenticated` and sets no cookie. Sessions are in memory: a restart signs every browser out.

### `GET /api/v1/auth/session`

Returns the current session as `{"name","scopes","expires_at"}` from the `mayfly_session` cookie, or `401 unauthenticated`. A bearer token does not authenticate this route.

### `DELETE /api/v1/auth/session`

Ends the session and expires both cookies. `204`. Requires the session cookie and `X-Mayfly-CSRF` with the `mayfly_csrf` value; a mismatch returns `403 forbidden` with message `csrf check failed`. Without a valid session, `401 unauthenticated`.

### `GET /api/v1/config`

UI metadata: default and allowed models, agents with effective harness/model/image/skills/MCP/limits, skills, MCP server names, trigger names, TTL limits. The `schedules` array here is a plain list; use `GET /api/v1/schedules` to page.

## Internal (session runtime only, outside the API promise)

These endpoints accept the scoped session token, never an API token. They keep the plain `{"error":"..."}` body and the shapes below for the runtime image of the same release. They are not covered by `api/openapi.yaml` and may change in any release.

| Endpoint | Purpose |
|---|---|
| `POST /internal/v1/sessions/{id}/events` | Append runtime events. An event may include `step_key`; a repeated key is dropped |
| `GET /internal/v1/sessions/{id}/inbox?after=<seq>` | SSE stream of human messages and close controls |
| `POST /internal/v1/sessions/{id}/journal` | Append one completed turn step (`session_id`, `turn`, `step`, `name`, `input`, `output`, `err`, `conv_len`, `time`) |
| `GET /internal/v1/sessions/{id}/journal?turn=<n>` | Steps for one turn. Omit `turn` for the last turn, which also returns its number |
| `POST /internal/v1/sessions/{id}/artifacts` | Upload a tar.gz bundle. Requires `Content-Length` and `X-Mayfly-SHA256`. Optional `X-Mayfly-Partial: true` |
| `GET /internal/v1/sessions/{id}/artifacts?key=<key>&offset=<n>&limit=<n>` | Read a window of an input artifact mounted on the session (default limit 24 KiB). Backs `read_artifact` |
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
