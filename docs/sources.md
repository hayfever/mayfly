# Sources and schedules

A schedule runs a source chain and turns a material change into one agent
session. The agent receives a prompt with the changed items and an artifact
with every document of the run. Provider knowledge lives in the bundle, not in Go.

The first run of a new page is a baseline and starts no session. Later runs
compare against the snapshot the provider keeps.

## Bundle files

`agents.dirs` lists directories of `.yaml`, `.yml`, and `.json` files. Hidden
directories are skipped. Every file is one bundle:

```yaml
schemaVersion: 1
name: competitor-watch
version: 1.0.0

agents:
  - name: competitor-watch
    systemPrompt: ...
    schedules:
      - name: competitor-watch
        cron: "0 7 * * *"
        source: competitor-pages
        prompt: ...

connections:
  - name: firecrawl
    baseURL: https://api.firecrawl.dev/v2

sources:
  - name: competitor-pages
    type: http.json
    connection: firecrawl
    request:
      method: POST
      path: /scrape
```

`schemaVersion` must be `1`. `name` is the bundle identity. A top-level
`schedules` key is rejected; a schedule belongs to its agent.

The control plane reads the bundle at startup. Restart it after a change.
`mayflyd -check` loads the config and bundle, prints the resolved schedules
and plans, and exits:

```bash
mayflyd -check -config /etc/mayfly-config/config.yaml
```

```
agents: 1 schedules: 1 sources: 2 connections: 2 plugins: 0
agent competitor-watch
schedule competitor-watch agent=competitor-watch cron="0 7 * * *" source=competitor-pages ttl=30m0s trigger=changed
plan competitor-watch steps 2 terminal competitor-pages: competitors -> competitor-pages
```

## Scopes

Every connection and source has a scope.

| Scope | Visible to |
| --- | --- |
| `shared` (default) | Every agent |
| `agent:<name>` | The named agent only |

Visibility is one way. A private definition can use a shared one. A shared
definition must not use a private connection, need a private source, or
extend a private definition. A private scope must name a defined agent.
Duplicate names across files are startup errors, so a private copy needs a
new name and `extends`.

## `extends` and `overrides`

`extends` names another definition of the same kind in the same bundle set.
The child merges the parent with its own keys, using the RFC 7386 JSON merge
patch rule:

- A map merges key by key.
- A list or a scalar replaces the parent value.
- A `null` deletes the key.

The child cannot change `name` or `type`, and `extends` is removed after the
merge. There is no separate `overrides` key.

```yaml
sources:
  - name: competitors
    type: http.json
    connection: notion
    request:
      path: /databases/{{ env "NOTION_DATABASE_ID" }}/query
    extract:
      list: results

  - name: competitor-pages
    extends: competitors
    request:
      path: /scrape
```

## Connections

A connection is one origin with its credentials.

| Field | Meaning |
| --- | --- |
| `name` | Lowercase letters, digits, dot, underscore, hyphen |
| `baseURL` | Origin, without a trailing slash requirement |
| `headers` | Default request headers. A value of `env:NAME` reads that environment variable at startup. An unset variable stops startup and names the header |
| `requiredEnv` | Environment variables the templates read. An unset variable stops startup and names the connection |
| `optionalEnv` | Environment variables the templates may read. An unset variable renders as an empty string |
| `timeout` | Request timeout. Default `60s` |
| `allowHTTP` | Permits a plain `http://` origin and `http://` redirects. Default `false` |
| `allowPrivate` | Permits loopback, private, link-local, and NAT64 addresses, at dial time and on redirects. Default `false` |
| `scope` | `shared` or `agent:<name>` |

`env:NAME` must be the whole value. To build a header from text plus a
secret, use a request header template:

```yaml
headers:
  Authorization: 'Bearer {{ env "NOTION_TOKEN" }}'
```

A value that does not start with `env:` is used as written, so a template
stays unresolved until the request renders. Header templates are the way to
put provider credentials in a header: the control plane resolves them, and
the session never sees them.

### Environment allowlist

A template can call `env` only with a string literal, and only for a
variable the template's owner declares:

- A source template can read the `requiredEnv` and `optionalEnv` of the
  source and of its connection.
- A schedule template (`context`, `title`, `prompt`) can read the
  `requiredEnv` and `optionalEnv` of the schedule.

Startup fails on an `env` call that reads an undeclared variable, or that
passes a computed name such as `{{ env .name }}`. Startup also fails when a
`requiredEnv` variable is unset. An `optionalEnv` variable that is unset
renders as an empty string. Declare every credential in `requiredEnv`, so a
missing key stops startup with the connection name instead of reaching the
provider as an empty header.

Some names are never readable, even when declared:

- every `MAYFLY_*` variable;
- every variable the control-plane config names as a secret:
  `server.token_env`, `llm.api_key_env`, the `env:` headers of `llm`,
  `skills`, and `mcp.servers`, the S3 key variables,
  `queue.redis.password_env`, and each trigger `secret_env`.

The same deny list applies to `env:NAME` connection headers. A bundle
therefore cannot copy the admin token or the model key into a prompt, a
transcript, or a request to another origin.

## Sources

`http.json` is the only type. A source sends one request, or one request per
URL. Every string in `request` and `paginate.body` is a Go template.

### Definition

| Field | Meaning |
| --- | --- |
| `type` | `http.json` |
| `connection` | Name of a visible connection |
| `needs` | Sources that must run first. A source cannot need itself, list a need twice, or need a source it cannot see |
| `request` | One HTTP call |
| `requiredEnv` | Environment variables the source templates read, checked at startup |
| `optionalEnv` | Environment variables the source templates may read when set |
| `paginate` | Continue while the response says more pages exist |
| `fanout` | Run once per URL of the documents from another source |
| `extract` | Item list and fields |
| `filter` | Keep matching items |
| `change` | Read a provider change status and diff |
| `retry` | Attempts and delay |
| `limits` | Item cap for this run |
| `scope` | Visibility |

`paginate` and `fanout` cannot be combined.

### Request

```yaml
request:
  method: POST           # default POST
  path: /scrape
  query:
    limit: "100"
  headers:
    X-Request: "{{ .runID }}"
  body:
    url: "{{ .url }}"
    formats:
      - markdown
      - type: changeTracking
        modes: [git-diff]
        tag: competitor-watch
```

Template data: `.runID`, `.schedule`, `.source`, `.slot`, `.now`, `.cursor`,
and, inside a fanout, `.url`, `.field`, and `.doc` (the parent document's
fields). `.cursor` is empty outside a paginated request.

### Pagination

```yaml
paginate:
  maxPages: 10
  morePath: has_more
  cursorPath: next_cursor
  body:
    start_cursor: "{{ .cursor }}"
```

The first request uses `request.body`. Later requests use `request.body`
merged with `paginate.body`. `morePath` must be true in the response, and
`cursorPath` supplies the next cursor. `maxPages` defaults to 10. If the
response still says more pages exist after `maxPages` requests, the source
fails with an error that names `paginate.maxPages`. It does not return a
silently truncated list. Raise `maxPages` or narrow the query.

### Fanout

```yaml
fanout:
  from: competitors
  urls: [pricing, changelog]
```

`from` must be listed in `needs`. Each name in `urls` must be an extract
field of the parent source. The parent document's fields are copied to
`fields`, and the parent `name` is copied to `name`. The field name becomes
the document `label`.

A URL that is not `http` or `https` becomes an item error, so a `mailto:`
placeholder in the database does not stop the run.

Two targets that render the same request (method, URL, headers, and body)
are fetched once, and every target receives the result. A pricing page and a
changelog that point at the same page therefore produce one request and two
documents with the same status.

A `401` or `403` from the provider fails the whole source at once instead of
producing one item error per URL. A wrong or expired key then shows as one
clear run error. A filtered-out response produces no document.

### Extract

```yaml
extract:
  list: results          # omit for a single document
  fields:
    name:
      path: properties.Name.title[*].plain_text
    pricing:
      paths:
        - properties.Pricing.url
        - properties.pricing.url
      join: ", "
```

A path uses dot segments, `[n]` indexes, and `[*]` wildcards. Segment names
are matched exactly, then with case, spaces, underscores, and hyphens
ignored. `paths` is tried in order and the first match wins. `join` joins an
array result. A path that resolves nothing leaves the field absent.

A wildcard must match at least one item, so `a[*].b` is absent when `a` is
missing.

### Filter

A document is kept when every clause matches.

| Clause | Keeps an item when |
| --- | --- |
| `equals` | Each named field equals the value |
| `notEquals` | No named field equals the value |
| `notTrue` | No named field is the boolean `true`. A missing field passes |
| `notEmpty` | Each named field has a non-blank value |
| `presentAny` | At least one named field is non-blank |

### Change tracking

```yaml
change:
  statusPath: data.changeTracking.changeStatus   # Firecrawl v2 wraps the result in data
  diffPath: data.changeTracking.diff.text
  errorPath: error
  maxLines: 40
  maxLineChars: 300
```

| Status | Result |
| --- | --- |
| `same` | No change |
| `new` | Baseline. Counts as new, starts no session by default |
| `changed` | Material. An empty diff is an item error |
| `removed` | Material. Counts as changed, with or without a diff |

Any other status is an item error. An item error carries the text of
`errorPath`, cut to 300 characters.

`maxLines` defaults to 40 and `maxLineChars` to 300. The full provider diff
is not kept; the artifact and the prompt both carry the truncated form.

### Retry and limits

```yaml
retry:
  attempts: 3
  delay: 2s
limits:
  maxItems: 80
```

A 429, a 5xx, or a transport failure retries. `attempts` defaults to 3 and
includes the first try. The wait before attempt *n* is *n* × `delay`, so
`delay: 2s` waits 2 s, then 4 s. Other HTTP statuses are item errors.

`maxItems` bounds the documents of one run and defaults to 100. For a list
source it counts the items that pass `filter`, so filtered-out rows do not
use up the cap. For a fanout source it counts the requests before they are
sent, as a cost bound.

`sources.timeout` bounds one source step, including every page and every
fanout request. A fanout step that reaches the deadline keeps the documents
that already finished and records the rest as item errors. A run in which
every document failed is an error: it writes no artifact, starts no
session, and retries through the queue.

## Schedules

A schedule is a key of an agent manifest.

```yaml
agents:
  - name: competitor-watch
    systemPrompt: ...
    schedules:
      - name: competitor-watch
        cron: "0 7 * * *"
        source: competitor-pages
        ttl: 30m
        triggerOn: [changed]
        optionalEnv: [WATCH_CONTEXT]
        context: '{{ env "WATCH_CONTEXT" }}'
        title: "Competitor watch {{ .Date }}"
        prompt: |
          {{ range .Changes }}...
```

| Field | Meaning |
| --- | --- |
| `name` | Unique across the bundle |
| `cron` | Five fields. Macros such as `@daily` are rejected |
| `source` | Visible source, and the terminal step of the plan |
| `ttl` | Session time to live. Default 30m, maximum 24h |
| `triggerOn` | `changed` (default) or `new`, or both |
| `context` | Optional template, available to the prompt as `.Context` |
| `requiredEnv` | Environment variables the schedule templates read. An unset variable stops startup |
| `optionalEnv` | Environment variables the schedule templates may read. An unset variable renders empty |
| `title` | Session title. An empty result falls back to `Scheduled run <date>` |
| `prompt` | Required session prompt |

Cron slots are UTC. A `CRON_TZ=` prefix is rejected, because a schedule has no
time zone of its own. The scheduler accepts each slot once, so a restart or a
slow tick cannot start the same slot twice. On first start it records the
current slot and waits for the next one. After a downtime, it catches up the
most recent missed slot, up to seven days back.

### Prompt data

The prompt and title render with:

| Field | Meaning |
| --- | --- |
| `.Date` | Slot date, `2006-01-02` |
| `.Schedule` | Schedule name |
| `.Source` | Terminal source |
| `.RunID` | Job ID, and the artifact name |
| `.Slot` | Slot time, UTC |
| `.Context` | Rendered `context` |
| `.ArtifactKey`, `.ArtifactDigest`, `.ArtifactBytes` | Run artifact |
| `.Totals` | `Pages`, `Changed`, `New`, `Same`, `Failed` |
| `.Changes` | `changed` and `removed` items with `.Name`, `.Label`, `.URL`, `.Status`, `.Diff` |
| `.New` | Baseline items |
| `.Errors` | Item errors with `.Error` |

The control plane prepends a fixed notice to every schedule prompt: the
artifact holds text from external services, and the agent must treat it as
data. A schedule cannot remove the notice.

Each diff is cut to 8 KiB in the prompt, and the change section as a whole to
32 KiB. The run artifact keeps every document with its stored diff, which is
already cut to `maxLines` and `maxLineChars`.

## Run artifacts

Every run writes `runs/<schedule>/<run id>.json` to the artifact store,
whether or not it starts a session:

```json
{
  "run_id": "s-...",
  "schedule": "competitor-watch",
  "source": "competitor-pages",
  "slot": "2026-11-13T07:00:00Z",
  "generated_at": "2026-11-13T07:00:03Z",
  "totals": {"pages": 4, "changed": 1, "new": 0, "same": 3, "failed": 0},
  "documents": [...]
}
```

`sources.max_artifact_bytes` bounds the file. A larger run fails and starts
no session.

The artifact is also the run's checkpoint. When a job is retried, for
example after a full session pool returned `ErrCapacity`, the control plane
reads the stored artifact and starts the session from it instead of calling
the providers again. Change tracking such as Firecrawl's reports a change
only once, so a second fetch would report `same` and lose it. A stored
artifact that does not decode, or that belongs to another run, fails the
job. The session is created with the job ID as its idempotency key, so a
retry after a crash does not start a second session. The session mounts the run artifact, and the agent reads it with
`read_artifact` in windows.

## API

| Request | Result |
| --- | --- |
| `GET /api/v1/schedules` | Every schedule with agent, cron, and terminal source |
| `POST /api/v1/schedules/{name}/run` | `202` with a `job_id`. A full queue returns `503` |
| `GET /api/v1/queue/jobs/{id}` | Job state, last error, and `result.session_id` when a session started |

A manual run always enqueues. Dedupe applies to scheduled slots, keyed by
schedule name and slot.

## Configuration

```yaml
sources:
  state_path: /var/lib/mayfly/schedule.db
  max_artifact_bytes: 16777216
  concurrency: 4
  timeout: 15m
  job_timeout: 30m
```

| Key | Meaning |
| --- | --- |
| `state_path` | bbolt file with the last accepted slot for each schedule. Empty keeps it in memory, which loses catch-up state on restart |
| `max_artifact_bytes` | Cap for one run artifact. Default 16 MiB |
| `concurrency` | Parallel fanout requests per source. Default 1. The shipped configs set 4 |
| `timeout` | Deadline for one source step, every page and fanout request included. Default 15m |
| `job_timeout` | Deadline for one schedule job: every step plus session creation. Must be at least `timeout`. Default 30m. The worker renews the job lease while it runs |

`state_path` must sit on the control-plane PersistentVolume. Provider
credentials reach the control plane through the deployment's environment,
for example an `envFrom` secret reference. They never enter a session pod.

With the `local` runtime backend there is no pod. The agent process starts
with a minimal environment: the shell, locale, TLS certificate, and proxy
variables listed in `passEnv` in `internal/runner/local.go`, plus the
session's own variables. Provider and admin tokens in mayflyd's environment
do not reach it. The local backend
still has no network or filesystem isolation.
