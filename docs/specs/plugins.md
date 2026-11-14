# Plugins: implementation spec

Status: approved design (v2: placements and tool gateway), not started
Audience: Mayfly maintainers and the coding agents that build it
User docs: [../plugins.md](../plugins.md) describes the finished behavior.
Every work package below must leave that page true for the parts it ships.
Base: commit `7aa80cc`

## Summary

A plugin is an MCP server declared in the project bundle and shipped as an
image. Its manifest picks a `placement`:

- `inline`: a stdio child of the session runtime, copied into the pod by an
  init container (the v1 design).
- `sidecar`: a native sidecar container in the session pod, reached over
  streamable HTTP MCP on a Unix socket.
- `service`: a Deployment and Service in the cluster, reconciled by `mayflyd`
  from the manifest and shared by sessions, reached over streamable HTTP MCP.

Every plugin call, whatever the placement, goes through a new tool gateway in
`mayflyd` (`internal/gateway`). The gateway holds each session's tool grant,
records every call in a durable ledger keyed by `<session>/<turn>/<step>`,
attaches the idempotency key, enforces timeouts and rate limits, writes the
audit log, and injects credentials. For inline and sidecar plugins the
runtime makes the call after the gateway authorizes it and reports the result
back. For service plugins the gateway makes the call itself.

`mcp.servers` tools move behind the same gateway for the built-in harness:
HTTP servers are called by the gateway through the existing broker's HTTP
client and resolved headers; stdio servers use the same begin/complete path as
inline plugins. That closes gap 8.2 (Gap 3) of
[architecture-gaps.md](../architecture-gaps.md) for every tool that is not a
built-in, including the competitor-watch Slack post.

The plugin process itself is always a plain MCP server. Sidecar and service
placements put a Mayfly shim (`mayfly-plugin-shim serve`) in front of a stdio
server, so one image works in all three placements.

Build order: gateway plus inline (phase A), then service (phase B), then
sidecar (phase C).

## Goals

- Custom tools in any language with no Mayfly SDK: any stdio MCP server works
  in every placement.
- One manifest format, one protocol (MCP), three placements, the same tool
  names (`plugin_<plugin>_<tool>`).
- One choke point for every non-built-in tool call: grant check, ledger,
  idempotency key, timeouts, rate limits, audit, credential injection.
- Exactly-once replay of completed calls across replacement containers, and
  no repeat of an interrupted non-idempotent call.
- Sidecar and service secrets never enter the agent container.
- Services get their own resources, scaling, state, and NetworkPolicy.
- Declared in the bundle, validated at startup and by `mayflyd -check`.
- `mayfly plugin init|check|test` with no cluster.

## Non-goals

- Running untrusted third-party plugin code. Plugins are trusted operator
  code. Not designed here; listed under [Future options](#future-options).
- Multi-architecture handling. Images must match the node architecture.
- A plugin SDK or a Mayfly-specific wire format to plugins.
- Hostname-based egress allowlists (NetworkPolicy is CIDR-based).
- Plugins in alternate harnesses (`harness: pi` and others). They keep using
  the broker's byte proxy for `mcp.servers` and get no plugins.
- Per-agent tool subsets (open question 3).
- Registry, hot reload, installing packages at session start.
- MCP resources, prompts, sampling, roots, or server-initiated requests.
- Autoscaling services (open question 11).

## Decisions

### One gateway, two dispatch modes

The gateway is a package inside `mayflyd`, wired into `internal/httpapi` next
to `internal/broker`. It never runs plugin code.

| Mode | Used for | Flow |
|---|---|---|
| Local dispatch | `inline`, `sidecar`, stdio `mcp.servers` | Runtime `POST .../tools/calls` (begin) → gateway checks grant, rate limit, ledger; returns `dispatch` with key, timeout, and a call token for sidecars → runtime calls the server in the pod → runtime `POST .../tools/calls/complete` with the result |
| Remote dispatch | `service`, HTTP `mcp.servers` | Runtime `POST .../tools/calls` → gateway checks, records pending, calls upstream over streamable HTTP MCP, records the result, returns `result` |

Why local dispatch for inline and sidecar: the process lives in the pod, and
routing arguments and results through `mayflyd` would add a network round trip
and put workspace-sized payloads on the control plane for no security gain.
The gateway still owns the decision (it can refuse), the key, the timeout, the
rate limit, the audit record, and the stored result.

Why remote dispatch for service: the service's NetworkPolicy admits only
`mayflyd`, so session pods cannot reach it at all, and the gateway is the only
place that can attach the service call token without exposing it to the
agent.

The agent-to-gateway credential is the existing session token
(`MAYFLY_SESSION_TOKEN`), authenticated with `Manager.SessionIDForToken` like
every `/internal/v1/` route. Its scope is the session's grant table (below),
computed at create and rebuilt from `session.json` on adoption. No second
token is minted for that hop: the agent container already holds the session
token, so a second credential there would add nothing.

### Ledger replaces the v1 runtime-side pending entry

v1 wrote a pending journal entry from the runtime before a non-idempotent
call. v2 moves the record to the gateway: the ledger record is written at
begin and completed with the result. On resume the runtime asks the gateway
again with the same key:

| Ledger state at begin | Tool | Gateway answer |
|---|---|---|
| none | any | Record `pending`, return `dispatch` (local) or call upstream (remote) |
| `done` | any | Return the recorded `result` with `replayed: true`. Not called again |
| `pending` | non-idempotent | Return an error `result` with `interrupted: true`; mark the record `done` with that result; the runtime emits the system event |
| `pending` | idempotent | Return `dispatch` (or call upstream) again with the same key; the record stays `pending` |

The `done` row is new relative to v1: a call that completed but whose journal
step never landed now replays its recorded result instead of running again.
`internal/agent/steps.go` changes only for the `Interrupted` and `Replayed`
flags on `toolStepOutput`; the runtime no longer writes pending entries.
Journal steps are still appended after the tool, exactly as today.

The idempotency key is `<session>/<turn>/<step>`. The gateway checks that
`turn` is at most the session's reserved sequence
(`journal.Sequencer.HighWater`) and that `step` is positive and within the
runtime's step bound for a turn (WP3 reads the real bound from
`internal/agent`; `sessions.max_steps` bounds model calls, and each model
step can be followed by tool steps). The same key with a different tool or
argument hash is rejected with 409, because it means the runtime is no longer
deterministic for that step.

### Inline-to-gateway path

1. Runtime calls `POST /internal/v1/sessions/{id}/tools/calls` with
   `{turn, step, call_id, tool, arguments}`. Arguments are sent so the gateway
   can hash them; they are not stored.
2. The gateway returns `dispatch: {key, timeout_ms}`.
3. The runtime calls `pluginhost.Host.Call` with the key, under the timeout.
4. The runtime calls `POST .../tools/calls/complete` with
   `{key, content, is_error}`. The gateway stores the content (capped at
   64 KiB, the tool output cap) and writes the audit record.
5. The runtime appends the journal step as today.

A crash between 2 and 4 leaves the ledger `pending`, which resume handles per
the table above. A crash between 4 and 5 leaves it `done`, which replays.

If the gateway is unreachable, the runtime retries begin and complete with a
5-minute budget, like model calls, then exits non-zero so the replacement
container takes over. It never calls a tool without a successful begin.

### Sidecar transport: streamable HTTP over a Unix socket

| Option | Reach | Verdict |
|---|---|---|
| Localhost TCP | Every container in the pod, including `run_command` in the agent container | Rejected as default: any process in the pod can connect |
| Unix socket on a shared emptyDir | Only containers that mount the volume | Chosen |
| Stdio across containers | Not possible without a bridge process | Rejected |

Chosen: streamable HTTP MCP (the HTTP transport that `internal/mcp/http.go`
already speaks) over a Unix socket at
`/run/mayfly/plugin-sockets/<name>/<name>.sock`, on one emptyDir named
`plugin-sockets-<name>` per sidecar, mounted into the agent container and that
sidecar only. HTTP over a socket gives request framing, status codes, and an
existing client; a socket file needs no port and no NetworkPolicy change.

A per-plugin volume is part of the isolation: with one shared, writable volume
any sidecar could unlink another plugin's socket and listen in its place, then
receive that plugin's call tokens and arguments. The socket is still not access
control by itself, because `run_command` runs in the agent container, which
mounts every socket directory. Every sidecar call therefore carries a call
token signed with that plugin's key, and the shim rejects calls without a valid
one. `initialize` and `tools/list` use a discovery token (gateway-signed,
`tool: "*"`, no key) so the runtime can verify tools at start.

The plugin process stays stdio. `mayfly-plugin-shim serve` is the sidecar's
command: it supervises the stdio child, serves streamable HTTP MCP on the
socket, verifies call tokens, and forwards JSON-RPC to the child. The shim is
copied into the sidecar by the `mayfly-tools` init container, so plugin
images need nothing Mayfly-specific beyond `/mayfly-plugin/`.

The sidecar is a native sidecar (an init container with
`restartPolicy: Always`, available in the Kubernetes 1.35 Mayfly pins), so it
is ready before the agent container starts and stops after it. A
`startupProbe` runs `mayfly-plugin-shim probe --listen unix:...`.

### Call tokens

Format: `v1.<base64url(claims JSON)>.<base64url(HMAC-SHA256(key, "v1." + claims))>`.

Claims: `{"sid": session, "plugin": name, "tool": remote tool name or "*",
"key": idempotency key or "", "args_sha256": hex or "", "exp": unix seconds}`.

- Sidecar: one key per plugin and session, derived as
  `HMAC(sessionKey, "mayfly/sidecar/" + plugin)` from the per-session random
  32-byte value. It is stored in the existing session Secret
  (`SessionSpec.Secret`, today only `token`) as item `gateway-key-<plugin>`,
  and only that item is mounted, as a file, into that plugin's sidecar
  container. Neither the agent container nor another sidecar mounts it, so a
  token minted for plugin B fails the signature check at plugin A. `mayflyd`
  derives the keys from the session key it keeps in memory and from the Secret
  on adoption (generalize `Client.SessionToken` into `SessionKeys`).
- Service: the key is per plugin, in Secret `mayfly-gateway-<name>` in
  `plugins.namespace`, created by the reconciler. The name deliberately does
  not use the `mayfly-plugin-` prefix, so a bundle cannot reference it as a
  plugin secret.
- `exp` is the call timeout plus 30 seconds. The shim rejects an expired
  token, a token whose `plugin` is not the plugin it serves, a token whose
  `tool` or `args_sha256` does not match the request,
  and a second use of the same `key` while the first is in flight. It keeps
  the last 1024 completed keys with their result until `exp`, and returns
  the cached result for a replay, so a retry after a dropped response does
  not re-run the tool.

The plugin never sees the token; it sees only `_meta`.

### Service: created by `mayflyd`, authenticated by NetworkPolicy and call tokens

`mayflyd` reconciles, for each `placement: service` manifest without
`service.mcpServer`, in `plugins.namespace` (default `sessions.namespace`):

| Object | Name | Content |
|---|---|---|
| Secret | `mayfly-gateway-<name>` | `key`: 32 random bytes. Created once, not rotated by reconcile (open question 8). A Secret that is deleted and recreated changes the key hash the Deployment carries, so the pods roll |
| Deployment | `mayfly-plugin-<name>` | Init container `mayfly-tools` (copies the shim); container `plugin` from `image`, command `/mayfly-tools/mayfly-plugin-shim serve --listen tcp::8080 --key-file /var/run/mayfly/gateway/key --max-concurrency <n> -- <command> <args>`; env from `env` plus `secretKeyRef` per secret; hardened security context with `fsGroup` set to the pod UID and Secret volume mode 0440, so the non-root container can read its key; `resources`; `replicas`; startup and readiness probes via `shim probe`; annotation `mayfly.io/gateway-key-hash` with the SHA-256 of the key |
| Service | `mayfly-plugin-<name>` | ClusterIP, port 8080 |
| NetworkPolicy | `mayfly-plugin-<name>` | Ingress only from pods matching `runtime.platform_labels` in `runtime.platform_namespace`, on 8080; egress to DNS (`runtime.dns_namespace`) and `service.egress` |

With `service.transport: http`, the container runs `command` directly (no
shim), listens on `service.port` and `service.path`, and gets the gateway key
as `MAYFLY_GATEWAY_KEY`. It may verify the `Authorization: Bearer <call
token>` header; NetworkPolicy is the required control in that mode, the token
check is optional.

Labels: `mayfly.io/managed=true`, `app.kubernetes.io/name=mayfly-plugin`,
`mayfly.io/plugin=<name>`; annotation `mayfly.io/spec-hash`. The app label
differs from session objects' (`k8s.AppName`), so `k8s.Client.GarbageCollect`
and `OrphanSessions`, which select on `managedSelector`, never match plugin
objects. Reconcile runs at startup and every `plugins.reconcile_interval`
(default 60s): create missing, update on hash change, delete objects whose
plugin left the bundle when `plugins.prune` is true (default). Reconcile
errors are logged; at startup they fail `/readyz` only for plugins that some
loaded agent selects with `required: true`.

Per-session calls: the gateway keeps one upstream MCP client per (agent
session, plugin), opened lazily, closed on finalize. Every `tools/call`
carries `_meta` with `mayfly.io/idempotency-key`, `mayfly.io/session`, and
`mayfly.io/agent`, and the call token in `Authorization`. The service keys
per-session state on `mayfly.io/session`; the shim multiplexes concurrent
calls from many sessions onto its one stdio child by rewriting JSON-RPC ids,
up to `service.concurrency`.

`service.mcpServer: <name>` references an `mcp.servers` entry with
`transport: http`: nothing is reconciled; the gateway calls that server with
its `ResolvedHeaders`. This puts `plugin_` names, a tool allowlist, and
idempotency flags on an existing remote server without moving its
credentials out of `config.yaml`.

RBAC: the controller Role gains `apps/deployments` and `services`
(create/get/list/update/delete) and `update` on `secrets` and
`networkpolicies`. When `plugins.namespace` differs from
`sessions.namespace`, the same Role is needed there (Helm renders it when
`plugins.namespace` is set).

### Broker relationship

The broker (`internal/broker`) stays the single holder of upstream
credentials and of the HTTP transport policy. The gateway is an MCP-aware
layer on top of it:

- `(*Broker).HTTPClient() *http.Client` (new accessor) returns the broker's
  client with its dial, TLS, and response-header timeouts. The gateway's
  upstream MCP clients use it through a new `mcp.NewHTTPWithClient`. `mayflyd`
  builds the broker once and passes the same client to the gateway and to the
  HTTP API, instead of building a second client inside `httpapi.New`.
- The upstream clients set no `http.Client.Timeout`. A client-level timeout
  hard-kills any call that outlives it, and manifests allow up to 15 minutes.
  Each call is bounded by its own context deadline instead; the transport's
  `ResponseHeaderTimeout`, dial, and TLS timeouts stay.
- HTTP `mcp.servers` credentials stay in `config.MCPServerConfig.ResolvedHeaders`,
  resolved once by `config.(*Config).resolveSecrets`. The gateway reads them
  and resolves nothing itself.
- `ServeMCP` and `/internal/v1/mcp/{server}` stay for alternate harnesses,
  which bypass the ledger. The built-in runtime stops using them; its HTTP
  `mcp.servers` tools go to `/internal/v1/sessions/{id}/tools/calls`.
- `ServeLLM` is unchanged.

No upstream credential is duplicated: service plugins use the reconciler's
gateway key (a Mayfly-owned secret), and `service.mcpServer` reuses the
`mcp.servers` headers.

### `mcp.servers` behind the gateway

- Tool names stay `mcp_<server>_<tool>`.
- HTTP servers: remote dispatch. Discovery moves to the gateway, which calls
  `tools/list` once per session per server (open question 9). The runtime does
  not start an HTTP server or connect to the proxy; the gateway adds the
  discovered tools to the grant, with their descriptions and schemas. When
  discovery fails the tools are absent for that session, and the runtime
  refuses an `mcp_*` name it neither holds as a local stdio tool nor received
  from the grant, so a call cannot fall back to the proxy and skip the ledger.
- Stdio servers: local dispatch; begin and complete wrap `mcp.Manager.Call`.
- New optional keys on `mcp.servers[]`: `tools` (allowlist; empty means all,
  today's behavior) and `idempotent_tools` (default empty: every tool is
  non-idempotent). Behavior change for resumed sessions only: today an
  in-flight `mcp_*` call is re-run on resume; after this change it is not,
  unless listed in `idempotent_tools` (open question 4).
- A stdio server no longer inherits `os.Environ()` (open question 12). It gets
  `sessioncfg.StdioEnv`: `PATH`, `HOME`, `TMPDIR`, `LANG`, `TZ`,
  `SSL_CERT_FILE`, `SSL_CERT_DIR`, the proxy variables in both cases, and the
  entry's `env`. `MAYFLY_SESSION_TOKEN`, `MAYFLY_CONFIG`, provider keys, and
  webhook secrets stay in the runtime. The same allowlist builder feeds the
  inline plugin environment (`sessioncfg.PassthroughEnv`), so the two cannot
  drift apart.

### Packaging (inline): init container copy

Unchanged from v1. Each inline plugin adds one init container that runs the
plugin image with Mayfly's static shim as its command and copies
`/mayfly-plugin/` into an emptyDir the agent container mounts read-only at
`/opt/mayfly/plugins/<name>/`. Reasons: stdio needs a child process; it works
on every supported Kubernetes and container runtime, including the pinned k3d
image (`rancher/k3s:v1.35.5-k3s1`); the image's entrypoint never runs, so
images can be `FROM scratch`; replacement containers get the same init
containers because `Manager.prepare` builds the pod from `session.json`.

Image volumes (`volumes[].image`, beta and on by default in 1.35, stable in
1.36, containerd 2.1 or later) can replace the copy later as
`runtime.plugin_packaging: image-volume` without changing plugin images
(open question 10).

### The shim (replaces the v1 copier)

`cmd/mayfly-plugin-shim`, `CGO_ENABLED=0`, installed in the agent image at
`/usr/local/bin/mayfly-plugin-shim`:

| Subcommand | Used by | Behavior |
|---|---|---|
| `install-self <dst>` | `mayfly-tools` init container | Copies its own executable to `dst`, mode 0755 |
| `copy <src> <dst>` | `plugin-<name>` init containers (inline) | The v1 copier: copies the tree, keeps modes minus group/other write, refuses escaping symlinks (checked lexically before the copy, and resolved with `filepath.EvalSymlinks` inside the destination afterwards, because a chain through another link can escape) and device files, fails with `plugin image has no /mayfly-plugin directory`, 1 GiB backstop |
| `serve --listen unix:PATH\|tcp:ADDR --key-file F [--session S] [--max-concurrency N] [--init-timeout D] -- CMD ARGS...` | Sidecar and service containers, local service children | Supervises the stdio child, serves streamable HTTP MCP on `POST /mcp`, verifies call tokens, multiplexes, restarts the child with back-off |
| `probe --listen ...` | Startup and readiness probes | Exits 0 when `serve` has initialized the child and listed its tools |

Pods with any inline or sidecar plugin get `mayfly-tools` (image
`runtime.image`, never the per-agent override, so it always has the shim) and
one `plugin-<name>` copy container per inline plugin.

### Naming

Tools: `plugin_<sanitize(plugin)>_<sanitize(tool)>`, matching
`mcp_<server>_<tool>` from `internal/mcp/manager.go` (`toolName`,
`sanitize`). Collisions and names over 64 characters are load errors, not
numeric suffixes, because the names are declared. Names do not depend on
placement, so moving a plugin between placements does not change prompts,
transcripts, or journal step names.

Docs: "plugin" means this feature. `docs/agents.md` calls agent manifests
"agent plugins" and says "This is the only plugin surface". WP15 renames that
wording to "agent manifest".

### Rules kept from v1

- **Loader rule:** `plugins/<name>/plugin.yaml` is a manifest; every other
  file under `plugins/<name>/` is skipped as plugin source.
- **Flat manifests:** `<name>.plugin.yaml` anywhere in the bundle, for
  ConfigMap bundles whose keys cannot contain `/`.
- **`allowShell` rule:** an agent with `allowShell` true cannot select an
  `inline` plugin that declares `secrets`. It no longer applies to sidecar or
  service plugins, whose secrets are outside the agent container. On the
  local backend sidecars run inline, so the rule applies to them there.
- **Secret prefix:** `secrets[].secret` must start with `mayfly-plugin-`.
- **`required` semantics** as in v1.

## Architecture

```text
bundle dir                 mayflyd                                              cluster
----------                 -------                                              -------
plugins/<n>/plugin.yaml ─▶ project.Load ─▶ Bundle.Plugins
agents.yaml (plugins) ───▶ agents.Definition.Plugins
                           session.loadAgents (visibility, harness, shell rule, images)
                           pluginsvc reconciler (service) ───────────────────▶ mayfly-plugin-<n>:
                                                                                 Deployment, Service,
                                                                                 NetworkPolicy, Secret
                           session.Create ─▶ sessioncfg.Config.Plugins
                                          ─▶ gateway.Register(Grant)
                           Manager.prepare ─▶ k8s.SessionSpec.Plugins ───────▶ session pod:
                                                                                 init: mayfly-tools (shim)
                                                                                 init: plugin-<n> (copy, inline)
                                                                                 sidecar: plugin-<n> (shim serve, UDS)
                                                                                 agent: pluginhost (inline stdio,
                                                                                        sidecar UDS client)
           runtime ──▶ /internal/v1/sessions/{id}/tools[/calls[/complete]]
                           gateway: grant, rate limit, ledger, audit, tokens
                             ├─ local dispatch: answer the runtime, record the result
                             └─ remote dispatch ─▶ broker HTTP client ─▶ service or mcp.servers (http)
```

### Files

New:

| Path | Purpose | WP |
|---|---|---|
| `internal/plugin/manifest.go` | `Manifest` types, `Parse`, `Validate`, defaults | WP1 |
| `internal/plugin/names.go` | `ToolName`, `IsManifestPath`, `ContainerName`, `ObjectName` | WP1 |
| `internal/plugin/manifest_test.go` | Table tests | WP1 |
| `internal/journal/ledger.go` | `Ledger` interface and `Call` record | WP3 |
| `internal/journal/ledger_test.go` | Conformance tests for memory and bbolt | WP3 |
| `internal/gateway/gateway.go` | `Gateway`, `Grant`, `GrantFor`, begin and complete, resume table | WP3, WP9, WP13 |
| `internal/gateway/api.go` | Request and response types shared with `platformclient` | WP3 |
| `internal/gateway/limits.go` | Per-session, per-plugin rate limiters; in-flight cap | WP3 |
| `internal/gateway/audit.go` | slog audit line and optional JSONL file | WP3 |
| `internal/gateway/token.go` | Call token sign and verify (used by the shim too) | WP3 |
| `internal/gateway/gateway_test.go` | Unit tests | WP3 |
| `internal/gateway/remote.go` | Upstream MCP clients per (session, server), discovery | WP9 |
| `internal/gateway/remote_test.go` | Against an in-process streamable HTTP MCP server | WP9 |
| `internal/pluginhost/host.go` | Inline supervisor: start, verify, call, restart, close | WP6 |
| `internal/pluginhost/env.go` | Process environment builder, on the shared `sessioncfg` allowlist | WP6 |
| `internal/sessioncfg/env.go` | Child environment allowlist: `StdioEnv`, `PassthroughEnv`, `DefaultPath` | WP6 |
| `internal/pluginhost/stderr.go` | Capped stderr sink | WP6 |
| `internal/pluginhost/host_test.go` | Tests against a fixture plugin | WP6 |
| `internal/pluginhost/testdata/fixture/main.go` | Go fixture MCP server (build tag `ignore`) | WP6 |
| `internal/pluginhost/sidecar.go`, `sidecar_test.go` | Sidecar client over UDS with tokens | WP13 |
| `internal/pluginsvc/reconcile.go` | Service object rendering and reconcile loop | WP12 |
| `internal/pluginsvc/local.go` | Local-backend service children | WP12 |
| `internal/pluginsvc/reconcile_test.go` | Fake clientset tests | WP12 |
| `cmd/mayfly-plugin-shim/main.go`, `main_test.go` | `install-self`, `copy` | WP5 |
| `cmd/mayfly-plugin-shim/serve.go`, `serve_test.go` | `serve`, `probe` | WP11 |
| `cmd/mayfly/plugin.go`, `plugin_test.go` | `mayfly plugin init\|check\|test` | WP10 |
| `cmd/mayfly/plugintemplates/{go,python,ts}/` | Embedded scaffolds | WP10 |
| `examples/plugins/notify/` | Go, sidecar, non-idempotent, uses the key | WP10, WP13 |
| `examples/plugins/tickets/` | Python, service, shared cache | WP10, WP12 |
| `examples/plugins/word-count/` | TypeScript, inline, no secrets | WP10 |
| `test/integration/plugin_test.go` | Local-backend tests, SIGKILL resume | WP8 |
| `e2e/plugin_test.go`, `e2e/plugins/` | k3d tests and fixture image source | WP14 |

Modified:

| Path | Change | WP |
|---|---|---|
| `internal/sessioncfg/config.go` | `Plugin`, `PluginTool`, `PluginSecret`, `PluginSidecar`, `Config.Plugins`, `MCPServer.Tools`/`IdempotentTools`, validation | WP1 |
| `internal/project/project.go` | Discover manifests, skip plugin trees, `Bundle.Plugins`, scope checks, `Describe` | WP2 |
| `internal/agents/agents.go` | `Definition.Plugins *[]string` | WP2 |
| `internal/session/agents.go` | `loadAgents` rules, `AgentInfo.Plugins` | WP2 |
| `cmd/mayflyd/main.go` | `RequirePluginImage`; ledger and gateway; `mgr.SetGateway`; reconciler | WP2, WP4, WP7, WP12 |
| `internal/journal/memory.go`, `internal/journal/bbolt.go` | Implement `Ledger`; `calls` bucket; `Delete` removes ledger records | WP3 |
| `internal/config/config.go` | `PluginsConfig` (`plugins:`) in WP3; `RuntimeConfig.PluginVolumeLimit` in WP7; `MCPServerConfig.Tools`/`IdempotentTools` in WP9 | WP3, WP7, WP9 |
| `internal/httpapi/server.go` | Construct the gateway; register the tool routes | WP4 |
| `internal/httpapi/internal.go` | Tool route handlers | WP4 |
| `internal/platformclient/client.go` | `Tools`, `BeginCall`, `CompleteCall` | WP4 |
| `internal/session/manager.go` | `resolvePlugins`, `copySession`, `prepare`, grants on create and adopt (WP7); `mcp.servers` tool lists (WP9); gateway key (WP13) | WP7, WP9, WP13 |
| `internal/session/lifecycle.go` | Finalize drops the grant; ledger deleted with the journal | WP7 |
| `internal/session/restart.go` | Local replacement re-resolves plugin secrets | WP7 |
| `internal/runner/runner.go`, `internal/runner/local.go` | `Spec.PluginSecrets`; write secret files | WP7 |
| `internal/model/model.go` | `Session.Plugins` | WP7 |
| `internal/k8s/spec.go` | Inline init containers and mounts (WP5); sidecars, per-plugin socket volumes, `gateway-key-<name>` (WP13) | WP5, WP13 |
| `internal/k8s/client.go` | Plugin object helpers (WP12); `SessionKeys` (WP13) | WP12, WP13 |
| `internal/mcp/stdio.go`, `internal/mcp/mcp.go`, `internal/mcp/http.go` | `NewStdioExact`, `CallToolMeta`, `Cancel`, `Exited`, `NewHTTPWithClient` (WP6); UDS dialer (WP13) | WP6, WP13 |
| `internal/mcp/manager.go` | Optional tool allowlist; naming unchanged | WP9 |
| `internal/broker/broker.go` | `HTTPClient()` accessor | WP9 |
| `internal/ratelimit/ratelimit.go` | No change expected; the gateway keys limiters itself | none |
| `internal/agent/tools.go` | Route `plugin_*` and `mcp_*` through the gateway; per-tool timeout and output cap | WP8 |
| `internal/agent/runtime.go` | Start and close the host; system events; gateway calls in `executeTool` (WP8); sidecar members (WP13) | WP8, WP13 |
| `internal/agent/steps.go` | `toolStepOutput.Interrupted`, `Replayed` | WP8 |
| `internal/agent/finalize.go` | Plugin stderr logs; `Manifest.Plugins` | WP8 |
| `internal/bundle/bundle.go` | `Manifest.Plugins []string` | WP8 |
| `deploy/k8s/base/rbac.yaml`, `deploy/helm/mayfly/templates/rbac.yaml` | Deployments, services, update verbs | WP12 |
| `deploy/helm/mayfly/values.yaml` | `plugins:` section, `plugin_volume_limit` | WP15 |
| `Dockerfile`, `Makefile` | Build and install `mayfly-plugin-shim`; `mkdir -p /opt/mayfly/plugins` | WP5 |
| `cmd/llmstub/main.go` or `internal/stub/llm.go` | Script `plugin_*` calls if the stub cannot already | WP8, WP14 |
| `docs/*`, `ARCHITECTURE.md`, `README.md`, `CHANGELOG.md` | Documentation | WP15 |

## Types and schema

### `internal/plugin` (new)

No Kubernetes or MCP imports. Used by `internal/project`, `internal/pluginsvc`,
`cmd/mayfly`, and tests.

```go
package plugin

const SchemaVersion = 1

const (
	ManifestFile   = "plugin.yaml"
	FlatSuffix     = ".plugin.yaml"
	SecretPrefix   = "mayfly-plugin-"
	MaxToolName    = 64
	MaxTimeout     = 15 * time.Minute
	MaxOutputBytes = 64 << 10 // agent.maxToolOutput
)

type Placement string

const (
	Inline  Placement = "inline"
	Sidecar Placement = "sidecar"
	Service Placement = "service"
)

type Manifest struct {
	SchemaVersion int               `yaml:"schemaVersion"`
	Name          string            `yaml:"name"`
	Version       string            `yaml:"version"`
	Description   string            `yaml:"description"`
	Scope         string            `yaml:"scope"`
	Placement     Placement         `yaml:"placement"` // default inline
	Image         string            `yaml:"image"`
	Command       string            `yaml:"command"`
	Args          []string          `yaml:"args"`
	Local         *LocalRun         `yaml:"local"`
	Required      bool              `yaml:"required"`
	Env           map[string]string `yaml:"env"`
	Secrets       []Secret          `yaml:"secrets"`
	Tools         []Tool            `yaml:"tools"`
	Limits        Limits            `yaml:"limits"`
	Inline        *InlineSpec       `yaml:"inline"`
	Sidecar       *SidecarSpec      `yaml:"sidecar"`
	Service       *ServiceSpec      `yaml:"service"`

	Dir    string `yaml:"-"` // absolute directory of the manifest file
	Origin string `yaml:"-"` // manifest path, for errors
}

type LocalRun struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}

type Secret struct {
	Env    string `yaml:"env"`
	Secret string `yaml:"secret"`
	Key    string `yaml:"key"`
}

type Tool struct {
	Name       string `yaml:"name"`
	Idempotent bool   `yaml:"idempotent"`
	Timeout    string `yaml:"timeout"`
}

type Limits struct {
	InitTimeout    string `yaml:"initTimeout"`    // default 30s
	CallTimeout    string `yaml:"callTimeout"`    // default 120s
	MaxOutputBytes int    `yaml:"maxOutputBytes"` // default 65536
	Rate           *Rate  `yaml:"rate"`
}

type Rate struct {
	Requests int    `yaml:"requests"`
	Interval string `yaml:"interval"`
	Burst    int    `yaml:"burst"` // default Requests
}

type InlineSpec struct {
	Workdir     string `yaml:"workdir"`     // plugin (default) or workspace
	MaxRestarts *int   `yaml:"maxRestarts"` // default 2
	StderrBytes int64  `yaml:"stderrBytes"` // default 1 MiB
}

type Resources struct {
	Requests map[string]string `yaml:"requests"`
	Limits   map[string]string `yaml:"limits"`
}

type SidecarSpec struct {
	Resources Resources `yaml:"resources"`
	RunAsUser *int64    `yaml:"runAsUser"` // default 65532; never 0
	Workspace string    `yaml:"workspace"` // none (default), read, write
}

type Egress struct {
	CIDR  string  `yaml:"cidr"`
	Ports []int32 `yaml:"ports"`
}

type ServiceSpec struct {
	Replicas    *int32    `yaml:"replicas"`    // default 1
	Concurrency int       `yaml:"concurrency"` // default 8
	Resources   Resources `yaml:"resources"`
	Egress      []Egress  `yaml:"egress"`
	Transport   string    `yaml:"transport"` // stdio (default) or http
	Port        int32     `yaml:"port"`      // default 8080
	Path        string    `yaml:"path"`      // default /mcp
	MCPServer   string    `yaml:"mcpServer"`
}

type ValidateOptions struct {
	DeniedEnv    []string // config.SecretEnvNames()
	RequireImage bool     // kubernetes backend
}

func Parse(raw []byte, origin string) (Manifest, error)
func (m *Manifest) Validate(opts ValidateOptions) error
func Load(dir string, opts ValidateOptions) (Manifest, error)

func ToolName(plugin, tool string) string // "plugin_" + sanitize(plugin) + "_" + sanitize(tool)
func IsManifestPath(rel string) (name string, manifest, skip bool)
func ContainerName(plugin string) string // "plugin-" + mapped name, DNS label
func ObjectName(plugin string) string    // "mayfly-plugin-" + mapped name, DNS-1035 label

func (m Manifest) InitTimeout() time.Duration
func (m Manifest) CallTimeout() time.Duration
func (m Manifest) ToolTimeout(name string) time.Duration
func (m Manifest) Effects() []string // tools with Idempotent=false, sorted
func (m Manifest) SpecHash() string  // service annotation
```

`sanitize` must match `internal/mcp.sanitize`. Copy it rather than import
`internal/mcp`, and test both on a shared table.

### `internal/sessioncfg` additions

```go
type Plugin struct {
	Name               string            `json:"name"`
	Placement          string            `json:"placement"`               // inline, sidecar, service
	Image              string            `json:"image,omitempty"`         // pod rebuilds; the runtime ignores it
	Dir                string            `json:"dir,omitempty"`           // inline
	Command            string            `json:"command,omitempty"`       // inline, sidecar
	Args               []string          `json:"args,omitempty"`
	Workdir            string            `json:"workdir,omitempty"`       // inline
	Socket             string            `json:"socket,omitempty"`        // sidecar
	Required           bool              `json:"required,omitempty"`
	Env                map[string]string `json:"env,omitempty"`           // inline, sidecar
	Secrets            []PluginSecret    `json:"secrets,omitempty"`       // inline, sidecar; names only
	SecretsDir         string            `json:"secrets_dir,omitempty"`   // inline
	Tools              []PluginTool      `json:"tools"`
	InitTimeoutSeconds int               `json:"init_timeout_seconds"`
	MaxRestarts        int               `json:"max_restarts,omitempty"`  // inline
	MaxOutputBytes     int               `json:"max_output_bytes"`
	StderrBytes        int64             `json:"stderr_bytes,omitempty"`  // inline
	Sidecar            *PluginSidecar    `json:"sidecar,omitempty"`       // pod rebuilds
}

type PluginSidecar struct {
	Requests  map[string]string `json:"requests,omitempty"`
	Limits    map[string]string `json:"limits,omitempty"`
	RunAsUser int64             `json:"run_as_user"`
	Workspace string            `json:"workspace"`
}

type PluginSecret struct {
	Env    string `json:"env"`
	Secret string `json:"secret,omitempty"` // empty on the local backend
	Key    string `json:"key,omitempty"`
}

type PluginTool struct {
	Name           string `json:"name"` // remote name
	Idempotent     bool   `json:"idempotent,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// Config gains:   Plugins []Plugin `json:"plugins,omitempty"`
// MCPServer gains:
//	Tools           []string `json:"tools,omitempty"`
//	IdempotentTools []string `json:"idempotent_tools,omitempty"`

// Child environment policy (env.go):
const DefaultPath = "/usr/local/bin:/usr/bin:/bin"
func StdioEnv(extra map[string]string) []string // complete env of a stdio MCP server
func PassthroughEnv() map[string]string         // locale, TZ, TLS trust, proxies, both cases
```

Service plugins appear in `Config.Plugins` with only `Name`, `Placement`,
`Required`, `Tools`, `InitTimeoutSeconds`, and `MaxOutputBytes`, so the
grant can be rebuilt from `session.json` on adoption. `session.json` never
holds a secret value. Rate limits come from the bundle in `mayflyd`, not from
`session.json`.

`Config.Validate` additions: unique names; known placement; inline needs
`Dir`, `Command`, `Workdir`; sidecar needs `Socket`, `Command`, `Sidecar`;
at least one tool; tool names unique per plugin; positive timeouts;
`MaxOutputBytes` in `1..65536`; secret env names pass `IsReservedEnv` and the
env-name pattern; `SecretsDir` required for inline plugins with secrets.
Independent of `internal/plugin`, so the runtime does not import the loader.

### `internal/journal`: ledger

```go
type CallState string

const (
	CallPending CallState = "pending"
	CallDone    CallState = "done"
)

type Call struct {
	Key         string    `json:"key"` // <session>/<turn>/<step>
	SessionID   string    `json:"session_id"`
	Turn        int64     `json:"turn"`
	Step        int       `json:"step"`
	Tool        string    `json:"tool"` // model-visible name
	ArgsSHA256  string    `json:"args_sha256"`
	State       CallState `json:"state"`
	Content     string    `json:"content,omitempty"`
	IsError     bool      `json:"is_error,omitempty"`
	Interrupted bool      `json:"interrupted,omitempty"`
	Attempts    int       `json:"attempts"`
	StartedAt   time.Time `json:"started_at"`
	DoneAt      time.Time `json:"done_at,omitempty"`
}

type Ledger interface {
	// Begin returns the existing record for c.Key, or stores c as pending.
	// created reports whether c was stored. Begin on a pending record
	// increments Attempts.
	Begin(ctx context.Context, c Call) (rec Call, created bool, err error)
	// Complete marks the record done. Repeating it with the same content is
	// a no-op; different content is an error.
	Complete(ctx context.Context, session, key, content string, isError, interrupted bool) (Call, error)
	Get(ctx context.Context, session, key string) (Call, bool, error)
}
```

`Memory` and `Bolt` implement it. Bolt adds a top-level bucket `calls` next
to `journal`, `seq`, and `last`, with a nested bucket per session keyed by
idempotency key. `Delete(ctx, session)` also deletes the session's `calls`
bucket, so finalize (`internal/session/lifecycle.go`, which already calls
`m.journal.Delete`) removes ledger records with the journal. Stored content
counts against `journal.max_session_bytes` with the same accounting as
`Manager.AppendJournal`.

### `internal/gateway` (new)

```go
type Placement string // inline, sidecar, service, mcp-stdio, mcp-http

type GrantTool struct {
	Name       string // model-visible
	Owner      string // plugin or mcp server name
	Remote     string // tool name on the server
	Placement  Placement
	Idempotent bool
	Timeout    time.Duration
	MaxOutput  int
}

type Grant struct {
	SessionID  string
	Agent      string
	Tools      map[string]GrantTool // fixed at create
	Wildcards  map[string]Placement // HTTP mcp.servers without a tools list
	SidecarKey []byte               // nil without sidecars
}

type Options struct {
	Ledger      journal.Ledger
	Bundle      func() *project.Bundle // manifests: service details, rate limits
	MCPServer   func(name string) (config.MCPServerConfig, bool)
	HTTPClient  *http.Client                                        // broker.HTTPClient()
	ServiceURL  func(plugin string) (url string, key []byte, err error) // pluginsvc
	MaxInflight int                                                 // plugins.max_inflight_calls
	Audit       *Audit
	Logger      *slog.Logger
	Now         func() time.Time
}

func New(opts Options) *Gateway
func GrantFor(cfg sessioncfg.Config, agent string, mcp []config.MCPServerConfig) Grant
func (g *Gateway) Register(grant Grant)
func (g *Gateway) Drop(session string) // closes upstream clients and limiters
func (g *Gateway) Discover(ctx context.Context, session string) (Discovery, error)
func (g *Gateway) Begin(ctx context.Context, session string, req BeginRequest) (BeginResponse, error)
func (g *Gateway) Complete(ctx context.Context, session string, req CompleteRequest) error
```

Grant tools for plugins come from the manifest, not from the server's
`tools/list`; the runtime verifies the list against them. HTTP
`mcp.servers` without a `tools` allowlist get a wildcard resolved against
the discovered list, which keeps today's "every tool of a selected server"
behavior.

### `internal/config`

```go
type Config struct {
	// ...existing...
	Plugins PluginsConfig `yaml:"plugins"`
}

type PluginsConfig struct {
	Namespace         string   `yaml:"namespace"`          // default sessions.namespace
	ReconcileInterval Duration `yaml:"reconcile_interval"` // default 60s
	Prune             *bool    `yaml:"prune"`              // default true
	AuditPath         string   `yaml:"audit_path"`         // optional JSONL file
	MaxInflightCalls  int      `yaml:"max_inflight_calls"` // default 256, across sessions
}

type RuntimeConfig struct {
	// ...existing...
	PluginVolumeLimit string `yaml:"plugin_volume_limit"` // default 512Mi
}

type MCPServerConfig struct {
	// ...existing...
	Tools           []string `yaml:"tools"`
	IdempotentTools []string `yaml:"idempotent_tools"`
}
```

Plugin images, including service Deployments, use `agents.allowed_images`,
`runtime.image_pull_secrets`, and `runtime.image_pull_policy`.

### `internal/k8s`

```go
const (
	PluginsDir       = "/opt/mayfly/plugins"
	PluginSecretsDir = "/var/run/mayfly/plugin-secrets"
	PluginSocketsDir = "/run/mayfly/plugin-sockets"
	GatewayKeyDir    = "/var/run/mayfly/gateway"
	ToolsDir         = "/mayfly-tools"
	PluginShim       = "/usr/local/bin/mayfly-plugin-shim"
)

type SessionSpec struct {
	// ...existing fields...
	Plugins           []sessioncfg.Plugin
	ToolsImage        string // runtime.image
	PluginVolumeLimit string
	GatewayKey        []byte // session Secret key gateway-key; nil without sidecars
	// SidecarKeys holds one derived key per sidecar plugin, from
	// HMAC(GatewayKey, "mayfly/sidecar/" + name). The pod builder writes each
	// as session Secret item gateway-key-<name> and mounts only that item into
	// that sidecar.
	SidecarKeys map[string][]byte
}
```

Inline pod additions (WP5): volumes `mayfly-tools` (emptyDir 64Mi) and
`plugins` (emptyDir `PluginVolumeLimit`); one projected
`plugin-secrets-<name>` per inline plugin with secrets (`items` key →
`<env>`, mode 0400). Init containers `mayfly-tools`, then `plugin-<name>`,
each with the agent container's security context (non-root 65532, read-only
root, no escalation, drop ALL, RuntimeDefault seccomp), the session pull
policy, requests `{cpu: 10m, memory: 32Mi}`, limits `{memory: 128Mi}`, no
env, no secrets. The agent container mounts `plugins` read-only at
`PluginsDir` and each secret volume at `PluginSecretsDir/<name>`. Inline
plugins run as the agent's UID, so the runtime clears its dumpable flag at
startup: a plugin then cannot read the session token from
`/proc/<pid>/environ`, although it can still read a sibling plugin's secret
files.

Sidecar pod additions (WP13): one volume `plugin-sockets-<name>` per sidecar
plugin (emptyDir, `medium: Memory`, 1Mi); the session Secret gains
`gateway-key-<name>` for each sidecar plugin; one native sidecar
`plugin-<name>` per sidecar plugin, after `mayfly-tools`, with
`restartPolicy: Always`, image `Image`, command
`/mayfly-tools/mayfly-plugin-shim serve --listen unix:/run/mayfly/plugin-sockets/<name>/<name>.sock --key-file /var/run/mayfly/gateway/gateway-key --session <id> -- <command> <args>`;
env `Env`, `secretKeyRef` per secret, `MAYFLY_PLUGIN_NAME`,
`MAYFLY_PLUGIN_DIR=/mayfly-plugin`, `MAYFLY_SESSION_ID`, and
`MAYFLY_WORKSPACE_DIR` when mounted; mounts `mayfly-tools` read-only,
its own `plugin-sockets-<name>` read-write at
`/run/mayfly/plugin-sockets/<name>/`, the session Secret with only
`items: [gateway-key-<name>]` at `GatewayKeyDir`, the workspace per
`Sidecar.Workspace`, and a private emptyDir at `/tmp`; the agent security
context with `runAsUser` from the manifest; manifest resources; a
`startupProbe` running `shim probe`. The agent container mounts every
`plugin-sockets-<name>` read-write at its directory and never mounts any
`gateway-key-*` item. Today the token
reaches the agent through `secretKeyRef` on key `token` in `Pod()`; keep it
that way and add no volume of the whole session Secret.

### Agent journal payloads (`internal/agent/steps.go`)

```go
type toolStepOutput struct {
	Message     llm.Message         `json:"message"`
	IsError     bool                `json:"is_error,omitempty"`
	Executed    bool                `json:"executed"`
	Replayed    bool                `json:"replayed,omitempty"`    // result came from the ledger
	Interrupted bool                `json:"interrupted,omitempty"` // the gateway reported an interrupted call
	Events      []journal.StepEvent `json:"events,omitempty"`
}
```

No pending entries in the journal.

## API

All routes are on the internal listener, authenticated with the session
token through `s.internalSession(r)` like the journal routes, with bodies
capped by `server.max_body_bytes`. A session whose agent uses an alternate
harness gets 404.

### `GET /internal/v1/sessions/{id}/tools`

```json
{
  "tools": [
    {"name": "plugin_tickets_get_ticket", "owner": "tickets", "placement": "service",
     "description": "...", "input_schema": {"type": "object"}, "idempotent": true, "timeout_ms": 120000},
    {"name": "plugin_notify_post", "owner": "notify", "placement": "sidecar",
     "idempotent": false, "timeout_ms": 30000}
  ],
  "sidecars": {"notify": {"discovery_token": "v1...."}},
  "errors": [{"owner": "tickets", "required": false, "error": "no ready replica"}]
}
```

Remote tools carry description and schema from gateway discovery; local
tools carry grant fields only and the runtime fills the rest from
`tools/list`. A required owner in `errors` makes the runtime fail the
session at start.

### `POST /internal/v1/sessions/{id}/tools/calls`

Request: `{"turn": 3, "step": 4, "call_id": "call_ab", "tool": "plugin_notify_post", "arguments": {}}`

200 responses:

```json
{"result": {"content": "...", "is_error": false, "replayed": false, "interrupted": false}}
{"dispatch": {"key": "s1/3/4", "timeout_ms": 30000, "token": "v1...."}}
```

`token` is set only for sidecar tools. Errors: 400 bad turn or step; 403
tool not granted; 409 key reused with another tool or arguments; 429 rate
limited, with `Retry-After` (the runtime turns it into an error result, it
does not retry); 503 in-flight cap reached (the runtime retries within its
budget).

### `POST /internal/v1/sessions/{id}/tools/calls/complete`

Request: `{"key": "s1/3/4", "content": "...", "is_error": false}`. 204 on
success and on a repeat with identical content; 409 when the record is done
with different content; 404 for an unknown key.

### `internal/platformclient`

```go
func (c *Client) Tools(ctx context.Context) (gateway.Discovery, error)
func (c *Client) BeginCall(ctx context.Context, req gateway.BeginRequest) (gateway.BeginResponse, error)
func (c *Client) CompleteCall(ctx context.Context, req gateway.CompleteRequest) error
```

The types live in `internal/gateway/api.go`, which imports nothing from the
server side.

## Behavior

### Loader rules (`internal/project`)

For each path under a bundle directory, relative and slash-separated:

| Path | Treatment |
|---|---|
| `plugins/<name>/plugin.yaml` | Plugin manifest |
| any other file under `plugins/<name>/` | Skipped (plugin source) |
| `plugins/<file>.yaml` directly under `plugins/` | Bundle file as today |
| `**/<name>.plugin.yaml` outside `plugins/*/` | Plugin manifest (flat form) |
| everything else | Unchanged |

Only the top-level `plugins/` directory of each `agents.dirs` entry is
special. Hidden directories stay skipped. The manifest name must equal
`<name>` from the path. A duplicate name is an error naming both paths.
Files are read through the existing `os.Root`, so manifests cannot escape the
bundle directory.

### Validation rules

At load (`plugin.Manifest.Validate`, called by `project.Load`):

1. `schemaVersion` is 1; `name` matches the project `namePattern`.
2. `scope` parses with `parseScope`; a private owner must be a known agent
   (extend `checkScopeOwners`).
3. `placement` is empty (inline), `inline`, `sidecar`, or `service`. Only the
   block matching the placement may be set.
4. `command` and `image` are required, except with `service.mcpServer`, where
   `image`, `command`, `args`, `local`, `env`, `secrets`, and every other
   `service` field must be empty. `image` may be empty when `RequireImage`
   is false (local backend).
5. `local.command` defaults to `command`, `local.args` to `args`.
6. `inline.workdir` is empty, `plugin`, or `workspace`.
7. `env` keys and `secrets[].env` pass `prompt.EnvPolicy{Deny: denied}.Check`
   (rejects `MAYFLY_*` and `config.SecretEnvNames`); `PATH`, `HOME`, `TMPDIR`
   are rejected; no name is in both; `env` values starting with `env:` are
   rejected with the stdio MCP server message from `config.resolveSecrets`.
8. `secrets[].secret` starts with `mayfly-plugin-` and is a DNS subdomain;
   `key` matches `[-._a-zA-Z0-9]+`.
9. At least one tool; tool names non-empty and unique; `ToolName` results
   unique within the plugin and at most 64 characters.
10. Durations parse, are positive, at most 15m. `inline.maxRestarts` is
    0..10, `maxOutputBytes` 1..65536, `inline.stderrBytes` 0..16 MiB.
    `limits.rate.requests` ≥ 1, `interval` ≥ 1s, `burst` ≥ 0.
11. `sidecar.runAsUser` is not 0; `sidecar.workspace` is `none`, `read`, or
    `write`; resource quantities parse.
12. `service.replicas` 0..20 (0 parks the plugin), `concurrency` 1..256,
    `transport` `stdio` or `http`, `port` 1..65535, `path` starts with `/`,
    `egress[].cidr` parses, ports 1..65535.

At startup (`session.loadAgents`, so `session.Validate` and `-check` run
them too):

13. Every name in an agent's `plugins` exists and is visible (`scopeVisible`).
14. An agent with plugins uses the built-in harness
    (`harnessName(agent) == "mayfly"`).
15. An agent for which `agentAllowShell` is true cannot select an `inline`
    plugin with `secrets`; on the local backend, not a `sidecar` plugin with
    `secrets` either. The message suggests `placement: sidecar`.
16. Plugin images pass `imageAllowed(image, cfg.Agents.AllowedImages)`.
17. `service.mcpServer` names an `mcp.servers` entry with `transport: http`
    that is visible to every agent selecting the plugin.
18. Model-visible names do not collide across an agent's plugins.

Config (`config.Validate`): `plugins.reconcile_interval` ≥ 10s;
`plugins.max_inflight_calls` ≥ 1; `mcp.servers[].tools` and
`idempotent_tools` hold unique names, and `idempotent_tools` ⊆ `tools` when
`tools` is set; `runtime.plugin_volume_limit` parses as a quantity.

### Session creation and adoption (`internal/session`)

`resolveRequest` calls `resolvePlugins(agent)` and returns
`[]sessioncfg.Plugin`. `Create`:

1. Sets `runtimeCfg.Plugins` and `model.Session.Plugins`. `CreateOptions`
   does not change: requests and triggers cannot select plugins.
2. Generates a 32-byte sidecar key when any plugin is a sidecar and passes it
   as `SessionSpec.GatewayKey`.
3. Builds the grant with `gateway.GrantFor` and registers it before the pod
   starts.

Adoption rebuilds the grant from the stored `session.json` with the same
function and reads the sidecar key from the session Secret. Finalize calls
`gateway.Drop`; the existing journal `Delete` removes the ledger records.

Per inline plugin, `resolvePlugins` fills:

| Field | Kubernetes backend | Local backend |
|---|---|---|
| `Dir` | `k8s.PluginsDir + "/" + name` | `manifest.Dir` |
| `Command`, `Args` | `command`, `args` | `local.command`, `local.args` |
| `SecretsDir` | `k8s.PluginSecretsDir + "/" + name` | `<state>/sessions/<id>/plugin-secrets/<name>` |
| `Secrets[].Secret`, `Key` | from the manifest | empty |
| `Image` | `image` | empty |

On the local backend a sidecar plugin resolves as inline (`Placement:
inline` in `session.json`) and `mayflyd` logs a warning at startup.

Local backend secrets: `runner.Spec.PluginSecrets map[string]map[string]string`
(plugin, env, value), resolved from the `mayflyd` environment by
`secrets[].env` at create and at local restart. A missing variable fails
`Create` and `Validate`. `local.Start` writes each value to
`SecretsDir/<env>`, mode 0600 in a 0700 directory, before starting the agent.
Values are never logged or written to `session.json`.

### Runtime host (`internal/pluginhost`)

Inline plugins, as in v1:

```go
func Start(ctx context.Context, plugins []sessioncfg.Plugin, opts Options) (*Host, []error)
func (h *Host) RequiredErr() error
func (h *Host) Tools() []Tool
func (h *Host) Tool(name string) (Tool, bool)
func (h *Host) Hidden() map[string][]string
func (h *Host) Call(ctx context.Context, name string, args json.RawMessage, key, token string) (string, bool, error)
func (h *Host) Close()
```

- Environment: `PATH` = `<Dir>/bin:` + container `PATH`; private `HOME` and
  `TMPDIR` under `/tmp/plugins/<name>/`; the `LANG`, `TZ`,
  `SSL_CERT_FILE`, `SSL_CERT_DIR`, `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`
  variables and their lowercase forms from `sessioncfg.PassthroughEnv`;
  `MAYFLY_PLUGIN_NAME`, `MAYFLY_PLUGIN_DIR`, `MAYFLY_SESSION_ID`,
  `MAYFLY_WORKSPACE_DIR`; then `Env`; then secrets read from
  `SecretsDir/<env>` (one trailing newline trimmed). Nothing else is
  inherited; test that `MAYFLY_SESSION_TOKEN` and `MAYFLY_CONFIG` are absent
  even when set in the parent.
- Command: absolute as is; containing `/` resolves against `Dir`; otherwise
  `exec.LookPath` on the built `PATH`. Working directory `Dir` or the
  workspace per `Workdir`.
- Spawn with `mcp.NewStdioExact`, own process group, stderr to the sink;
  `initialize` and `tools/list` within `InitTimeoutSeconds`; verify declared
  tools are reported, schemas are JSON objects (empty becomes
  `{"type":"object","properties":{}}`), undeclared tools are hidden.
- `Call`: restart an exited plugin if restarts remain and re-verify; send
  `tools/call` with `_meta` (`mayfly.io/idempotency-key`,
  `mayfly.io/session`, `mayfly.io/agent`) under the tool timeout; on timeout
  send `notifications/cancelled` and return an error result; on exit during
  the call return an error result with the stderr tail; never retry inside
  `Call`; truncate with the existing `truncate` format.
- Stderr sink: lines to `ArtifactsDir/plugins/<name>.stderr.log` up to
  `StderrBytes`, then one `[stderr truncated]` marker; debug log lines cut at
  2 KiB; last 8 KiB kept (`tailBuffer`).
- `Close`: per plugin in parallel, close stdin, wait 2s, SIGTERM the group,
  wait 2s, SIGKILL. It holds no call lock. The client pointer sits behind its
  own lock, so `Close` kills a process whose call is in flight and that call
  returns the exit result; a shutdown must not wait for a tool timeout of up
  to 15 minutes, or the bundle and the finalize step are lost. The stdio
  transport's `Close` takes no call lock either, for the same reason.

Sidecar members (WP13): an `mcp.Client` over HTTP with a Unix-socket
dialer, sending `Authorization: Bearer <token>` (discovery token for
`initialize` and `tools/list`, call token for `tools/call`). An unreachable
socket is reported as a call error; nothing is restarted by the host,
Kubernetes restarts the sidecar.

The host never talks to the gateway; `internal/agent` does.

### MCP client changes (`internal/mcp`)

- `NewStdioExact(ctx, name, command string, args, env []string, dir string, stderr io.Writer) (*Client, error)`:
  `env` is the complete environment. `NewStdio` keeps `mergeEnv`.
- `(*Client).CallToolMeta(ctx, name string, args json.RawMessage, meta map[string]any)`;
  `CallTool` calls it with nil meta, and `_meta` is omitted when nil.
- `(*Client).Cancel(ctx, id int64, reason string)` sends
  `notifications/cancelled`; `call` exposes the request ID it used.
- `(*Client).Exited() <-chan struct{}` (stdio only; nil for HTTP).
- `stdioTransport.Close` gains SIGTERM before SIGKILL.
- `NewHTTPWithClient(name, url string, headers map[string]string, client *http.Client, perRequest func(*http.Request)) *Client`.
  `NewHTTP(name, url, token, headers)` keeps its signature and calls it.
- WP13: a Unix-socket option, a client whose `Transport.DialContext` dials
  the socket, with URL `http://plugin/mcp`.

### Runtime integration (`internal/agent`)

- `Runtime.Run`: after `mcp.Start`, start `pluginhost` for inline (and, from
  WP13, sidecar) plugins, then call `platformclient.Tools`. Required
  failures (local, or in `errors`) finalize the session as `failed`, as
  `newToolset` failures do today; others emit `model.EventSystem`
  `Plugin unavailable: <err>`. Hidden tools emit
  `Plugin <name> exposes tools that the manifest does not declare; they are hidden: a, b`.
  The "Session started" event data gains `plugins`.
- HTTP `mcp.servers` (from WP9): the runtime no longer connects to
  `/internal/v1/mcp/{server}`; their tools come from `Tools()` and are
  called by remote dispatch. `mcp.Start` skips HTTP entries.
- `toolset.Tools()` order: built-ins, `mcp_*` as today, then `plugin_*`.
  Empty plugin descriptions become `Plugin tool <tool> from plugin <plugin>`.
- `executeTool` for every non-built-in tool:
  1. `BeginCall` with turn, step, call id, tool, and arguments.
  2. `result`: use it. Set `Replayed` or `Interrupted` on the step output.
     For interrupted, add a `tool_result` event with `data.interrupted =
     true` and a `system` event with the message:
     `ERROR: tool <name> was interrupted by a container restart and may already have taken effect. Mayfly did not run it again because the tool is not idempotent. Check the target before retrying.`
  3. `dispatch`: call locally (`pluginhost` or `mcp.Manager`) with the key
     and token under `timeout_ms`, then `CompleteCall`, then return.
  4. 403 and 429 become error results; 409 is fatal; network errors and 503
     retry within the 5-minute budget, then fatal.
- `runOrReplay`, `verifyTurn`, and `prepareResume` are unchanged: journal
  steps are appended after `executeTool` returns.
- `Run` shutdown calls `plugins.Close()` next to `r.mcp.Close()`.
- `finalize`: `manifest.Plugins`; inline stderr logs added as
  `plugins/<name>.stderr.log`.

### Gateway behavior

Begin:

1. Look up the grant: unknown session → 404, unknown tool → 403.
2. Validate turn and step. Compute `args_sha256` over the canonical JSON
   (decode, then `json.Marshal`, which sorts map keys).
3. Rate limit: one `ratelimit.Limiter` per (session, plugin) from
   `limits.rate`; denied → 429 with `RetryAfter`. Nothing is recorded.
4. In-flight cap: `plugins.max_inflight_calls` across sessions, counting
   remote calls in progress and local dispatches not yet completed or
   expired → 503.
5. `Ledger.Begin` and the resume table. Existing record with another tool or
   hash → 409.
6. Local: return `dispatch` (with a call token for sidecars). Remote: call
   upstream under the tool timeout, `Complete` the record with the result or
   an error result (on timeout, after `notifications/cancelled`), return
   `result`.
7. Audit: `call.begin` and `call.done` records with time, session, agent,
   tool, owner, placement, key, `args_sha256`, attempt, and for done
   `duration_ms`, `is_error`, `replayed`, `interrupted`. Arguments and results
   are never logged. The record goes to the `mayflyd` logger and, with
   `plugins.audit_path`, is appended to that file (O_APPEND, 0600).

Complete: require a `pending` record; cap content at the tool's
`MaxOutput` with the runtime's `truncate` format; store; audit.

Upstream errors become error results with the server's message; HTTP 401 or
403 from an `mcp.servers` upstream becomes `upstream rejected credentials`.

### Service reconciler (`internal/pluginsvc`)

Rules beyond the [service decision](#service-created-by-mayflyd-authenticated-by-networkpolicy-and-call-tokens):

- `ServiceURL(plugin)` returns `http://mayfly-plugin-<name>.<ns>.svc:8080/mcp`
  (or `service.port` and `service.path` for `transport: http`) and the key
  from `mayfly-gateway-<name>`, cached and re-read after a 401 from the
  shim.
- The gateway learns readiness lazily: a failed upstream `initialize` makes
  the plugin unavailable for that session, reported in `Discover().errors`.
- Updates use update with `resourceVersion` on hash change, which the fake
  clientset supports.
- Local backend: one `mayfly-plugin-shim serve --listen unix:<state>/plugins/<name>.sock`
  child per service plugin, with `local.command`, env from `env`, and
  secrets from the `mayflyd` environment; restarted with back-off; killed on
  shutdown. `service.mcpServer` needs nothing locally.

### Shim `serve` (`cmd/mayfly-plugin-shim`)

- Starts the child with the container environment minus every `MAYFLY_*`
  except `MAYFLY_PLUGIN_NAME`, `MAYFLY_PLUGIN_DIR`, `MAYFLY_SESSION_ID`,
  `MAYFLY_WORKSPACE_DIR`.
- Initializes the child once. Each client's `initialize` is answered with
  the child's capabilities; the child sees one long session.
- Forwards `tools/list` as is and `tools/call` after token checks, rewriting
  JSON-RPC ids to a shim counter and back. Forwards
  `notifications/cancelled` with the rewritten id.
- `--max-concurrency` bounds in-flight calls; excess gets HTTP 503.
- Child exit fails in-flight calls with an error result and the stderr tail;
  the child restarts with back-off from 1s doubling to 30s; `probe` fails
  while it is down.
- Child stderr goes to the container's stderr.

## Future options

Not designed; listed so later work has a starting point.

- **gVisor or Kata through `runtimeClassName`.** Pod-level, so it fits a
  `service` Deployment or a whole session pod, not one sidecar in a pod.
- **WebAssembly plugins.** A WASI runtime in the shim or the runtime, with
  capability-based file and network access.

Until one of these is designed, plugins are trusted code.

## Work packages

### Order and parallelism

```text
Phase A: gateway and inline
  Wave 1:  WP1
  Wave 2:  WP2   WP3   WP5   WP6     (disjoint files)
  Wave 3:  WP4   WP7   WP10          (disjoint files)
  Wave 4:  WP8   WP9                 (disjoint files)
Phase B: service
  Wave 5:  WP11
  Wave 6:  WP12
Phase C: sidecar
  Wave 7:  WP13
Closing
  Wave 8:  WP14 (finish)   WP15
```

| ID | Title | Depends on | Wave | Phase |
|---|---|---|---|---|
| WP1 | Manifest v2 and session config types | none | 1 | A |
| WP2 | Bundle loading, agent selection, `-check` | WP1 | 2 | A |
| WP3 | Ledger and gateway core | WP1 | 2 | A |
| WP4 | Gateway endpoints and platformclient | WP3 | 3 | A |
| WP5 | Shim `install-self`/`copy` and inline pod packaging | WP1 | 2 | A |
| WP6 | MCP client changes and inline plugin host | WP1 | 2 | A |
| WP7 | Session resolution, grants, local secrets | WP2, WP3, WP5 (merge after WP4) | 3 | A |
| WP8 | Runtime integration through the gateway | WP4, WP6, WP7 | 4 | A |
| WP9 | Gateway remote dispatch and the `mcp.servers` fold | WP4, WP6, WP7 | 4 | A |
| WP10 | CLI and examples | WP1, WP6 | 3 | A |
| WP11 | Shim `serve`, `probe`, call tokens | WP3, WP5, WP6 | 5 | B |
| WP12 | Service reconciler, RBAC, endpoint mode, local children | WP9, WP11 | 6 | B |
| WP13 | Sidecar pod spec and UDS dispatch | WP8, WP11, WP12 | 7 | C |
| WP14 | End-to-end tests on k3d | WP8 and WP9 (A), WP12 (B), WP13 (C) | 4 to 8 | A to C |
| WP15 | Documentation | all others | 8 | A to C |

WP11 has no phase A dependency beyond waves 2 and 3, so it may start in
wave 3 or 4 when an agent is free; it is listed in wave 5 to keep the build
order (gateway and inline, then service, then sidecar). WP13 depends on WP12
only to rebase on `internal/k8s/client.go`.

WP14 opens after wave 4 with the phase A subtests, gains the service
subtests after WP12 and the sidecar subtests after WP13, and closes in
wave 8.

File ownership is disjoint inside a wave. Files shared across waves (the
later package rebases): `internal/config/config.go` (WP3, WP7, WP9),
`internal/session/manager.go` (WP7, WP9, WP13), `internal/k8s/spec.go` (WP5,
WP13), `internal/k8s/client.go` (WP12, WP13), `cmd/mayflyd/main.go` (WP2,
WP4, WP7, WP12; WP4 merges before WP7's one-line change), `internal/gateway/gateway.go` (WP3, WP9, WP13),
`internal/mcp/http.go` (WP6, WP13).

Every package ends with `gofmt`, `go vet ./...`,
`go vet -tags e2e ./e2e/...`, and `go test ./...` passing.

---

### WP1: Manifest v2 and session config types

- **Depends on:** none.
- **Files:** create `internal/plugin/manifest.go`, `internal/plugin/names.go`,
  `internal/plugin/manifest_test.go`; modify `internal/sessioncfg/config.go`,
  `internal/sessioncfg/config_test.go`.
- **Behavior:** the types and functions in
  [`internal/plugin`](#internalplugin-new) and
  [`internal/sessioncfg` additions](#internalsessioncfg-additions);
  validation rules 1 and 3 to 12 (rule 2 needs the bundle and is WP2);
  defaults; `IsManifestPath` per [Loader rules](#loader-rules-internalproject);
  `ContainerName` and `ObjectName` (lowercase, `.` and `_` to `-`, truncated
  to 63 with an 8-character hash suffix on truncation). `Parse` uses
  `yaml.Decoder.KnownFields(true)`, like `decodeManifest`.
- **Acceptance:**
  - Every manifest in `docs/plugins.md` parses and validates (copied into
    `internal/plugin/testdata/` with a comment naming the doc section).
  - Each rule has a failing case whose error names the field and the path.
  - A `service` block on an inline manifest fails; `service.mcpServer` with
    `image` fails.
  - `ToolName("notify", "post") == "plugin_notify_post"`; the sanitizer
    matches `internal/mcp.sanitize` on a shared table.
  - `sessioncfg.Config` with each placement round-trips through JSON;
    `Validate` rejects each invalid case.
- **Tests:** unit: `TestParseRejectsUnknownFields`, `TestValidateRules`
  (table), `TestPlacementBlocks`, `TestDefaults`, `TestToolNameMatchesMCP`,
  `TestIsManifestPath`, `TestContainerAndObjectNames`,
  `TestConfigValidatePlugins`.

### WP2: Bundle loading, agent selection, `-check`

- **Depends on:** WP1.
- **Files:** modify `internal/project/project.go`,
  `internal/project/project_test.go`, `internal/agents/agents.go`,
  `internal/agents/agents_test.go`, `internal/session/agents.go`, a new
  `internal/session/agents_test.go`, `cmd/mayflyd/main.go`
  (`RequirePluginImage`), `docs/sources.md` (expected `-check` output only).
- **Behavior:**
  - `Load`'s `WalkDir` callback consults `plugin.IsManifestPath` before the
    extension switch; manifests are parsed through the existing `os.Root`;
    `buildBundle` validates each and stores `Bundle.Plugins`; private owners
    are checked.
  - `agents.Definition.Plugins *[]string` (`yaml:"plugins" json:"plugins"`);
    nil and empty both mean none; duplicates and invalid names rejected.
  - `Describe`: the first line gains `plugins: N` after `connections: N`;
    one line per plugin, sorted:
    `plugin <name> placement=<p> scope=<scope> tools=<n> effects=<a,b|-> image=<image|->`,
    with ` mcpServer=<name>` for endpoint plugins; agent lines append
    ` plugins=<a,b>` when non-empty.
  - `session.loadAgents`: rules 13 to 18 in the existing message style
    (`%s: agent %q references unknown plugin %q`).
  - `AgentInfo.Plugins []string` (`json:"plugins"`).
- **Acceptance:**
  - A bundle with `plugins/notify/plugin.yaml`, `package.json`, and
    `tsconfig.json` loads without parsing the JSON files.
  - `plugins/x/plugin.yaml` with `name: y` and duplicate names fail, naming
    both paths.
  - `notify.plugin.yaml` at the bundle root loads as plugin `notify`.
  - `-check` for the competitor-watch fixture changes only by `plugins: 0`.
  - Each rule 13 to 18 fails `session.Validate` with a specific message. An
    inline plugin with secrets and `allowShell` fails; a sidecar plugin with
    secrets and `allowShell` passes on kubernetes and fails on local.
  - `GET /api/v1/config` lists each agent's plugins (its `agents` field comes from `Manager.Agents`).
- **Tests:** unit: `TestLoadPluginManifests`, `TestLoadSkipsPluginSource`,
  `TestFlatPluginManifest`, `TestDuplicatePlugin`, `TestPrivatePluginScope`,
  `TestDescribePlugins`; session: `TestAgentUnknownPlugin`,
  `TestAgentPluginScope`, `TestAgentPluginHarness`,
  `TestAgentPluginSecretsWithShell` (placement × backend table),
  `TestAgentPluginImageAllowlist`, `TestAgentPluginMCPServerRef`.

### WP3: Ledger and gateway core

- **Depends on:** WP1.
- **Files:** create `internal/journal/ledger.go`,
  `internal/journal/ledger_test.go`, `internal/gateway/gateway.go`,
  `internal/gateway/api.go`, `internal/gateway/limits.go`,
  `internal/gateway/audit.go`, `internal/gateway/token.go`,
  `internal/gateway/gateway_test.go`; modify `internal/journal/memory.go`,
  `internal/journal/bbolt.go`, `internal/config/config.go` and
  `internal/config/config_test.go` (`plugins:` section only).
- **Behavior:** [ledger](#internaljournal-ledger),
  [gateway types](#internalgateway-new), `GrantFor`, and
  [Gateway behavior](#gateway-behavior) for local dispatch. Remote dispatch
  is an interface that returns an error until WP9, and `Discover` omits
  remote tools until then. Token sign and verify use `hmac.Equal`.
  `Delete` removes ledger records.
- **Acceptance:**
  - One test per resume-table row, including that an interrupted answer
    marks the record done, so a second resume gets the same interrupted
    result.
  - Same key with other arguments → error; same arguments → the existing
    record.
  - A rate-limited begin records nothing and reports a positive
    `RetryAfter`.
  - Bolt records survive close and reopen; `Delete` removes them along with
    the journal.
  - Audit records never contain argument or result text (sentinel test).
  - Tokens: tampered claims, wrong key, expired, wrong tool, wrong hash all
    fail verification.
- **Tests:** unit: `TestLedgerConformance` (memory and bbolt),
  `TestLedgerDeleteWithJournal`, `TestGatewayResumeTable`,
  `TestGatewayKeyReuse`, `TestGatewayRateLimit`, `TestGatewayInflightCap`,
  `TestGatewayStepBounds`, `TestGrantFor`, `TestAuditOmitsPayloads`,
  `TestCallToken`, `TestConfigPluginsDefaults`.

### WP4: Gateway endpoints and platformclient

- **Depends on:** WP3.
- **Files:** modify `internal/httpapi/server.go`, `internal/httpapi/internal.go`,
  `internal/httpapi/server_test.go`, `internal/platformclient/client.go`,
  `internal/platformclient/client_test.go` (create if absent),
  `cmd/mayflyd/main.go` (build the ledger from the journal store, construct
  the gateway, pass it to the server and the session manager options).
- **Behavior:** [API](#api). The server holds a `*gateway.Gateway`; grants
  are registered by the session manager (WP7), so tests here register them
  directly.
- **Acceptance:**
  - Wrong session token → 401; tool not in the grant → 403; alternate
    harness session → 404.
  - Begin and complete round-trip through `httptest` with the memory ledger;
    429 carries `Retry-After`.
  - `platformclient` decodes every response shape and maps 403, 409, 429,
    and 503 to typed errors.
- **Tests:** `TestInternalToolsAuth`, `TestInternalToolCallRoundTrip`,
  `TestInternalToolCallRateLimited`, `TestPlatformClientToolCalls`.

### WP5: Shim `install-self`/`copy` and inline pod packaging

- **Depends on:** WP1.
- **Files:** create `cmd/mayfly-plugin-shim/main.go`,
  `cmd/mayfly-plugin-shim/main_test.go`; modify `internal/k8s/spec.go`,
  `internal/k8s/spec_test.go`, `Dockerfile`, `Makefile`.
- **Behavior:** `install-self` and `copy` per
  [The shim](#the-shim-replaces-the-v1-copier); `serve` and `probe` exit 2
  with "not implemented" until WP11. Inline pod additions per
  [`internal/k8s`](#internalk8s); `SessionSpec.Validate` checks that
  `PluginVolumeLimit` parses and `ToolsImage` is set when inline plugins are
  present. No NetworkPolicy change. `Dockerfile`: build
  `./cmd/mayfly-plugin-shim`, install it at `/usr/local/bin/mayfly-plugin-shim`
  in the `mayfly-agent` stage, `RUN mkdir -p /opt/mayfly/plugins` before
  `USER`. `Makefile`: add `mayfly-plugin-shim` to `CMDS`.
- **Acceptance:**
  - A spec without plugins renders exactly today's pod (golden), and so does
    a spec with only service plugins.
  - A spec with inline plugins renders the init containers in order with
    the hardened security context, the volumes, the mounts, and projected
    secrets with mode 0400.
  - The copier copies a tree, keeps the executable bit, and rejects an
    escaping symlink.
  - `make image` builds and the agent image contains the shim.
- **Tests:** `TestPodWithoutPluginsUnchanged`, `TestPodServiceOnlyUnchanged`,
  `TestPodPluginInitContainers`, `TestPodPluginSecretsProjected`,
  `TestShimCopyTree`, `TestShimCopyRejectsEscapingSymlink`,
  `TestShimInstallSelf`.

### WP6: MCP client changes and inline plugin host

- **Depends on:** WP1.
- **Files:** modify `internal/mcp/stdio.go`, `internal/mcp/mcp.go`,
  `internal/mcp/http.go`, `internal/mcp/mcp_test.go`; create
  `internal/pluginhost/host.go`, `internal/pluginhost/env.go`,
  `internal/pluginhost/stderr.go`, `internal/pluginhost/host_test.go`,
  `internal/pluginhost/testdata/fixture/main.go`.
- **Behavior:** [MCP client changes](#mcp-client-changes-internalmcp)
  (except the WP13 socket option) and the inline part of
  [Runtime host](#runtime-host-internalpluginhost). The fixture is a small
  stdio MCP server on the existing JSON-RPC shapes (no new module
  dependency), configured by env: tools to report, sleep in a tool, exit
  after N calls, stderr output, a file that records each call's arguments
  and `_meta`, and a per-`mayfly.io/session` counter tool. WP11, WP12, and
  WP14 reuse it.
- **Acceptance:**
  - The plugin environment has exactly the listed variables;
    `MAYFLY_SESSION_TOKEN` set in the parent is absent.
  - A missing declared tool fails start; an undeclared tool is hidden and
    listed by `Hidden()`; a non-object schema fails start; `RequiredErr`
    reports a required failure.
  - A crash between calls restarts the plugin on the next call, bounded by
    `MaxRestarts`; the error after that names the plugin and has stderr.
  - A crash during a call returns an error result and does not retry.
  - A timeout sends `notifications/cancelled` (recorded by the fixture) and
    a late response does not corrupt the next call.
  - `_meta` keys reach the server; stderr is capped with one marker;
    `Close` stops a plugin that ignores stdin EOF in about 4s and kills one
    that ignores SIGTERM.
  - `TestStdioClient` and `TestManagerNamesTools` still pass;
    `NewHTTPWithClient` uses the given client and per-request hook.
- **Tests:** `TestHostEnvExact`, `TestHostVerifyTools`, `TestHostRestart`,
  `TestHostCrashDuringCall`, `TestHostTimeoutCancels`, `TestHostMeta`,
  `TestHostStderrCap`, `TestHostClose`, `TestHTTPWithClientHeaders`.

### WP7: Session resolution, grants, local secrets

- **Depends on:** WP2, WP3, WP5.
- **Files:** modify `internal/session/manager.go` (`resolvePlugins`,
  `copySession`, `prepare`, grant registration on create and adopt),
  `internal/session/lifecycle.go` (finalize calls `gateway.Drop`),
  `internal/session/restart.go`, `internal/session/manager_test.go`,
  `internal/session/restart_test.go`, `internal/session/adopt_test.go`,
  `internal/runner/runner.go`, `internal/runner/local.go`,
  `internal/runner/local_test.go`, `internal/model/model.go`,
  `internal/config/config.go` and `internal/config/config_test.go`
  (`runtime.plugin_volume_limit`, default `512Mi`), and one line in
  `cmd/mayflyd/main.go` (`mgr.SetGateway(gw)`). That line is the only overlap
  with WP4 in wave 3: merge WP4 first, then WP7 rebases.
- **Behavior:** `(*Manager).SetGateway(g GrantRegistry)` where
  `GrantRegistry` is the `Register`/`Drop` subset of `*gateway.Gateway`
  (nil means no plugins and no gateway routes in use, for existing tests).
  [Session creation and adoption](#session-creation-and-adoption-internalsession)
  for inline and service plugins; sidecar-as-inline on the local backend;
  local secrets. `resolvePlugins` runs inside `resolveRequest`, so
  `Manager.Validate` (used by webhooks before accept) catches a missing
  local secret. `prepare` fills `Plugins`, `ToolsImage`
  (`cfg.Runtime.Image`), and `PluginVolumeLimit`.
- **Acceptance:**
  - A Kubernetes session with two inline plugins and one service plugin
    writes a `session.json` with Kubernetes paths, no secret values, and the
    service entry with tools only; its pod has the inline init containers.
  - The grant for that session lists exactly the declared tools.
  - A replacement pod and an adopted session have identical plugin pod
    sections and the same grant; a `pending` ledger record from before the
    restart is answered per the resume table.
  - Finalize drops the grant and the ledger records.
  - Local backend: secret files 0600; a missing variable fails `Create` and
    `Validate` with `plugin %q: environment variable %s is not set`.
  - Requests that set `mcp_servers` do not change plugins.
- **Tests:** `TestCreateResolvesPlugins`,
  `TestCreatePluginSessionJSONHasNoSecrets`, `TestCreateRegistersGrant`,
  `TestRestartKeepsPlugins`, `TestAdoptRebuildsGrant`,
  `TestFinalizeDropsLedger`, `TestLocalWritesPluginSecrets`,
  `TestLocalMissingPluginSecret`, `TestLocalSidecarRunsInline`,
  `TestConfigPluginVolumeLimit`.

### WP8: Runtime integration through the gateway

- **Depends on:** WP4, WP6, WP7.
- **Files:** modify `internal/agent/tools.go`, `internal/agent/runtime.go`,
  `internal/agent/steps.go`, `internal/agent/finalize.go`, the matching
  `internal/agent/*_test.go`, `internal/bundle/bundle.go`; create
  `test/integration/plugin_test.go`; modify `cmd/llmstub/main.go` or
  `internal/stub/llm.go` only if the stub cannot script `plugin_*` calls.
- **Behavior:** [Runtime integration](#runtime-integration-internalagent)
  for inline plugins, stdio `mcp.servers` (local dispatch), and any tool
  `Tools()` reports with a remote placement (remote dispatch is just a
  `result` answer). `mcp.Start` skips an HTTP server only when `Tools()`
  lists its tools, so HTTP servers stay on the broker proxy until WP9
  merges, with no change in this package.
- **Acceptance:**
  - A local-backend session calls an inline plugin tool; the audit has
    begin and done; the transcript has the result.
  - Integration: SIGKILL the agent while a non-idempotent plugin tool
    sleeps; the replacement does not call it again (fixture call log) and
    the transcript has the interrupted message and the `system` event.
  - The same with an idempotent tool: called again with the same key.
  - Kill after `CompleteCall` and before the journal append (test hook):
    the replacement does not call the tool, and the step has
    `replayed: true`.
  - A stdio `mcp.servers` tool goes through begin and complete.
  - The gateway down for less than the budget: the call proceeds after
    recovery; longer: the runtime exits non-zero and the replacement
    resumes.
- **Tests:** unit: `TestExecuteToolDispatch`, `TestExecuteToolReplayed`,
  `TestExecuteToolInterrupted`, `TestExecuteToolRateLimited`,
  `TestGatewayRetryBudget`; integration: `TestPluginResumeDoesNotRepeatEffect`,
  `TestPluginResumeRepeatsIdempotent`, `TestPluginReplaysCompletedCall`,
  `TestStdioMCPThroughGateway`.

### WP9: Gateway remote dispatch and the `mcp.servers` fold

- **Depends on:** WP4, WP6, WP7.
- **Files:** create `internal/gateway/remote.go`,
  `internal/gateway/remote_test.go`; modify `internal/gateway/gateway.go`
  (enable remote dispatch and remote discovery), `internal/broker/broker.go`
  (`HTTPClient()`), `internal/broker/broker_test.go`,
  `internal/config/config.go` and `internal/config/config_test.go`
  (`mcp.servers[].tools`, `idempotent_tools`), `internal/mcp/manager.go`
  (stdio allowlist), `internal/session/manager.go` (copy the two lists into
  `sessioncfg.MCPServer` in `resolveRequest`).
- **Behavior:** remote dispatch per [Gateway behavior](#gateway-behavior):
  one upstream client per (session, server) built with
  `mcp.NewHTTPWithClient`, the broker's client, and `ResolvedHeaders`;
  per-session discovery; `ServeMCP` unchanged. Service plugins resolve
  through an `Options.ServiceURL` that returns an error until WP12.
- **Acceptance:**
  - An HTTP `mcp.servers` call reaches an in-process streamable HTTP MCP
    server with the configured headers and the `_meta` idempotency key; the
    runtime never receives the headers.
  - The `tools` allowlist hides other tools from discovery and returns 403.
  - An in-flight call to a tool not in `idempotent_tools` is not repeated on
    resume; a listed one is.
  - Upstream clients close on finalize.
  - Alternate harness sessions still use `/internal/v1/mcp/{server}`.
  - After WP8 and WP9, the competitor-watch example works with no config
    change and its Slack post is no longer repeated on resume.
- **Tests:** `TestRemoteCallInjectsHeaders`, `TestRemoteAllowlist`,
  `TestRemoteResumeNonIdempotent`, `TestRemoteClosesOnFinalize`,
  `TestBrokerProxyUnchanged`, `TestConfigMCPToolLists`.

### WP10: CLI and examples

- **Depends on:** WP1, WP6.
- **Files:** create `cmd/mayfly/plugin.go`, `cmd/mayfly/plugin_test.go`,
  `cmd/mayfly/plugintemplates/go/{plugin.yaml,main.go,go.mod,Dockerfile,.dockerignore}.tmpl`,
  `cmd/mayfly/plugintemplates/python/{plugin.yaml,server.py,requirements.txt,Dockerfile,.dockerignore}.tmpl`,
  `cmd/mayfly/plugintemplates/ts/{plugin.yaml,src/index.ts,package.json,tsconfig.json,Dockerfile,.dockerignore}.tmpl`,
  `examples/plugins/notify/`, `examples/plugins/tickets/`,
  `examples/plugins/word-count/`; modify `cmd/mayfly/main.go` (dispatch and
  `usage`).
- **Behavior:** `init`, `check`, and `test` as in
  [the user docs](../plugins.md#local-testing). They ignore placement and
  run the stdio server locally through `pluginhost`. `init` writes
  `placement: inline`. The examples start as `placement: inline`; WP12
  switches `tickets` to `service` and WP13 switches `notify` to `sidecar`,
  so the examples load at every step. Plugin commands need no `MAYFLY_URL`
  or token.
  Every scaffold's sample tool reads `_meta["mayfly.io/idempotency-key"]`
  and shows forwarding it as an `Idempotency-Key` header, with a comment
  explaining when to declare `idempotent: true`. The sample manifest leaves
  the tool at `idempotent: false`.
- **Acceptance:**
  - `init` then `check` passes for each language where the toolchain exists
    (Go always in CI; Python and Node tests skip without `python3` or
    `node`).
  - `test` against `examples/plugins/word-count` prints the expected count.
  - `test` exits 1 for an unknown tool, invalid JSON, a missing secret, and
    an error result.
  - Each generated sample tool forwards the idempotency key from `_meta`.
- **Tests:** `TestInitWritesValidManifest`, `TestInitRefusesNonEmptyDir`,
  `TestCheckGoTemplate`, `TestPluginTestWordCount`, `TestPluginTestErrors`.

### WP11: Shim `serve`, `probe`, call tokens

- **Depends on:** WP3 (`internal/gateway/token.go`), WP5, WP6 (fixture).
- **Files:** create `cmd/mayfly-plugin-shim/serve.go`,
  `cmd/mayfly-plugin-shim/serve_test.go`; modify
  `cmd/mayfly-plugin-shim/main.go` (dispatch).
- **Behavior:** [Shim serve](#shim-serve-cmdmayfly-plugin-shim) and the
  verify side of [Call tokens](#call-tokens), including replay dedupe.
  Listens on `unix:` (removes a stale socket file, mode 0660) or `tcp:`.
- **Acceptance:**
  - With the fixture as the child, `tools/list` with a discovery token works
    and without one gets 401.
  - `tools/call` with a valid call token works; mismatched tool or argument
    hash, expired, or wrong key → 401; the same key twice concurrently →
    409; the same key after completion returns the cached result without
    calling the child.
  - 16 concurrent calls with distinct keys each get their own result.
  - A child crash fails in-flight calls with the stderr tail and restarts
    the child; `probe` fails while it is down and passes after.
  - The child lacks `MAYFLY_SESSION_TOKEN` when it is set for the shim.
- **Tests:** `TestServeDiscoveryToken`, `TestServeCallToken`,
  `TestServeReplayDedupe`, `TestServeMultiplex`, `TestServeChildRestart`,
  `TestServeEnv`, `TestProbe`.

### WP12: Service reconciler, RBAC, endpoint mode, local children

- **Depends on:** WP9, WP11.
- **Files:** create `internal/pluginsvc/reconcile.go`,
  `internal/pluginsvc/local.go`, `internal/pluginsvc/reconcile_test.go`;
  modify `internal/k8s/client.go` and `internal/k8s/client_test.go` (plugin
  object helpers in `plugins.namespace`), `cmd/mayflyd/main.go` (start the
  reconciler or local children; pass `ServiceURL` to the gateway),
  `deploy/k8s/base/rbac.yaml`, `deploy/helm/mayfly/templates/rbac.yaml`,
  `examples/plugins/tickets/plugin.yaml` (switch to `placement: service`).
- **Behavior:** [Service reconciler](#service-reconciler-internalpluginsvc)
  and the service decision. `service.mcpServer` resolves to the
  `mcp.servers` entry through the WP9 remote path under `plugin_` names.
- **Acceptance:**
  - Fake clientset: reconcile creates the four objects with labels, hash,
    hardened pod, `secretKeyRef` env, resources, and replicas; an unchanged
    manifest writes nothing; a changed one updates; a removed plugin is
    pruned, and is kept with `prune: false`.
  - `GarbageCollect` and `OrphanSessions` ignore plugin objects.
  - The NetworkPolicy admits only `runtime.platform_labels` pods in
    `runtime.platform_namespace` and allows egress only to DNS and the
    manifest CIDRs.
  - A gateway call to a service plugin (an `httptest` server running the
    real shim handler in front of the fixture) carries a valid token and
    `_meta` session and key.
  - A `service.mcpServer` plugin calls the configured server with its
    headers under `plugin_` names, and nothing is reconciled for it.
  - Local backend: a service plugin runs as a shim child on a socket in the
    state directory and stops on shutdown.
  - The RBAC files grant the new verbs; `helm template` renders them, and
    renders the Role in `plugins.namespace` when it is set.
- **Tests:** `TestReconcileCreates`, `TestReconcileNoop`,
  `TestReconcileUpdates`, `TestReconcilePrune`, `TestGCIgnoresPluginObjects`,
  `TestServiceNetworkPolicy`, `TestGatewayServiceCall`,
  `TestServiceMCPServerRef`, `TestLocalServiceChild`.

### WP13: Sidecar pod spec and UDS dispatch

- **Depends on:** WP8, WP11, WP12.
- **Files:** modify `internal/k8s/spec.go`, `internal/k8s/spec_test.go`
  (sidecar containers, per-plugin socket volumes, `gateway-key-<name>`),
  `internal/k8s/client.go` (`SessionKeys`), `internal/session/manager.go`
  (key generation; adoption reads it), `internal/session/adopt_test.go`,
  `internal/gateway/gateway.go` (sidecar tokens in dispatch and discovery),
  `internal/mcp/http.go` (socket dialer), `internal/agent/runtime.go`
  (start sidecar members), `internal/sessioncfg/config.go` (only if WP1's
  sidecar validation needs adjusting); create
  `internal/pluginhost/sidecar.go` and `internal/pluginhost/sidecar_test.go`;
  modify `examples/plugins/notify/plugin.yaml` (switch to
  `placement: sidecar`).
- **Behavior:** sidecar pod additions per [`internal/k8s`](#internalk8s);
  the runtime connects to each socket with the discovery token, verifies
  tools, and dispatches with call tokens.
- **Acceptance:**
  - Pod: one native sidecar per plugin after `mayfly-tools`, with
    `restartPolicy: Always`, a startup probe, manifest resources and UID,
    `secretKeyRef` env, its own socket volume, and only its own
    `gateway-key-<name>` item mounted. The agent container has no env or
    mount for sidecar secrets and no `gateway-key-*` item.
  - No two sidecars share a key file or a socket volume, and a token signed
    for one plugin fails the signature check at another.
  - `session.json` for a sidecar plugin holds no secret values.
  - A replacement pod and an adopted session use the same gateway key.
  - The runtime calls the sidecar through the socket with a call token.
- **Tests:** `TestPodSidecarContainers`, `TestPodGatewayKeyOnlyInSidecars`,
  `TestAdoptKeepsGatewayKey`, `TestSidecarClientTokens`,
  `TestExecuteToolSidecar`.

### WP14: End-to-end tests on k3d

- **Depends on:** WP8 and WP9 (phase A subtests), WP12 (phase B), WP13
  (phase C).
- **Files:** create `e2e/plugin_test.go`, `e2e/plugins/` (bundle files and
  the fixture image source); modify `e2e/main_test.go` (build, import into
  k3d, allowlist the image), `e2e/values.yaml`, `e2e/agents.yaml`,
  `e2e/manifests/stubs.yaml` if a stub is needed, and the Make targets that
  build e2e images.
- **Behavior:** real pods and Deployments on k3d
  (`rancher/k3s:v1.35.5-k3s1`).
- **Acceptance:**
  - A: an inline session completes; the pod has the init containers and a
    read-only plugin mount; the plugin env has no `MAYFLY_SESSION_TOKEN`;
    deleting the pod during a non-idempotent call does not repeat it; an
    HTTP `mcp.servers` stub call carries an idempotency key; an image
    without `/mayfly-plugin/` fails the session at start.
  - B: the reconciler creates the service; two sessions call it and share
    one replica's state (fixture counter keyed by `_meta` session); a
    session pod cannot reach the service directly; the service reaches only
    its egress CIDR.
  - C: a sidecar session completes; the agent container has no sidecar
    secret; an unauthenticated socket call from `run_command` gets 401.
  - `TestNetworkIsolation` still passes.
- **Tests:** `TestPluginInlineSession`, `TestPluginEnvIsolated`,
  `TestPluginResumeAfterPodDelete`, `TestPluginMissingDirFailsSession`,
  `TestMCPServerIdempotencyKey`, `TestPluginServiceShared`,
  `TestPluginServiceNetworkPolicy`, `TestPluginSidecarSession`,
  `TestPluginSidecarSocketRequiresToken`. Run with `make e2e` and
  `E2E_QUEUE=redis make e2e`.

### WP15: Documentation

- **Depends on:** all others merged.
- **Files:** modify `docs/plugins.md` (remove the status line; correct
  drift), `docs/agents.md` (`plugins` field; "agent manifest" wording;
  remove "This is the only plugin surface" and the Limits lines about custom
  tools), `docs/mcp-and-skills.md` (gateway, `tools`, `idempotent_tools`,
  the resume change, the `plugin_` prefix), `docs/security.md` (plugin
  trust, where secrets live per placement, call tokens, the `allowShell`
  rule, service NetworkPolicy, what is not solved),
  `docs/configuration.md` (`plugins:`, `runtime.plugin_volume_limit`,
  `mcp.servers` keys), `docs/deployment.md` (RBAC additions,
  `plugins.namespace`), `docs/architecture-gaps.md` (8.2 closed for plugins
  and `mcp.servers` under the built-in harness, open for alternate
  harnesses), `docs/architecture.md` (gateway and session start),
  `docs/api.md` (internal tool routes), `docs/testing.md`,
  `ARCHITECTURE.md` (code map entries for `internal/plugin`,
  `internal/gateway`, `internal/pluginhost`, `internal/pluginsvc`,
  `cmd/mayfly-plugin-shim`; the invariant on tool calls and the ledger),
  `README.md` (drop "planned"), `deploy/helm/mayfly/values.yaml`
  (`plugins:`, `plugin_volume_limit`), `CHANGELOG.md` (the feature and the
  `mcp.servers` resume change).
- **Behavior:** docs describe what shipped, in the existing tone.
- **Acceptance:** every path, key, flag, route, and tool name in the docs
  exists in the code; `-check` output in `docs/sources.md` matches.
- **Tests:** a path and key check (`rg` for every `internal/...`,
  `docs/...`, and config key the docs mention).

## Open questions

Changed from v1: 1, 4, 6, 7, 8, 9, 11, 12, 13, 14, 15 are new or rewritten.
v1's questions on sidecars and per-plugin resource limits are settled by the
placements.

1. **Sidecar over localhost TCP as an option.** Recommendation: not in v2.
   Call tokens would make TCP acceptable, but the socket limits reach to
   containers that mount the volume and costs nothing. Add
   `sidecar.listen: tcp` only if an image cannot use Unix sockets.
2. **Can API requests or triggers select plugins?** Recommendation: no. The
   agent manifest is the reviewed place to grant tools with effects.
3. **Per-agent tool subsets (gap 8.3).** Recommendation: later, as
   `plugins: [{name: notify, tools: [post]}]`. The grant already holds a
   per-session tool set, so this is a loader and `GrantFor` change only.
4. **Default idempotency for `mcp.servers` tools.** v2 treats them as
   non-idempotent, so an in-flight call is no longer repeated on resume.
   Recommendation: ship it as the default with a CHANGELOG note. Repeating
   an unknown effect is the worse failure; operators list safe tools in
   `idempotent_tools`.
5. **Separate image allowlist for plugins.** Recommendation: reuse
   `agents.allowed_images`, including for service Deployments.
6. **Hostname egress for plugins.** NetworkPolicy matches CIDRs, and SaaS
   APIs have no stable ones. Services already isolate egress by CIDR, which
   covers internal APIs. Recommendation: later, an egress proxy in
   `mayflyd` with a per-plugin hostname allowlist, used by sidecar and
   service plugins through `HTTPS_PROXY`.
7. **Reject or warn on `allowShell` with inline secrets.** Recommendation:
   reject, as in v1, now with an error that suggests `placement: sidecar`.
8. **Gateway key rotation for services.** Reconcile creates the key once.
   Recommendation: later, `mayflyd plugin rotate-key <name>`, with the shim
   accepting two `--key-file` values during the switch.
9. **Cross-session discovery cache for remote servers.** Recommendation:
   per-session in v2, which stays correct when a server's tools depend on
   credentials or change. Add a TTL cache keyed by server only if
   `tools/list` latency shows up in session start times.
10. **Image volumes instead of copying (inline).** Recommendation: add
    `runtime.plugin_packaging: copy|image-volume` once the minimum supported
    Kubernetes is 1.36; keep `copy` the default.
11. **Service autoscaling.** Recommendation: v2 owns `replicas`. Later,
    `service.autoscale: {min, max, cpu}` renders an HPA and reconcile stops
    writing `replicas`.
12. **The `mergeEnv` leak.** Resolved. Stdio `mcp.servers` used to inherit
    `os.Environ()`, including `MAYFLY_SESSION_TOKEN`, which with the gateway
    also authorizes tool calls. They now use `sessioncfg.StdioEnv`, the same
    allowlist as an inline plugin, and `NewStdioExact` with that environment.
    This is a behavior change for a server that depended on inheriting an
    unrelated variable; set it in the entry's `env` instead.
13. **Arguments in the ledger.** v2 stores only their hash. Recommendation:
    keep it that way. The journal already holds arguments for replay; the
    ledger should not become a second copy of sensitive inputs.
14. **Plugins for alternate harnesses.** They bypass the gateway through the
    byte proxy. Recommendation: later, expose the gateway as a per-session
    streamable HTTP MCP endpoint (`/internal/v1/sessions/{id}/mcp`) that any
    harness can use, with idempotency keys from a gateway call counter
    instead of journal steps.
15. **`mayfly plugin test --shim`.** Running the plugin behind the shim
    locally would exercise tokens and multiplexing and catch stdout logging
    bugs. Recommendation: add it after WP11; it is small.
16. **Name of the feature in docs.** Recommendation: "plugin" means this
    feature; agent manifests are "agent manifests" (WP15).
