# Configuration

`mayflyd --config <path>` loads one YAML document. Unknown fields are ignored. Durations use Go syntax: `30s`, `15m`, `2h`. Validate before deploy by running the binary once; invalid configuration exits with code 2.

Complete example: [examples/config.local.yaml](../examples/config.local.yaml). Kubernetes example: [deploy/k8s/base/configmap.yaml](../deploy/k8s/base/configmap.yaml).

## server

| Key | Default | Meaning |
|---|---|---|
| `addr` | `:8080` | HTTP listen address |
| `public_url` | `http://localhost:8080` | URL used in session links and WebSocket URLs |
| `internal_url` | `public_url` | URL that session pods use to reach the control plane; use a cluster Service URL in Kubernetes |
| `token_env` | `MAYFLY_API_TOKEN` | Environment variable holding the admin token. If empty, a random token is generated and logged at startup |
| `attach_ticket_ttl` | `60s` | Lifetime of one-time WebSocket attach tickets |
| `max_body_bytes` | `4194304` | Maximum JSON body for session creation |

## sessions

| Key | Default | Meaning |
|---|---|---|
| `namespace` | `mayfly` | Kubernetes namespace for session objects |
| `default_ttl` | `1h` | TTL when a request does not set one |
| `max_ttl` | `4h` | TTL cap |
| `idle_timeout` | `20m` | Runtime closes itself after this period without user messages; `0` disables |
| `close_grace` | `90s` | Time to wait for finalize after close before force stop and fallback |
| `event_buffer` | `5000` | Retained thread events per session in memory |
| `max_steps` | `24` | Maximum model/tool iterations per user message |
| `max_history_messages` | `120` | Messages sent to the model; oldest are trimmed at a user boundary |
| `allow_shell` | `true` | Enables the `run_command` tool inside the container |
| `shell_timeout` | `120s` | Maximum command duration |
| `workspace_dir` | `/workspace` | Container workspace path; overridden for the local backend |
| `artifacts_dir` | `/var/lib/mayfly/artifacts` | Container bundle staging path; overridden for the local backend |

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
| `workspace_limit` | `1Gi` | EmptyDir workspace size |
| `artifact_volume_limit` | `1Gi` | EmptyDir artifact staging size |
| `resources.requests` | `{}` | Container requests, for example `cpu: 100m` |
| `resources.limits` | `{}` | Container limits |
| `env` | `{}` | Literal environment variables added to the session pod. Do not put secrets here; pod specs are readable in the namespace |
| `poll_interval` | `2s` | Pod state polling interval |
| `pod_labels` | `{}` | Extra labels for session objects |

## llm

| Key | Default | Meaning |
|---|---|---|
| `base_url` | — | OpenAI-compatible root, for example `https://api.openai.com/v1` |
| `api_key_env` | — | Control-plane environment variable with the upstream key |
| `model` | — | Default model |
| `allow_models` | `[]` | Additional models that requests may select. An empty list allows only the default |
| `timeout` | `180s` | Per-request model timeout |
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

S3-compatible means AWS Signature Version 4 with a configurable endpoint and path style. This covers MinIO, Cloudflare R2, Backblaze B2, and GCS interoperability mode. The native GCS API and Artifact Registry are not implemented; add an `artifact.Store` implementation for them.

Credentials come from one of two places. Set `access_key_env` and `secret_key_env` to read static keys from the control-plane environment. Leave both empty to use the default AWS credential chain: environment variables, shared config, web identity (`IRSA`), ECS task role, or EC2 instance metadata. That lets the control plane use an IAM role with no static keys. Credentials never reach session pods. Static keys and environment credentials are read at startup; role credentials from IRSA, ECS, or instance metadata are fetched and refreshed by the SDK.

Mayfly asks the SDK for checksums only when an operation requires one. R2, B2, and older MinIO builds reject the default CRC32 checksum headers, so those stores work without extra configuration. Test the store with one export before you rely on it.

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
| `dirs` | Directories of `.yaml`, `.yml`, or `.json` agent manifests. Loaded at startup; hidden directories are skipped |
| `allowed_images` | Globs for images declared by manifests. Empty means any manifest image is allowed |

Agent manifests and selection are documented in [agents.md](agents.md). The built-in `default` agent uses the global model, skill, MCP, and limit settings. Manifest agents can select a named in-image `harness`, and can set a `runtime` image, command, args, env, and harness config.

## skills

| Key | Meaning |
|---|---|
| `dirs` | Directories of Markdown skill files. The control plane reads them at startup |
| `sources` | Pinned tar.gz archives fetched and verified by the control plane at startup |
| `default` | Skill names injected when a request selects none. Empty means every discovered skill |

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

There is no archive cache yet: every control-plane start fetches and verifies the source. The control plane needs outbound access to the archive host. GitHub archive URLs redirect to `codeload.github.com`, and authorization headers are not guaranteed across that host change; use GitOps or a mirror for private repositories if token forwarding matters.

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

HTTP server URLs in session configuration point at the control plane proxy. Session pods never see the upstream URL or its headers.

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
```

| Key | Meaning |
|---|---|
| `name` | Path segment: `/hooks/<name>` |
| `type` | `generic`, `github`, `slack`, `datadog` |
| `secret_env` | Environment variable holding the shared secret or signing secret |
| `allow_unsigned` | Skip verification. Use only for local development |
| `template` | Optional Go `text/template` applied to a `Data` value |
| `ttl`, `model`, `agent`, `skills`, `mcp_servers` | Session defaults for this trigger |

Template data fields: `.Trigger`, `.Type`, `.Event`, `.Delivery`, `.Actor`, `.Agent`, `.Title`, `.Body`, `.URL`, `.Payload`.

Signature behavior:

- `generic`: `X-Mayfly-Signature-256` HMAC-SHA256 hex, optional `sha256=` prefix, or `X-Mayfly-Token`.
- `github`: `X-Hub-Signature-256` only. The static `X-Mayfly-Token` header is not accepted, because GitHub signs each delivery and a static token can be replayed.
- `slack`: `X-Slack-Signature` with a 5-minute timestamp window; also answers `url_verification`.
- `datadog`: `X-Mayfly-Token`.

## Environment variables

| Variable | Used by | Meaning |
|---|---|---|
| `MAYFLY_API_TOKEN` | control plane | Admin bearer token |
| `MAYFLY_CONFIG` | all binaries | Default config path |
| `MAYFLY_LOG_LEVEL` | control plane, runtime | `debug`, `info`, `warn`, `error` |
| `MAYFLY_SESSION_TOKEN` | runtime | Injected per session; do not set manually |
| `KUBECONFIG` | control plane | Used when not running in-cluster |

Trigger, LLM, MCP, and S3 environment variables are named by configuration.
