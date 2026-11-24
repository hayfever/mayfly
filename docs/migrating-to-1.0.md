# Migrating to 1.0.0

This guide moves an install from 0.3.0 to 1.0.0-rc.2. The release is additive
for the documented surfaces. No persisted format changed, so the control plane
reads the 0.3.0 state files without a migration step.

1.0.0 freezes the public surface. From this release, the HTTP API, webhook
receivers, CLI (including its environment variables), config keys, and Helm
values follow [deprecation.md](deprecation.md). A removal needs a major version.
Persisted stores carry their own format versions; see
[Persisted formats](configuration.md#persisted-formats).

## Before you upgrade

1. Read the 1.0.0-rc.1 and 1.0.0-rc.2 entries in [CHANGELOG.md](../CHANGELOG.md).
2. Back up the state volume. See [Backup the state volume](runbook.md#backup-the-state-volume).
3. Update your values file for any chart value that the release notes name. This
   release changes no existing value.
4. Check that every `/api/v1` client sends no unknown request field. A body with
   an unknown field is `400 invalid_request`. The rule is unchanged from 0.2.0;
   a client that ignores a `400` shows the fault now.

## Upgrade

1. Run a template check with your values:

```bash
helm upgrade mayfly oci://ghcr.io/<owner>/charts/mayfly --version 1.0.0-rc.2 \
  -n mayfly -f values.yaml --dry-run
```

2. Run the upgrade and wait for the rollout:

```bash
helm upgrade mayfly oci://ghcr.io/<owner>/charts/mayfly --version 1.0.0-rc.2 \
  -n mayfly -f values.yaml
kubectl -n mayfly rollout status deployment/mayfly --timeout=3m
```

3. Check `/readyz` and the session list. The steps are in
   [Upgrade](runbook.md#upgrade).

A running session keeps its pod and workspace claim across the upgrade. The new
control plane adopts the pod and continues the turn from the journal.

## Schedule time zones

A schedule accepts an optional `timezone` field holding an IANA name. Omitting
it keeps the existing behavior: the cron fields are read in UTC. No action is
needed for an existing bundle. See
[Schedules](sources.md#schedules) for the daylight-saving rules.

## Web UI sessions

The web UI no longer keeps the API token in browser storage. It exchanges the
token once for an `HttpOnly`, `SameSite=Strict` session cookie, so page script
cannot read the token.

1. Sign in once with the token in Settings. The control plane sets
   `mayfly_session` and `mayfly_csrf`; the token field is cleared.
2. Sign out with the Sign out control. `DELETE /api/v1/auth/session` expires both
   cookies. An old `mayfly.token` value in localStorage is deleted on load, so
   you do not need to clear storage.
3. A control-plane restart ends every browser session. Sign in again after an
   upgrade or a restart.
4. Sessions live in the control-plane process, not in the state volume. The chart
   runs one replica (`replicas: 1`), so one process holds every session.

Browsers, not other clients, meet these rules. A request that carries an
`Authorization: Bearer` header is authenticated by that header alone, and a
wrong header is not rescued by a valid cookie. A cookie-authenticated request
that mutates state (`POST`, `PUT`, `PATCH`, `DELETE`) must send `X-Mayfly-CSRF`
with the value of the `mayfly_csrf` cookie. An `Origin` header must match
`server.public_url` or the request host. See
[Web UI session](security.md#web-ui-session) for the full rule.

Sign-in itself accepts only an `application/json` body and refuses an `Origin`
that fails the same check with `403 forbidden`.

Automation that calls the API with a bearer token needs no change.

## Audit log line

A mutating, authenticated `/api/v1` request writes one audit line. The line
carries a `via` field: `bearer` for an `Authorization` header, or `web` for a
session cookie. A failed authentication on a scoped route now writes one line
with `token_name` set to `unknown` and status `401`; a failure over the per-IP
budget has status `429`.

Audit lines are written through a dedicated logger and no longer depend on
`MAYFLY_LOG_LEVEL`. With `MAYFLY_LOG_LEVEL=warn` or `error`, the request log
gets quieter but the audit trail does not.

A log consumer that matches an exact key set must accept `via`. A consumer that
reads named fields needs no change. The message `audit` and the field set are
documented in [Audit log](security.md#audit-log) and stay stable through 1.x.

## Token length

A resolved API token secret must be at least 32 characters. Shorten nothing:
replace a short value with at least 32 random characters before you upgrade.
A shorter value stops startup with an error that names the config key and
environment variable. Run `mayflyd --config <path>` once to check before you
deploy.

## Session memory and request bodies

The inbox of unread human messages now has its own byte budget.
`sessions.inbox_buffer_bytes` defaults to `sessions.event_buffer_bytes`
(8 MiB). Worst case per session is now `event_buffer_bytes` plus
`inbox_buffer_bytes`, about 16 MiB at the defaults. Before, the inbox had only
the count cap and could reach about 164 MiB with defaults. Existing
configuration files need no change. Keep `inbox_buffer_bytes` above 32 KiB,
because one human message can be that large and a larger message than the
whole budget is dropped.

A request body that does not arrive within `server.body_read_timeout`
(default 30s) now gets `408 request_timeout` and the connection closes. This
covers `/api/v1` JSON bodies, `/hooks/{name}`, and the internal JSON endpoints.
WebSocket attach, event and inbox streams, the LLM proxy, and artifact uploads
keep their own behavior.

## Skill cache

`skills.cache_dir` stores verified skill source archives by SHA-256. A restart
reads a cached archive and makes no request to the archive host.

- An empty `skills.cache_dir` derives `<journal directory>/skill-cache` when the
  journal backend keeps a file.
- The chart sets `/var/lib/mayfly/skill-cache`, which is on the state volume.
- The control plane deletes entries at startup when no configured source names
  their digest. A cache entry that fails its digest check is fetched again.
- A failed cache write logs a warning and does not stop startup.

The cache holds source archives, not secrets. A restart needs the archive host
only when no cache entry exists.

## Release gate

1.0 adds a stability gate. Before a release tag, the release commit must pass
20 of 20 e2e runs plus the repeated unit stress:

```bash
gh workflow run e2e-stability.yml -f runs=20 -f ref=<40-hex commit sha>
```

The workflow rejects a branch or tag name. See
[Stability gate](testing.md#stability-gate). This step applies to the
maintainers who tag the release, not to an operator.

## Rollback

The state format is unchanged, so a rollback to 0.3.0 reads the same files. Stop
the new control plane first, then run `helm rollback mayfly -n mayfly`. Browser
sessions end with the restart, and 0.3.0 ignores the skill cache directory.
Neither affects session recovery.
