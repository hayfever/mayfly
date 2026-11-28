# Queue jobs

Mayfly accepts work as a versioned JSON job envelope. A producer writes one
envelope to a queue backend. A worker decodes the envelope before it runs the
job. The envelope is the public queue contract. It is not an HTTP request body.

`api/job.v1.schema.json` is the source of truth for version 1. `api/openapi.yaml`
references it as `JobEnvelope`. `hack/api-diff.sh` compares the schema with the
newest release tag. It accepts a new optional property and fails on a removal,
a rename, a type or constraint change, or a change to a required list.

## Envelope

```json
{
  "version": 1,
  "kind": "session.create",
  "id": "d-4f0c...",
  "idempotency_key": "delivery-123",
  "created_at": "2026-11-18T09:00:00Z",
  "agent": "triage",
  "prompt": "Investigate the alert",
  "session": {
    "title": "Alert",
    "model": "gpt-5",
    "ttl": "45m",
    "skills": ["reporting"],
    "mcp_servers": ["echo"],
    "artifacts": [
      {
        "key": "runs/a.json",
        "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        "bytes": 128,
        "content_type": "application/json"
      }
    ]
  },
  "trigger": {"kind": "generic", "name": "alerts", "event": "webhook"},
  "metadata": {"team": "platform"}
}
```

### Job identity

| Field | Type | Meaning |
| --- | --- | --- |
| `version` | integer | Required. The major version. The only value is `1`. |
| `kind` | string | Required. `session.create` or `source.run`. |
| `id` | string | The job id. Mayfly assigns it when a message has none. |
| `idempotency_key` | string | The producer's deduplication key. Optional. |
| `created_at` | date-time | Required. When the producer created the job, in UTC. |
| `attempts` | integer | Mayfly maintains it. A producer omits it. |
| `metadata` | object | Producer labels. Mayfly does not interpret them. |

### Work

| Field | Type | Meaning |
| --- | --- | --- |
| `agent` | string | Agent name. Empty selects the default agent. |
| `prompt` | string | The first user message. Required for `session.create`. |
| `session.title` | string | Session title. Empty uses the first line of the prompt. |
| `session.model` | string | A model from `llm.allow_models`. Empty uses the default. |
| `session.ttl` | string | A Go duration such as `30m`. Empty uses the configured default. |
| `session.skills` | string array | Skills the session may use. |
| `session.mcp_servers` | string array | MCP servers the session may call. |
| `session.artifacts` | object array | Inputs the producer stored. The session may read them. |

An artifact has `key`, `sha256`, `bytes`, and an optional `content_type`. The
control plane checks the reference and does not fetch the bytes.

### Cause

| Field | Type | Meaning |
| --- | --- | --- |
| `trigger.kind` | string | Required. For example `github`, `generic`, `schedule`, or `api`. |
| `trigger.name` | string | Configured trigger or schedule name. |
| `trigger.event` | string | Event within the trigger, such as `pull_request`. |
| `trigger.actor` | string | The account that caused the event, when known. |
| `source` | object | The scheduled run. Required for `source.run`. |

A `source` object has `schedule`, `source`, and `slot`. It also accepts
`run_id`, which defaults to the job id.

## Kinds

A `session.create` job creates one session from the prompt and options. Webhook
deliveries use it. A `source.run` job runs one scheduled data source. It creates
a session only when the run has changes that the schedule triggers on. Cron
slots and manual schedule runs use it.

## Versioning

The version is a major version. These rules keep a version 1 reader compatible
with later producers:

1. Add an optional field without a new version. A reader ignores fields it does
   not know.
2. Never remove, rename, or retype a field in version 1. Never change what an
   existing field means. Publish version 2 instead.
3. A reader rejects a version it does not know. The store buries the job with
   the version in the error, so the queue keeps moving.
4. A reader also rejects an unknown `kind`, a `session.create` without a
   `prompt`, and a `source.run` without a complete `source`. A build that does
   not implement a kind, or that reads a job the schema rejects, buries the job
   with the reason instead of running it.
5. Validate producer output against `api/job.v1.schema.json` in tests.

## Idempotency

`idempotency_key` is the deduplication key. The key is scoped by the job kind
and the trigger. A repeated message with the same key is accepted once. Later
copies receive a duplicate answer while `queue.dedupe_ttl` holds the key.
Without a key every message is a new job. A producer that supplies its own `id`
still dedupes on `idempotency_key`: the same key with a different `id` is a
duplicate.

The job id is deterministic for a key. Mayfly also uses the job id as the
session idempotency key, so a worker that stops between Create and Ack does not
start a second session when the job runs again.

Webhook producers use the sender's delivery key, such as
`X-GitHub-Delivery` or `Idempotency-Key`. A delivery key must be valid UTF-8;
a request whose key is not valid UTF-8 gets `400` with
`code: invalid_request` and is not queued. The control plane never rewrites a
key, because the key is the dedupe identity. A schedule slot uses
`schedule:<name>:<RFC 3339 slot>`. A manual run uses a random key, so it neither
consumes nor blocks a cron slot.

## Storage and upgrades

The bbolt and redis stores write the envelope as the job payload. The memory
store holds the same fields in process memory. The envelope is independent of
the backend, and a backend driver does not change the contract.

A store from v0.1.0 or a release candidate holds the earlier shape: a top-level
`options` object with capitalized field names, a numeric `ttl`, and the kinds
`session` and `source`. The reader decodes that shape and keeps the job. The
next write of the record uses the version 1 envelope.

A payload that does not decode, an unknown major version, or an unknown kind is
moved to the dead set. The record keeps the original job id and the decode
reason. `GET /api/v1/queue/dead` shows it, and the admin API can retry or delete
it.
