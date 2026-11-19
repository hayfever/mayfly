# Migrating to 0.2.0

This guide moves an install from 0.1.x or a 0.1.0 release candidate to 0.2.0.
0.2.0 is pre-1.0, so it has breaking changes. Read [CHANGELOG.md](../CHANGELOG.md)
for the full list.

A 0.2.0 control plane writes a newer state format. You cannot run 0.1.x on that
state. Back up the state volume before the upgrade. To go back, restore the
backup and run the build that wrote it.

## Before you upgrade

1. Read the 0.2.0 breaking changes.
2. Back up the state volume. See [Backup the state volume](runbook.md#backup-the-state-volume).
3. Check that the claim `<release fullname>-state` has room for `sessions.db`. The default path is `/var/lib/mayfly/sessions.db`.
4. Export any artifact from a running session that you must keep. The state backup does not contain the per-session workspace claims.
5. Add `version: 1` to the config file. A missing key is accepted and logs a warning. An unknown value stops startup with exit code 2.

## Helm values

Remove `replicaCount`. The chart pins one replica and `values.schema.json`
rejects unknown top-level values. Two replicas cannot share the ReadWriteOnce
state volume.

Set `shutdown.gracePeriodSeconds` higher than `shutdown.budgetSeconds`.
`helm template` fails when the budget does not fit. The defaults are 25 and 30
seconds.

If you use `server.tokens`, put the token secrets in the Secret that
`controlPlane.tokensSecret` names. The legacy `server.token_env` remains one
token named `default` with the `admin` scope when `server.tokens` is empty.

## API responses

The changes apply to `/api/v1` and `/hooks/{name}`:

1. Errors now use `{"error":{"code":"...","message":"...","details":{...}}}`. A 401 has code `unauthenticated`. Unknown failures return 500 `internal`.
2. List routes return `items` and an optional `next_cursor`. The former `sessions`, `dead`, and `schedules` arrays are gone.
3. `limit` accepts 1 to 500. Any other value is 400 `invalid_request`.
4. Schedules are sorted by name.
5. CLI errors print `code: message`. Update scripts that parse the old text.

Follow `next_cursor` until it is absent. Do not build a cursor value.

These response shapes also changed:

- `POST /api/v1/queue/dead/{id}/retry` returns the shared accepted-job object `{"accepted","duplicate","job_id","delivery","status"}`. It returned `202 {"retried":true,"id":"..."}` before.
- Session JSON no longer includes `backend`, `pod_name`, or `artifact.uri`. Download the bundle through `GET /api/v1/sessions/{id}/artifact`.
- Unknown fields in `/api/v1` request bodies return `400 invalid_request`. Webhook bodies from external senders stay permissive.

## API tokens

1. `mayflyd` exits with code 2 when no token is configured. It no longer generates a token and logs the secret.
2. A non-empty `server.tokens` list replaces `server.token_env`. Add that variable as an explicit token to keep it.
3. Every route requires a scope. The legacy `default` token keeps `admin`. `POST /api/v1/sessions/{id}/attach-ticket` requires `sessions:write`.
4. The CLI reads `MAYFLY_API_TOKEN`. `MAYFLY_TOKEN` still works as a deprecated alias and prints one warning.

On the main port, `/metrics` requires a token with the `admin` scope. Use
`server.metrics.address` for an unauthenticated in-cluster scrape, or set
`server.metrics.require_auth` on the separate address.

## Sessions and state

Started and finished sessions now persist in `sessions.db`. Put the file on the
control-plane PersistentVolume.

A restarted control plane lists finished sessions that retention still covers.
`DELETE` on a restored session returns 204. A state file with a newer
`format_version` stops startup with an error that names the file.

An adoption failure reports the current attempt count with the correct singular
or plural. `DELETE` is safe against an adoption retry in flight.

## Queue jobs

The raw queue payload is now the version 1 envelope in
[queue-jobs.md](queue-jobs.md). Mayfly decodes the 0.1.x shape and rewrites it on
the next record write. A third party that reads or writes raw queue records must
use the version 1 envelope.

The job id comes from the idempotency key, scoped by kind and trigger. Two
envelopes with the same `idempotency_key` and different ids are duplicates. A
producer-supplied id does not change the dedupe key.

`GET /api/v1/queue/jobs/{id}` returns `409 conflict` with the decoder reason for
a payload it cannot decode. Do not retry on `409`. A version 1 `session.create`
without a prompt, or a `source.run` without a complete source, is buried with a
reason instead of claimed and retried.

## Events

The WebSocket and event wire format keeps its JSON keys. Clients must ignore an
unknown event type and an unknown field. The final `status` event can now say
`"status":"closing"` and omits `error` when there is no failure. `Event.type` and
`Trigger.kind` are no longer closed enums in the OpenAPI document, so a new type
or optional field is additive.

Go importers of `internal/model` and `internal/journal` must use
`model.EventData` and `model.DataAs[T]` instead of `map[string]any`.

## Release artifacts

Images and the chart are signed from 0.2.0. Releases before 0.2.0 are not
signed. Verify a new release with the commands in
[Verify a published release](release.md#verify-a-published-release).

## Upgrade

1. Run a template check with your values:

```bash
helm upgrade mayfly oci://ghcr.io/<owner>/charts/mayfly --version 0.2.0 \
  -n mayfly -f values.yaml --dry-run
```

2. Run the upgrade and wait for the rollout:

```bash
helm upgrade mayfly oci://ghcr.io/<owner>/charts/mayfly --version 0.2.0 \
  -n mayfly -f values.yaml
kubectl -n mayfly rollout status deployment/mayfly --timeout=3m
```

3. Check `/readyz` and the session list, then read the startup log for adopt results. The steps are in [Upgrade](runbook.md#upgrade).

A running session keeps its pod and workspace claim across the upgrade. The new
control plane adopts the pod and continues the turn from the journal.
