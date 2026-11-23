# Configuration

`mayflyd --config <path>` loads one YAML document. Unknown fields are rejected at startup, so a misspelled key fails fast instead of falling back to a default. Durations use Go syntax: `30s`, `15m`, `2h`. Validate before deploy by running the binary once; invalid configuration exits with code 2.

Complete example: [examples/config.local.yaml](../examples/config.local.yaml). Kubernetes example: [deploy/k8s/base/configmap.yaml](../deploy/k8s/base/configmap.yaml).

## version

| Key | Default | Meaning |
|---|---|---|
| `version` | `1` | Config file format version. This build reads version 1 |

Set `version: 1` in every file. A file without the key is read as version 1, and `mayflyd` logs a warning at startup. Any other value fails the load with an error that names the rejected version and the supported version, and the process exits with code 2.

## server

| Key | Default | Meaning |
|---|---|---|
| `addr` | `:8080` | HTTP listen address |
| `public_url` | `http://localhost:8080` | URL used in session links and WebSocket URLs |
| `internal_url` | `public_url` | URL that session pods use to reach the control plane; use a cluster Service URL in Kubernetes |
| `token_env` | `MAYFLY_API_TOKEN` | Environment variable holding the single admin token. Used only when `tokens` is empty. If no token resolves, `mayflyd` exits with code 2. The secret must be at least 32 characters |
| `tokens` | `[]` | Named API tokens with explicit scopes. A non-empty list replaces `token_env`. See [security.md](security.md#api-authentication) |
| `tokens[].name` | — | Token name. Unique. The name appears in the audit log, never the secret |
| `tokens[].scopes` | — | At least one of `admin`, `sessions:read`, `sessions:write`, `queue:read`, `queue:write`, `schedules:read`, `schedules:write`. `admin` grants every scope |
| `tokens[].secret_env` | — | Environment variable that holds the token secret. Required and unique per token. A missing variable stops startup. The secret must be at least 32 characters |
| `tokens[].expires_at` | — | Optional RFC 3339 time. A token at or past this time is refused with `401` |
| `trust_forwarded_for` | `false` | Key the failed-authentication limiter on the first `X-Forwarded-For` hop. Set it only behind a proxy that overwrites the header. See [security.md](security.md#api-authentication) |
| `attach_ticket_ttl` | `60s` | Lifetime of one-time WebSocket attach tickets |
| `max_body_bytes` | `4194304` | Maximum JSON body for session creation |
| `webhook_max_body_bytes` | `1048576` | Maximum webhook body when the trigger sets no `max_body_bytes`. Must be positive and at most `16777216` |
| `shutdown_budget` | `25s` | Whole stop sequence after SIGTERM: HTTP drain, queue worker drain, then session manager shutdown. The stages get 20, 40, and 40 percent of the budget in that order, each on its own deadline. Keep the budget below the pod's `terminationGracePeriodSeconds`; the chart sets both. Minimum `3s` |
| `metrics.enabled` | `true` | Serve the Prometheus endpoint. See [observability.md](observability.md) for every metric |
| `metrics.address` | empty | Serve `/metrics` on a listener of its own, such as `:9090`. Empty serves it on `addr`. Must differ from `addr` |
| `metrics.require_auth` | `true` on `addr`, `false` on a separate address | Require a token with the `admin` scope for `/metrics` |

## sessions

| Key | Default | Meaning |
|---|---|---|
| `namespace` | `mayfly` | Kubernetes namespace for session objects |
| `default_ttl` | `1h` | TTL when a request does not set one |
| `max_ttl` | `4h` | TTL cap |
| `idle_timeout` | `20m` | Runtime closes itself after this period without user messages; `0` disables |
| `close_grace` | `90s` | Time to wait for finalize after close before force stop and fallback |
| `event_buffer` | `5000` | Retained thread events per session in memory |
| `event_buffer_bytes` | `8388608` | Retained thread byte budget per session, in addition to `event_buffer`. The oldest events are evicted first. A human message the runtime has not read stays in the inbox even when its thread copy is evicted |
| `max_event_bytes` | `65536` | Cap on one runtime event. Longer text and payload strings are truncated rune-safely and end with `…[truncated]` |
| `max_steps` | `24` | Maximum model/tool iterations per user message |
| `max_history_messages` | `120` | Messages sent to the model; oldest are trimmed at a user boundary |
| `max_concurrent` | `50` | Maximum sessions that have not finalized. Webhook work waits in the queue; direct API calls get `429` and `Retry-After`. `0` removes the cap |
| `retention` | `30m` | How long a finalized session stays in the list for status and events, and how long its record stays in the record store. Artifact metadata is kept separately. `0` keeps sessions forever |
| `connect_timeout` | `2m` | A session whose runtime has not reached the control plane in this time is failed. Measured from launch, including time the pod spends Pending. `0` disables |
| `max_restarts` | `2` | Replacement containers for a non-terminal session whose pod fails or disappears. `0` disables replacement and falls back immediately; on SIGTERM the runtime then finalizes with its bundle instead of waiting for a replacement |
| `persistent_workspace` | `true` | Mount one per-session PVC at the workspace and artifact paths. `false` uses EmptyDirs; the workspace and `conversation.jsonl` would not survive a container, so no replacement is started and the session fails instead. On SIGTERM the runtime then finalizes with its bundle instead of waiting for a replacement |
| `allow_shell` | `false` | Enables the `run_command` tool inside the container |
| `shell_timeout` | `120s` | Maximum command duration |
| `workspace_dir` | `/workspace` | Container workspace path; overridden for the local backend |
| `artifacts_dir` | `/var/lib/mayfly/artifacts` | Container bundle staging path. The Kubernetes backend uses `/artifacts` on the workspace claim; the local backend uses its state directory |
| `records.backend` | `journal.backend` | `memory` or `bbolt`. Where session records and the artifact index are kept. `bbolt` keeps finished sessions in `GET /api/v1/sessions` and artifact downloads working across a restart. `memory` keeps them in the process |
| `records.path` | `sessions.db` beside `journal.bbolt.path` | Record database file, for the `bbolt` backend only. Put it on the control-plane PersistentVolume |

### Session memory

The hub keeps one stream per active session in control-plane memory. The
worst case for one session is `event_buffer_bytes` plus the inbox. The inbox
holds at most `event_buffer` human messages, and one message is at most 32
KiB. With the defaults that is 8 MiB + 5000 × 32 KiB, about 164 MiB. With
`max_concurrent` = 50 the hub bound is about 8 GiB.

The chart's 1 GiB memory limit covers normal load, not this bound. Raise the
pod memory limit when you raise `event_buffer_bytes`, `event_buffer`, or
`max_concurrent`. Lower `event_buffer` to cut the inbox term, because one
human message can be 32 KiB.

## runtime

| Key | Default | Meaning |
|---|---|---|
| `backend` | `kubernetes` | `kubernetes` or `local` |
| `image` | — | Agent image (required for Kubernetes) |
| `image_pull_policy` | `IfNotPresent` | Kubernetes pull policy |
| `image_pull_secrets` | `[]` | Secret names attached to session pods for private images |
| `agent_binary` | — | Agent binary path (required for local) |
| `state_dir` | `.mayfly` | Local backend state and workspace directory |
| `service_account` | `mayfly-agent` | Service account for session pods |
| `platform_namespace` | `sessions.namespace` | Namespace containing the control plane |
| `platform_labels` | `app.kubernetes.io/name: mayflyd` | Pod labels used in the egress rule |
| `network_policy` | `true` | Create a per-session NetworkPolicy |
| `dns_namespace` | `kube-system` | Namespace allowed for DNS egress |
| `egress` | `[]` | Extra egress CIDRs: `[{cidr: 203.0.113.0/24, ports: [443]}]` |
| `workspace_limit` | `1Gi` | Workspace size. With `sessions.persistent_workspace` it sizes the workspace half of the per-session PVC (`mayfly-<id>-workspace`); otherwise it sizes the EmptyDir |
| `artifact_volume_limit` | `1Gi` | Artifact staging size. With persistence, the PVC is sized as `workspace_limit + artifact_volume_limit` because the bundle is staged on the same claim; otherwise it sizes the artifact EmptyDir |
| `plugin_volume_limit` | `512Mi` | Size of the EmptyDir that holds the copied files of all inline plugins in one session pod |
| `storage_class` | `""` | StorageClass for the workspace claim. Empty uses the cluster default; set it on a cluster without a default class, or the claim never binds and the session times out |
| `resources.requests` | `{}` | Container requests, for example `cpu: 100m` |
| `resources.limits` | `{}` | Container limits |
| `env` | `{}` | Literal environment variables added to the session pod. Do not put secrets here; pod specs are readable in the namespace. `MAYFLY_CONFIG` and `MAYFLY_SESSION_TOKEN` are owned by the control plane and are rejected at startup |
| `poll_interval` | `2s` | Pod state polling interval |
| `pod_labels` | `{}` | Extra labels for session objects |

## llm

| Key | Default | Meaning |
|---|---|---|
| `base_url` | — | OpenAI-compatible root, for example `https://api.openai.com/v1` |
| `api_key_env` | — | Control-plane environment variable with the upstream key |
| `model` | — | Default model |
| `allow_models` | `[]` | Additional models that requests may select. An empty list allows only the default |
| `timeout` | `180s` | Time allowed for the upstream connection and response headers. A stalled provider is dropped at this point; streamed bodies continue until the session disconnects |
| `headers` | `{}` | Extra upstream headers |
| `max_body_bytes` | `16777216` | Maximum broker request size |

For multiple providers, point `base_url` at LiteLLM and use its model aliases. See [providers.md](providers.md).

## artifacts

| Key | Default | Meaning |
|---|---|---|
| `store` | `local` | `local` or `s3` |
| `local.dir` | `./artifacts` | Directory for local bundles |
| `s3.endpoint` | AWS S3 | Custom S3-compatible endpoint, for example `http://minio:9000` |
| `s3.region` | — | Signing region |
| `s3.bucket` | — | Bucket name |
| `s3.access_key_env` | — | Environment variable with the access key. Must be set together with `secret_key_env` |
| `s3.secret_key_env` | — | Environment variable with the secret key. Must be set together with `access_key_env` |
| `s3.session_token_env` | — | Optional environment variable with a session token for temporary static credentials |
| `s3.prefix` | — | Object key prefix |
| `s3.path_style` | `false` | Use path-style URLs (MinIO and most local S3 servers) |
| `max_bytes` | `256MiB` | Maximum accepted bundle size |
| `index_retention` | `0` | How long the artifact index keeps a bundle's download metadata. `0` keeps it, because the bundle is the durable output of a session; set a duration to bound the index |

S3-compatible means AWS Signature Version 4 with a configurable endpoint and path style. This covers MinIO, Cloudflare R2, Backblaze B2, and GCS interoperability mode. The native GCS API and Artifact Registry are not implemented; add an `artifact.Store` implementation for them.

Credentials come from one of two places. Set `access_key_env` and `secret_key_env` to read static keys from the control-plane environment. Leave both empty to use the default AWS credential chain: environment variables, shared config, web identity (`IRSA`), ECS task role, or EC2 instance metadata. That lets the control plane use an IAM role with no static keys. Credentials never reach session pods. Static keys and environment credentials are read at startup; role credentials from IRSA, ECS, or instance metadata are fetched and refreshed by the SDK.

Mayfly asks the SDK for checksums only when an operation requires one. R2, B2, and older MinIO builds reject the default CRC32 checksum headers, so those stores work without extra configuration. Test the store with one export before you rely on it.

## queue

Accepted webhook work is stored here before any pod exists, so a slow cluster cannot make a sender time out. `memory` is for development; `bbolt` and `redis` survive a control-plane restart.

```yaml
queue:
  backend: bbolt            # memory, bbolt, redis
  max_jobs: 1000
  max_attempts: 5
  retry_base: 5s
  visibility_timeout: 5m
  dedupe_ttl: 72h
  tombstone_ttl: 1h
  poll_interval: 2s
  bbolt:
    path: /var/lib/mayfly/queue.db
  redis:
    url: redis://valkey.mayfly.svc:6379/0
    password_env: MAYFLY_REDIS_PASSWORD
    key_prefix: mayfly
```

| Key | Default | Meaning |
|---|---|---|
| `backend` | `memory` in config, `bbolt` in the Helm chart | `memory`, `bbolt`, or `redis` |
| `max_jobs` | `1000` | Maximum live (not buried) jobs; further webhooks get `429` |
| `max_attempts` | `5` | Failed attempts before a job is buried |
| `retry_base` | `5s` | First retry delay; doubles per attempt up to ten minutes |
| `visibility_timeout` | `5m` | How long a claimed job stays invisible before it is retried. Minimum `2m` |
| `dedupe_ttl` | `72h` | How long a delivery key is remembered. Keep it at or above the sender's redelivery window (GitHub redelivers for three days). Minimum `1s` |
| `tombstone_ttl` | `1h` | How long a completed job keeps its session link for `GET /api/v1/queue/jobs/{id}`. `0` disables expiry: `memory` keeps records until the process restarts; `bbolt` (`done` bucket) and `redis` (`{<key_prefix>}:done:<job id>` keys) keep them indefinitely; nothing in Mayfly removes them |
| `poll_interval` | `2s` | Worker polling delay when the queue is empty or capacity is full |
| `bbolt.path` | — | Database file. Put it on a PersistentVolume for durability across pod replacement |
| `redis.url` | — | `redis://` or `rediss://` URL. Host, port, user, password, and TLS come from it |
| `redis.password_env` | — | Environment variable that overrides the URL password |
| `redis.username` | — | ACL user name |
| `redis.key_prefix` | `mayfly` | Keys are written as `{<prefix>}:queue`, `:jobs`, `:dead`, `:seen:<id>`, `:done:<id>` |
| `redis.cluster` | `false` | Use the cluster client for Redis Cluster or ElastiCache cluster mode |

Backend requirements:

- `bbolt` locks its file, so only one control-plane replica can use it. Back up the file; it is the only copy.
- `redis` runs two Lua scripts for atomic accept and claim, so the cache must support `EVAL`. Redis, Valkey, KeyDB, Dragonfly, ElastiCache, Memorystore, Azure Managed Redis, and Upstash do.
- `redis` must not evict keys: set `maxmemory-policy noeviction` on a dedicated instance or database, and enable AOF (`appendonly yes`, `everysec`) or snapshots. An `allkeys-lru` cache silently drops accepted jobs.
- Dedupe keys live for `dedupe_ttl`, so size the cache for roughly one key per accepted delivery in that window. Completed jobs keep a tombstone for `tombstone_ttl`; buried jobs stay until retried or deleted.

## journal

The journal records completed turn steps so a replacement session container can finish an interrupted turn without repeating them. `memory` is for tests and single-process development; `bbolt` survives a control-plane restart and is the chart default.

```yaml
journal:
  backend: bbolt            # memory or bbolt
  bbolt:
    path: /var/lib/mayfly/journal.db
  max_entry_bytes: 8388608
  max_session_bytes: 67108864
```

| Key | Default | Meaning |
|---|---|---|
| `backend` | `bbolt` in config and chart | `memory` or `bbolt` |
| `bbolt.path` | `.mayfly/journal.db` in config, `/var/lib/mayfly/journal.db` in the chart | Database file. Put it on the control-plane PersistentVolume; a control-plane restart must be able to read the last turn |
| `max_entry_bytes` | `8388608` (8 MiB) | Largest journal entry a runtime may send. A larger entry gets `413`. A model step can carry a 4 MiB `write_file` argument, so keep this above that |
| `max_session_bytes` | `67108864` (64 MiB) | Journal bytes one session may hold. An append past the quota is rejected |

The journal also rejects a step number of `1 + max_steps × 129` or more within a turn: one user step, then one model step and up to 128 tool calls per round. These bounds keep a compromised or prompt-injected runtime from filling the state volume that the queue and the schedule watermark share.

`bbolt` locks its file, so only one control-plane replica can use it. A session's entries are deleted when it finalizes: the artifact bundle is the durable history. The database also holds a per-session sequence reservation, which keeps a restarted control plane from reusing a turn key, and the tool ledger: one `pending` or `done` record per granted tool call, keyed by `<session>/<turn>/<step>`. The ledger is deleted with the session's journal.

## Persisted formats

Every store the control plane writes carries a format version. The journal, the schedule watermark, the queue, and the session record store each write `format_version: 1` into their own meta bucket or key. A stored source run carries `version: 1` in its JSON.

A queue job is also a versioned JSON envelope. The store format and the job envelope are versioned separately, and the envelope gains optional fields without a new version. See [queue-jobs.md](queue-jobs.md).

On open, a store with no stamp is read as version 1: v0.1.0 wrote the version 1 layout without recording it, so its data loads unchanged. A store that reports a newer version stops the control plane with an error that names the store and both versions. Upgrade `mayflyd` instead of opening it. `1.x` adds a migration case per store when a layout changes.

## plugins

Service plugin workloads and the tool gateway.

```yaml
plugins:
  namespace: ""             # defaults to sessions.namespace
  reconcile_interval: 60s
  prune: true
  audit_path: ""            # optional JSONL file
  max_inflight_calls: 256
```

| Key | Default | Meaning |
|---|---|---|
| `namespace` | `sessions.namespace` | Holds the Deployments, Services, NetworkPolicies, and `mayfly-gateway-<name>` Secrets of service plugins |
| `reconcile_interval` | `60s` | How often service plugin objects are reconciled. Minimum `10s` |
| `prune` | `true` | Delete service objects whose manifest left the bundle |
| `audit_path` | `""` | Append one JSONL record per `call.begin` and `call.done`. Empty keeps the records in the process log only |
| `max_inflight_calls` | `256` | Dispatched tool calls allowed across every session. Over the cap, a call gets `503` and the runtime retries within its budget |

Service plugins use `agents.allowed_images`, `runtime.image_pull_policy`, and `runtime.image_pull_secrets` like session images. Placement, limits, and secrets are in [plugins.md](plugins.md).

## agents

```yaml
agents:
  dirs: [./agents]
  allowed_images:
    - mayfly-agent:*
    - ghcr.io/acme/*
```

| Key | Meaning |
|---|---|
| `dirs` | Directories of `.yaml`, `.yml`, or `.json` bundles. A bundle holds agents, schedules, connections, sources, and plugins. Loaded at startup; hidden directories are skipped |
| `allowed_images` | Globs for images declared by manifests. Empty means any manifest image is allowed |

Agent manifests and selection are documented in [agents.md](agents.md). The built-in `default` agent uses the global model, skill, MCP, and limit settings. Manifest agents can select a named in-image `harness`, and can set a `runtime` image, command, args, env, and harness config.

## sources

```yaml
sources:
  state_path: /var/lib/mayfly/schedule.db
  max_artifact_bytes: 16777216
  concurrency: 4
  timeout: 15m
  job_timeout: 30m
```

| Key | Meaning |
|---|---|
| `state_path` | bbolt file that records the last accepted slot for each schedule. Empty keeps it in memory |
| `max_artifact_bytes` | Cap for one run artifact. Default 16 MiB |
| `concurrency` | Parallel fanout requests per source. Default `4`. Must be positive |
| `timeout` | Deadline for one source step, every page and fanout request included. Default `15m` |
| `job_timeout` | Deadline for one schedule job: every source step plus session creation. Default `30m`. Must be at least `timeout` |

Other queue jobs, such as webhook deliveries, have a 5 minute deadline. The worker renews a job's lease every third of `queue.visibility_timeout` while the job runs, so a job longer than the visibility timeout is not handed to a second worker.

Set `state_path` on the control plane's PersistentVolume. Without it a restart loses the slot watermark and the catch-up window. Connection headers and request templates read provider credentials from the control-plane environment, for example through `envFrom` on the Deployment. A template can read only the variables its connection, source, or schedule declares in `requiredEnv` or `optionalEnv`. `MAYFLY_*` variables and every secret this file names (`server.token_env`, `llm.api_key_env`, `env:` headers, S3 keys, the Redis password, trigger secrets) are never readable. See [sources.md](sources.md).

## skills

| Key | Meaning |
|---|---|
| `dirs` | Directories of Markdown skill files. The control plane reads them at startup |
| `sources` | Pinned tar.gz archives fetched and verified by the control plane at startup |
| `default` | Skill names injected when a request selects none. Empty means every discovered skill |
| `cache_dir` | Directory that holds verified source archives. Empty derives `<journal directory>/skill-cache` when the journal backend keeps a file |

Remote skill source, using a repository link:

```yaml
skills:
  sources:
    - name: acme-skills
      repo: https://github.com/acme/skills
      ref: <commit-sha>
      sha256: "<archive-sha256>"
      subdir: skills
      strip_components: 1
      headers:
        Authorization: "env:GITHUB_TOKEN"
      timeout: 30s
```

Or set the archive URL directly:

```yaml
    - name: acme-skills
      url: https://github.com/acme/skills/archive/<commit-sha>.tar.gz
      sha256: "<archive-sha256>"
```

`repo` supports `github.com`, `gitlab.com`, and `bitbucket.org`; other hosts must use `url`. `ref` is required with `repo`; use a commit SHA, not a branch. `repo` and `url` are mutually exclusive. The control plane still downloads a tar.gz archive; the repo fields are URL sugar, not a git client.

`strip_components` defaults to `1`, which removes the `repo-ref/` directory in GitHub and GitLab archives. `subdir` is matched after stripping. The archive must be a `.tar.gz`; entries other than regular files are ignored, and only `.md`/`.markdown` files under `subdir` are loaded. The SHA-256 is mandatory and checked before extraction.

Sources and local directories are combined. Duplicate skill names across either are a startup error. Fetching happens at control-plane startup; sessions never contact the code host. Pin the URL to a commit SHA and the archive SHA-256, not a branch.

Each verified archive is stored in `cache_dir` under its SHA-256, so a restart reads the file and makes no network request. A restart needs the archive host only when no cache entry exists, or when a cache entry fails its digest check. A failed cache write logs a warning and does not stop startup. Only files named `<sha256>.tar.gz` are read or deleted, and no request headers are written to the directory. The cache is safe because every entry carries the digest the configuration pins. Entries for digests that no configured source names are deleted at startup.

GitHub archive URLs redirect to `codeload.github.com`. Authorization headers are not guaranteed across that host change. Use GitOps or a mirror for private repositories if token forwarding matters.

## mcp

```yaml
mcp:
  servers:
    - name: github
      transport: http          # http or stdio
      url: https://mcp.example.com/mcp
      headers:
        Authorization: "env:GITHUB_MCP_TOKEN"
      opt_in: false            # false: available by default
      init_timeout: 30s
```

| Key | Meaning |
|---|---|
| `name` | Stable server name; used in tool prefixes and per-session selection |
| `transport` | `http` (proxied) or `stdio` (process in the container) |
| `url` | Upstream HTTP MCP URL. Only the control plane connects to it |
| `headers` | Upstream headers. `env:NAME` resolves from the control-plane environment |
| `command`, `args`, `env` | stdio process definition. `env` values must be literal; `env:` references are rejected |
| `opt_in` | If true, the server is used only when a request selects it |
| `init_timeout` | MCP initialize and tools/list timeout |
| `tools` | Allowlist of model-visible tool names. Empty offers every tool the server reports |
| `idempotent_tools` | Tools that are safe to repeat after a container restart. Empty means every tool is non-idempotent. Must be a subset of `tools` when `tools` is set |

HTTP server URLs in session configuration point at the control plane proxy. Session pods never see the upstream URL or its headers.

With the built-in harness, every `mcp_*` tool call goes through the tool gateway, which records it in the ledger and writes the call audit record. `* /internal/v1/mcp/{server}` remains for alternate harnesses. See [mcp-and-skills.md](mcp-and-skills.md#tool-calls-through-the-gateway).

## triggers

```yaml
triggers:
  - name: github
    type: github
    secret_env: GITHUB_WEBHOOK_SECRET
    agent: triage
    template: |
      GitHub {{.Event}} by {{.Actor}}: {{.Body}}
    ttl: 30m
    skills: [incident-triage]
    mcp_servers: [github]
    max_body_bytes: 262144
    rate_limit:
      requests: 30
      interval: 1m
      burst: 10
```

| Key | Meaning |
|---|---|
| `name` | Path segment: `/hooks/<name>` |
| `type` | `generic`, `github`, `slack`, `datadog` |
| `secret_env` | Environment variable holding the shared secret or signing secret |
| `allow_unsigned` | Skip verification. Use only for local development |
| `template` | Optional Go `text/template` applied to a `Data` value |
| `ttl`, `model`, `agent`, `skills`, `mcp_servers` | Session defaults for this trigger |
| `max_body_bytes` | Webhook body limit for this trigger. `0` or an omitted key uses `server.webhook_max_body_bytes`. Must be at most `16777216` |
| `rate_limit.requests` | Sessions admitted per `interval`. Default `30`. `0` disables the limit |
| `rate_limit.interval` | Refill period. Default `1m` |
| `rate_limit.burst` | Requests admitted at once before the rate applies. Default `10` when `rate_limit` is omitted; a `rate_limit` block without `burst` uses `requests` |

A body over the limit returns `413` with `code: payload_too_large`. The control plane checks the limit before signature verification, so an oversize delivery does not spend a rate-limit token.

A trigger over its limit gets `429` with `Retry-After`. Only requests that pass signature verification spend a token, so unsigned traffic cannot shut a trigger. The limit is per trigger and per control-plane process; `queue.max_jobs` and `sessions.max_concurrent` remain the last lines of defense across all creation paths.

Template data fields: `.Trigger`, `.Type`, `.Event`, `.Delivery`, `.Actor`, `.Agent`, `.Title`, `.Body`, `.URL`, `.Payload`.

Signature behavior:

- `generic`: an HMAC-SHA256 hex signature of the body in `X-Mayfly-Signature-256` or `X-Hub-Signature-256` (optional `sha256=` prefix), or the shared secret in `X-Mayfly-Token`.
- `github`: the same HMAC-SHA256 signature in `X-Hub-Signature-256`, which GitHub sends, or in `X-Mayfly-Signature-256`, which a relay or the load-test harness can send. Both headers carry the same signature over the same body and secret. The static `X-Mayfly-Token` header is not accepted, because GitHub signs each delivery and a static token can be replayed.
- `slack`: `X-Slack-Signature` with a 5-minute timestamp window; also answers `url_verification`.
- `datadog`: same as `generic`.

## Environment variables

| Variable | Used by | Meaning |
|---|---|---|
| `MAYFLY_API_TOKEN` | control plane, CLI | Default `server.token_env`: one admin-scoped token named `default`. The CLI sends it as the bearer token |
| `MAYFLY_URL` | CLI | Control-plane URL. Default `http://localhost:8080` |
| `MAYFLY_CONFIG` | control plane, runtime, harness | Default config path: `config.yaml` for `mayflyd`, the session config for the runtime |
| `MAYFLY_LOG_LEVEL` | control plane, runtime | `debug`, `info`, `warn`, `error` |
| `MAYFLY_TOKEN` | CLI | Deprecated alias for `MAYFLY_API_TOKEN`. The CLI prints one warning when it uses the alias |
| `MAYFLY_SESSION_TOKEN` | runtime | Injected per session; do not set manually |
| `KUBECONFIG` | control plane | Used when not running in-cluster |

Trigger, LLM, MCP, and S3 environment variables are named by configuration.
