# Plugins

A plugin adds custom tools to a session. It is an MCP server, written in any
language, declared in the project bundle. Mayfly speaks plain MCP to it. There
is no plugin SDK and no Mayfly-specific protocol: a stdio server written for
any MCP client works unchanged.

Each plugin picks where it runs with `placement`:

- `inline`: a child process of the agent runtime, inside the agent container.
- `sidecar`: its own container in the session pod.
- `service`: a long-lived Deployment that `mayflyd` runs and shares across
  sessions.

The agent sees the same tool names, `plugin_<plugin>_<tool>`, whatever the
placement. Every call goes through the tool gateway in `mayflyd`, which checks
that the session may use the tool, records the call, attaches an idempotency
key, enforces timeouts and rate limits, and writes the audit log.

Use a plugin when a tool needs code: calling an internal API, posting to a
chat service, parsing a file format, or wrapping a CLI. Use a skill when the
agent only needs instructions.

## Choose a placement

| | `inline` | `sidecar` | `service` |
|---|---|---|---|
| Runs as | Child process of the agent runtime | Container in the session pod | Deployment and Service, shared by sessions |
| Started | When the session container starts | With the session pod | When `mayflyd` reconciles the bundle |
| State | Per container; lost on a replacement container | Per session pod | Shared and long-lived: caches, connection pools, warm models |
| Start cost per session | A process start | A container start (image pull, then seconds) | None |
| Secrets | Files in the agent container | In the sidecar only | In the service only |
| With `allowShell` | Rejected if the plugin has secrets | Allowed | Allowed |
| CPU and memory | Shares the agent container's limits | Its own requests and limits | Its own requests, limits, and replicas |
| Network | Session pod policy (`runtime.egress`) | Session pod policy (`runtime.egress`) | Its own NetworkPolicy and egress allowlist |
| Workspace access | Yes | Optional mount | No |
| Local backend | Yes | Runs as `inline`, so its secrets move into the agent process | Child process of `mayflyd` |

Choose:

- **`inline`** for small, fast tools without credentials, or when the agent
  has no shell: file format parsers, calculators, local CLIs, tools that read
  or write the workspace. It is the default and the cheapest.
- **`sidecar`** when a per-session tool needs a credential the agent must not
  be able to read, needs its own CPU or memory, or needs a runtime that does
  not fit in the agent container: a browser, a headless office suite, a
  language server.
- **`service`** when start-up is expensive or the tool benefits from shared
  state: a search index, a cache in front of a rate-limited API, a pool of
  database connections, anything that needs its own egress rules. Also for an
  MCP server you already run, through `service.mcpServer`.

## How it runs

1. The bundle declares the plugin in `plugins/<name>/plugin.yaml`. An agent
   lists it under `plugins`.
2. `mayflyd` validates the manifest at startup. `mayflyd -check` prints it.
   For `service` plugins, `mayflyd` creates or updates the Deployment,
   Service, and NetworkPolicy.
3. When a session starts, the gateway records which tools the session may
   call. For `inline` and `sidecar`, the session pod gets the plugin's files
   or container.
4. The runtime lists the tools: inline and sidecar tools from the plugin
   itself, service tools from the gateway. It checks them against the
   manifest.
5. When the model calls a plugin tool, the runtime asks the gateway. For a
   service tool the gateway makes the call. For an inline or sidecar tool the
   gateway authorizes it and the runtime makes the call locally, then reports
   the result back to the gateway.
6. When the session ends, inline and sidecar plugins stop with it. Services
   keep running.

## Quickstart: an inline plugin in Python

Scaffold a plugin inside the bundle directory:

```bash
mayfly plugin init --lang python plugins/word-stats
```

That writes `plugin.yaml`, `server.py`, `requirements.txt`, a `Dockerfile`, and
a `.dockerignore`. The server uses only the Python standard library, so
`requirements.txt` stays empty. Stdout carries the protocol; write logs to
stderr.

The scaffold exposes one `send` tool. To count words instead, replace the
scaffold's `send` entry in `HANDLERS` with this handler:

```python
# plugins/word-stats/server.py
import os
import pathlib


def count_words(arguments, meta):
    """Count the words in a workspace file."""
    root = pathlib.Path(os.environ["MAYFLY_WORKSPACE_DIR"]).resolve()
    target = (root / arguments["path"]).resolve()
    if root not in target.parents:
        raise ValueError("path must stay inside the workspace")
    return str(len(target.read_text().split()))
```

Declare the same tool in `TOOLS`:

```python
    {
        "name": "count_words",
        "description": "Count words in a workspace file.",
        "inputSchema": {
            "type": "object",
            "properties": {"path": {"type": "string"}},
            "required": ["path"],
        },
    },
```

```yaml
# plugins/word-stats/plugin.yaml
schemaVersion: 1
name: word-stats
version: 0.1.0
placement: inline
image: ghcr.io/acme/mayfly-plugin-word-stats:0.1.0
command: server/server
local:
  command: python3
  args: [server.py]
tools:
  - name: count_words
    idempotent: true
```

The agent image does not include Python, so an inline Python plugin carries
its own interpreter. The scaffolded `Dockerfile` freezes the server with
PyInstaller on Debian bookworm, the same base as the agent image:

```dockerfile
FROM python:3.12-slim-bookworm AS build
WORKDIR /src
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt pyinstaller
COPY server.py .
RUN pyinstaller --onedir --name server server.py

FROM scratch
COPY --from=build /src/dist/server /mayfly-plugin/server
```

Test one tool without a cluster, then build and push the image:

```bash
mayfly plugin test plugins/word-stats count_words '{"path":"report.md"}' --workspace .
docker build -t ghcr.io/acme/mayfly-plugin-word-stats:0.1.0 plugins/word-stats
docker push ghcr.io/acme/mayfly-plugin-word-stats:0.1.0
```

Enable it for an agent and restart `mayflyd`:

```yaml
agents:
  - name: writer
    systemPrompt: ...
    plugins: [word-stats]
```

Sessions for `writer` now have a `plugin_word_stats_count_words` tool.

## Quickstart: a sidecar plugin in Go

This plugin posts to a team webhook. The webhook URL is a credential, so the
plugin runs as a sidecar: the URL is in the sidecar's environment and never in
the agent container.

```bash
mayfly plugin init --lang go plugins/notify
```

The scaffold is a standard-library-only stdio server with one `send` tool.
[examples/plugins/notify](../examples/plugins/notify) is the complete webhook
plugin; its handler:

```go
// plugins/notify/main.go (excerpt)
func post(text string, meta map[string]any) (string, error) {
	target := os.Getenv("NOTIFY_WEBHOOK_URL")
	if target == "" {
		return "", fmt.Errorf("NOTIFY_WEBHOOK_URL is not set")
	}
	key, _ := meta["mayfly.io/idempotency-key"].(string)
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return fmt.Sprintf("posted (status %d)", resp.StatusCode), nil
}
```

A plugin is a plain stdio MCP server: JSON-RPC on stdin and stdout, logs on
stderr, with `initialize`, `tools/list`, and `tools/call`. No SDK is required.
Mayfly runs the process behind a small adapter in the sidecar, so the same
image works in every placement.

```yaml
# plugins/notify/plugin.yaml
schemaVersion: 1
name: notify
version: 0.1.0
placement: sidecar
image: ghcr.io/acme/mayfly-plugin-notify:0.1.0
command: bin/notify
local:
  command: go
  args: [run, .]
env:
  NOTIFY_CHANNEL: "#ops"
secrets:
  - env: NOTIFY_WEBHOOK_URL
    secret: mayfly-plugin-notify
    key: webhook-url
tools:
  - name: post
    idempotent: false
    timeout: 30s
sidecar:
  resources:
    requests: {cpu: 10m, memory: 32Mi}
    limits: {memory: 64Mi}
```

```dockerfile
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/notify .

FROM scratch
COPY --from=build /out/notify /mayfly-plugin/bin/notify
```

```bash
kubectl -n mayfly create secret generic mayfly-plugin-notify --from-literal=webhook-url=https://hooks.example.com/...
```

The webhook host must be reachable from the session pod, so add a
`runtime.egress` rule for it. `idempotent: false` matters on resume; see
[Resume and idempotency](#resume-and-idempotency).

## Quickstart: a service plugin

This plugin answers ticket lookups from a cache shared by every session, and
is the only workload allowed to reach the ticket API.

```yaml
# plugins/tickets/plugin.yaml
schemaVersion: 1
name: tickets
version: 0.1.0
placement: service
image: ghcr.io/acme/mayfly-plugin-tickets:0.3.0
command: bin/tickets
env:
  TICKETS_URL: https://tickets.internal.example.com/api
  CACHE_TTL: 10m
secrets:
  - env: TICKETS_TOKEN
    secret: mayfly-plugin-tickets
    key: token
tools:
  - name: get_ticket
    idempotent: true
  - name: add_comment
    idempotent: false
limits:
  rate: {requests: 30, interval: 1m}
service:
  replicas: 2
  concurrency: 16
  resources:
    requests: {cpu: 50m, memory: 128Mi}
    limits: {memory: 256Mi}
  egress:
    - cidr: 10.20.0.0/16
      ports: [443]
```

`mayflyd` creates `Deployment/mayfly-plugin-tickets`,
`Service/mayfly-plugin-tickets`, and `NetworkPolicy/mayfly-plugin-tickets` in
`plugins.namespace`, plus `Secret/mayfly-gateway-tickets`, which holds the key
that signs call tokens for the service. The NetworkPolicy admits traffic only
from `mayflyd` and allows egress only to DNS and `service.egress`. Session pods
never reach the service; the gateway calls it.

A service is shared, so per-session data must be keyed on the session ID that
Mayfly sends with every call (`_meta["mayfly.io/session"]`).

### An MCP server you already run

Point a service plugin at an `mcp.servers` entry. Mayfly creates nothing; the
URL and credentials stay in `config.yaml`, and the manifest adds the tool
declarations, idempotency flags, and `plugin_` names:

```yaml
# config.yaml
mcp:
  servers:
    - name: crm
      transport: http
      url: https://crm-mcp.internal.example.com/mcp
      headers:
        Authorization: "env:CRM_MCP_TOKEN"
      opt_in: true
```

```yaml
# plugins/crm/plugin.yaml
schemaVersion: 1
name: crm
placement: service
service:
  mcpServer: crm
tools:
  - name: find_account
    idempotent: true
```

## Bundle layout

```text
bundle/
  agents.yaml
  plugins/
    word-stats/
      plugin.yaml
      server.py
      requirements.txt
      Dockerfile
```

The loader reads `plugin.yaml` from each directory directly under `plugins/`
and skips every other file under `plugins/`, so source files such as
`package.json` are not parsed as bundle files. The directory name must equal
the manifest `name`.

A ConfigMap cannot hold directories. For a bundle mounted from a ConfigMap,
name the file `<name>.plugin.yaml` instead, for example
`word-stats.plugin.yaml`. It may sit anywhere in the bundle.

The plugin's code is not read from the bundle. It comes from `image`.

## Manifest reference

```yaml
schemaVersion: 1
name: notify
version: 0.1.0
description: Post messages to the team channel.
scope: shared
placement: sidecar
image: ghcr.io/acme/mayfly-plugin-notify@sha256:...
command: bin/notify
args: []
local:
  command: go
  args: [run, .]
required: false
env:
  NOTIFY_CHANNEL: "#ops"
secrets:
  - env: NOTIFY_WEBHOOK_URL
    secret: mayfly-plugin-notify
    key: webhook-url
tools:
  - name: post
    idempotent: false
    timeout: 30s
  - name: recent
    idempotent: true
limits:
  initTimeout: 30s
  callTimeout: 120s
  maxOutputBytes: 65536
  rate: {requests: 20, interval: 1m, burst: 5}
inline:
  workdir: plugin
  maxRestarts: 2
  stderrBytes: 1048576
sidecar:
  resources: {requests: {cpu: 10m, memory: 32Mi}, limits: {memory: 64Mi}}
  runAsUser: 65532
  workspace: none
service:
  replicas: 1
  concurrency: 8
  resources: {requests: {cpu: 50m, memory: 128Mi}, limits: {memory: 256Mi}}
  egress: [{cidr: 203.0.113.0/24, ports: [443]}]
  transport: stdio
  port: 8080
  path: /mcp
  mcpServer: ""
```

Only the block that matches `placement` may be set.

### Common fields

| Field | Default | Meaning |
|---|---|---|
| `schemaVersion` | required | Must be `1` |
| `name` | required | Plugin name: lowercase letters, digits, dot, underscore, hyphen. Must match the directory, or the `<name>` in `<name>.plugin.yaml` |
| `version`, `description` | empty | Informational only |
| `scope` | `shared` | `shared` or `agent:<name>`. A private plugin is usable only by its owner |
| `placement` | `inline` | `inline`, `sidecar`, or `service` |
| `image` | required, except with `service.mcpServer` | Image that holds the plugin under `/mayfly-plugin/`. Must match `agents.allowed_images`. Pin it by digest |
| `command` | required, except with `service.mcpServer` | Executable. A path with a `/` that is not absolute is relative to the plugin directory. A bare name is looked up on `PATH`, with the plugin's `bin/` first |
| `args` | `[]` | Arguments |
| `local.command`, `local.args` | `command`, `args` | Used by the local backend and `mayfly plugin test`, relative to the manifest directory |
| `required` | `false` | `true` fails the session at start when the plugin is unavailable or its tools do not match. `false` records a `system` event and continues without its tools |
| `env` | `{}` | Literal environment variables. For `inline` and `sidecar` the values land in `session.json` in a ConfigMap; never put secrets here. `env:` values are rejected |
| `secrets[].env` | required | Variable name the plugin process receives |
| `secrets[].secret` | required | Kubernetes Secret name; must start with `mayfly-plugin-`. In `sessions.namespace` for `inline` and `sidecar`, in `plugins.namespace` for `service` |
| `secrets[].key` | required | Key in that Secret |
| `tools[].name` | required | Tool name exactly as the server reports it in `tools/list` |
| `tools[].idempotent` | `false` | `true` when running the tool twice with the same arguments is safe. Controls resume behavior |
| `tools[].timeout` | `limits.callTimeout` | Per-call timeout, at most `15m` |
| `limits.initTimeout` | `30s` | Bound on start, `initialize`, and `tools/list` |
| `limits.callTimeout` | `120s` | Default per-call timeout, at most `15m` |
| `limits.maxOutputBytes` | `65536` | Cap on one tool result. Cannot exceed 64 KiB, the cap for every tool |
| `limits.rate` | none | Calls per session to this plugin: `requests` per `interval`, with `burst` |

Variable names in `env` and `secrets` follow the same rules as `requiredEnv`:
`MAYFLY_*` is reserved, and no name may be a variable the control plane reads
a secret from (`config.SecretEnvNames`). `PATH`, `HOME`, and `TMPDIR` are set
by Mayfly and cannot be declared. Unknown fields are rejected.

### `inline`

| Field | Default | Meaning |
|---|---|---|
| `inline.workdir` | `plugin` | Working directory: `plugin` (the plugin directory, read-only in a pod) or `workspace`. Relative paths in `args` resolve against it |
| `inline.maxRestarts` | `2` | Restarts after the plugin exits, per container |
| `inline.stderrBytes` | `1048576` | Stderr kept in the session bundle per plugin and container |

### `sidecar`

| Field | Default | Meaning |
|---|---|---|
| `sidecar.resources` | none | `requests` and `limits` for the sidecar container |
| `sidecar.runAsUser` | `65532` | UID of the sidecar container. Non-root is enforced |
| `sidecar.workspace` | `none` | `none`, `read`, or `write`: mount the session workspace into the sidecar |

### `service`

| Field | Default | Meaning |
|---|---|---|
| `service.replicas` | `1` | Deployment replicas, `0` to `20`. `0` parks the plugin |
| `service.concurrency` | `8` | Concurrent calls per replica, `1` to `256` |
| `service.resources` | none | `requests` and `limits` for the plugin container |
| `service.egress` | `[]` | CIDRs and ports the service may reach, besides DNS |
| `service.transport` | `stdio` | `stdio`: the plugin is a stdio server and Mayfly's adapter serves it over HTTP. `http`: the plugin serves streamable HTTP MCP itself on `port` and `path`, and Mayfly sets `MAYFLY_GATEWAY_KEY` so it can verify call tokens |
| `service.port`, `service.path` | `8080`, `/mcp` | Where an `http` plugin listens |
| `service.mcpServer` | empty | Name of an `mcp.servers` entry with `transport: http`. Mayfly creates nothing and calls that server; `image`, `command`, `args`, `local`, `env`, `secrets`, and every other `service` field must be empty |

## Enable a plugin for an agent

```yaml
agents:
  - name: competitor-watch
    systemPrompt: ...
    mcpServers: [firecrawl]
    plugins: [notify, tickets]
```

| Rule | Behavior |
|---|---|
| Selection | Only the agent manifest selects plugins. An agent without `plugins` gets none. API requests and triggers cannot add or remove them |
| Visibility | A plugin with `scope: agent:<name>` is usable only by that agent |
| Unknown name | `mayflyd` fails at startup and `-check` fails |
| Harness | Plugins need the built-in harness. An agent with another `harness` and a non-empty `plugins` list fails at startup |
| Shell | An agent with `allowShell: true` cannot use an `inline` plugin that declares `secrets`. `run_command` runs in the same container and could read them. Sidecar and service secrets are not in the agent container |

`mayflyd -check` prints one line per plugin:

```text
plugin notify placement=sidecar scope=shared tools=2 effects=post image=ghcr.io/acme/mayfly-plugin-notify@sha256:...
```

`effects` lists the tools that are not idempotent. A `service.mcpServer`
plugin has `image=-` and ends with `mcpServer=<server>`.

## Tool names

```text
plugin_<plugin>_<tool>
```

Plugin `notify`, tool `post` becomes `plugin_notify_post`. Characters other
than letters and digits become `_`, as for `mcp_<server>_<tool>`. A name
longer than 64 characters, or two tools of one plugin that sanitize to the
same name, fail at startup. The journal records the step as
`tool:plugin_notify_post`.

## Tool checks

When a session starts, Mayfly compares the plugin's `tools/list` with the
manifest:

- A declared tool the server does not report is an error. With
  `required: true` the session fails; otherwise a `system` event names the
  missing tools and the plugin's tools are not offered.
- A tool the server reports but the manifest does not declare is not shown to
  the model, and the gateway refuses calls to it. A `system` event lists the
  hidden tools.
- An input schema that is not a JSON object is an error.

The manifest is therefore also an allowlist: a server with 30 tools exposes
only the ones you declare.

## The tool gateway

The gateway is part of `mayflyd`, next to the credential broker that proxies
model and MCP traffic. The session token is the gateway credential. When a
session is created, the gateway records the exact set of tools it may call;
the token authorizes those and nothing else.

For every call, the gateway:

- checks the tool is in the session's set,
- applies `limits.rate` and the call timeout,
- assigns the idempotency key and records the call in its ledger,
- injects credentials (for `service.mcpServer` and `mcp.servers`),
- writes an audit record: session, agent, plugin, tool, key, a SHA-256 of
  the arguments, duration, and outcome.

What it does depends on the placement:

| Placement | Who makes the call | How |
|---|---|---|
| `inline` | The runtime | The gateway authorizes and records the call, the runtime calls the child process over stdio, then reports the result |
| `sidecar` | The runtime | As `inline`, but the gateway also signs a call token. The runtime sends it to the sidecar over a Unix socket, and the sidecar's adapter rejects a call without a valid token |
| `service` | The gateway | The gateway calls the service over HTTP with a signed call token, or calls the `mcp.servers` entry with its configured headers |

For inline and sidecar calls the plugin process still runs in the pod; the
gateway is asked before and told after. The gateway hashes the arguments and
does not store them. It keeps each result in its ledger until the session
finalizes, so a replacement container can get it back without calling the
tool again. Audit records hold neither arguments nor results.

`mcp.servers` tools use the same gateway with the built-in harness: names stay
`mcp_<server>_<tool>`, and they get the same ledger, resume behavior, rate
limiting, and audit records. See
[mcp-and-skills.md](mcp-and-skills.md) for `tools` and `idempotent_tools` on
`mcp.servers` entries.

## Packaging

A plugin image contains one directory, `/mayfly-plugin/`, with everything the
plugin needs: the executable, scripts, dependencies, and a runtime if needed.
Files must be readable by the UID it runs as. Mayfly never runs the image's
entrypoint, so the image can be `FROM scratch`.

- `inline`: an init container from the plugin image copies `/mayfly-plugin/`
  into a pod volume with a small static copier that Mayfly supplies. The agent
  container mounts it read-only at `/opt/mayfly/plugins/<name>/` and runs the
  plugin with the agent image's libraries and runtimes.
- `sidecar` and `service`: the plugin image is the container image. Mayfly
  copies its adapter into the container and runs it as the command; the
  adapter starts your `command` as a stdio child and serves it to Mayfly.

What the agent image provides to inline plugins:

| Language | Inline image needs |
|---|---|
| Go, Rust, Zig | The static binary |
| TypeScript, JavaScript | Compiled JavaScript and `node_modules`; `command: node`. The agent image has Node 22 |
| Python | A frozen build (PyInstaller) or a relocatable interpreter. The agent image has no Python |
| Anything else | The binary and its libraries, built for Debian bookworm |

A sidecar or service image runs on its own base, so it can be any image that
has `/mayfly-plugin/` and the libraries the plugin needs.

Images are built for the node architecture. Every session pulls inline and
sidecar images, including replacement containers; set
`runtime.image_pull_policy: IfNotPresent`. `runtime.image_pull_secrets`
applies to plugin images too. `runtime.plugin_volume_limit` (default `512Mi`)
bounds the copied files of all inline plugins in one pod.

## Environment and secrets

A plugin process does not inherit the runtime's environment. It receives
only:

| Variable | Value |
|---|---|
| `PATH` | The plugin's `bin/`, then the container default |
| `HOME`, `TMPDIR` | A private directory under `/tmp/plugins/<name>/` |
| `LANG`, `TZ`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, proxy variables | Copied when set in the container |
| `MAYFLY_PLUGIN_NAME`, `MAYFLY_PLUGIN_DIR` | The plugin's name and directory |
| `MAYFLY_SESSION_ID`, `MAYFLY_WORKSPACE_DIR` | Inline and sidecar only: the session ID and workspace path (empty when the sidecar has no workspace mount) |
| `env` entries | As declared |
| `secrets` entries | From the Secret |

It never receives `MAYFLY_SESSION_TOKEN`, `MAYFLY_CONFIG`, the agent's
`runtime.env`, or a control-plane secret. A service plugin gets the session ID
per call in `_meta["mayfly.io/session"]` instead.

Where secrets live:

| Placement | Delivery | Visible to the agent container |
|---|---|---|
| `inline` | Files under `/var/run/mayfly/plugin-secrets/<name>/`, mode 0400, read by the runtime and passed to that plugin only | Yes. Hence the `allowShell` rule |
| `sidecar` | Environment of the sidecar container | No |
| `service` | Environment of the service container | No |
| `service.mcpServer` | Headers resolved in `mayflyd` from `env:NAME` | No |

On the local backend, each secret is read from the `mayflyd` environment
variable named by `secrets[].env`.

## Networking

`inline` and `sidecar` plugins share the session pod's network and its
NetworkPolicy: by default only the control plane and cluster DNS. A plugin
that calls an external API needs a `runtime.egress` rule, which applies to the
whole pod.

A `service` plugin has its own NetworkPolicy: ingress only from `mayflyd`,
egress only to DNS and `service.egress`. Use a service when a plugin needs
network access that agents should not have. Allowlists by hostname are not
supported; NetworkPolicy matches CIDRs.

## Resume and idempotency

Every plugin call is a journaled step and a ledger record in the gateway. When
a replacement container resumes a turn:

| What happened before the container died | `idempotent: true` | `idempotent: false` |
|---|---|---|
| The call finished and its step reached the journal | Not called again | Not called again |
| The call finished but its step did not reach the journal | Not called again; the gateway returns the recorded result | Not called again; the gateway returns the recorded result |
| The call was in flight | Called again with the same idempotency key | Not called again. The model gets an error result: the call was interrupted, may already have taken effect, and was not repeated. A `system` event says the same in the thread |

Each `tools/call` request carries:

```json
{"_meta": {
  "mayfly.io/idempotency-key": "<session>/<turn>/<step>",
  "mayfly.io/session": "<session>",
  "mayfly.io/agent": "<agent>"
}}
```

The key is the same across replacement containers. A tool that passes it to
its target, or deduplicates on it, gets at most one effect even when it is
called twice, and can declare `idempotent: true`.

Declare `idempotent: true` only when a repeat is harmless: reads, upserts
keyed by the arguments, or effects deduplicated on the key. Leave everything
else at the default `false`.

Most third-party MCP servers ignore `_meta` and never forward the key, and
some target APIs (Slack's `chat.postMessage`, for example) have no
idempotency key at all. Do not mark a third-party tool idempotent just because
the API behind it supports keys; check that the server forwards mayfly's key.
For your own plugins, forward it to any API that accepts one (Stripe and many
payment, email, and webhook APIs take an `Idempotency-Key` header), as the
`notify` example above does, then declare `idempotent: true`. The scaffolds
from `mayfly plugin init` include this forwarding.

The same default applies to `mcp.servers`: their tools are non-idempotent
unless listed in `idempotent_tools`. Before this change an interrupted
`mcp.servers` call ran again on resume; now it returns the interrupted error
instead. List read-only and key-forwarding tools in `idempotent_tools` to keep
the old behavior for them.

## Failures and restarts

| Event | Behavior |
|---|---|
| Plugin unavailable at session start (does not start, fails `initialize` or `tools/list`, exceeds `initTimeout`, service has no ready replica) | `required: true` fails the session. Otherwise a `system` event and the session continues without its tools |
| Declared tool missing from `tools/list` | Same as unavailable |
| Inline plugin exits between calls | Restarted on the next call, up to `inline.maxRestarts`. The tool list must match again |
| Sidecar container exits | Kubernetes restarts it with back-off. Calls meanwhile return an error result |
| Plugin exits or the connection drops during a call | Error result with the last stderr lines when known. Not retried |
| Call exceeds its timeout | Mayfly sends `notifications/cancelled` and returns an error result. A late response is discarded |
| Rate limit reached | Error result naming the plugin. The runtime does not retry, and the gateway records no call |
| Gateway unreachable | The runtime retries for up to 5 minutes, then exits so a replacement container resumes the turn |
| Session ends | Inline: stdin closed, SIGTERM to the process group after 2 seconds, SIGKILL after 2 more. Sidecar: stopped with the pod. Service: keeps running |

Inline stderr goes to `plugins/<name>.stderr.log` in the session bundle,
capped at `inline.stderrBytes`. Sidecar and service stderr go to the
container log.

## Local testing

`mayfly plugin` works offline and does not talk to a control plane.

```bash
mayfly plugin init --lang python|ts|go DIR
mayfly plugin check DIR
mayfly plugin test DIR TOOL JSON [--workspace DIR] [--key KEY] [--timeout 2m]
```

- `init` scaffolds `plugin.yaml` (placement `inline`), the server, a
  `Dockerfile`, and a `.dockerignore`. The name defaults to the directory
  name.
- `check` validates the manifest, starts the plugin with `local.command`, and
  compares `tools/list` with the declared tools. It exits non-zero on any
  mismatch.
- `test` does the same checks, then calls one tool with the JSON arguments and
  prints the result. It exits 1 when the result is an error. Secrets are read
  from your environment by `secrets[].env`. `--workspace` sets
  `MAYFLY_WORKSPACE_DIR` (default: a temporary directory). `--key` sets the
  idempotency key.

Placement does not change the MCP protocol the plugin speaks, so a plugin that
passes `mayfly plugin test` behaves the same in every placement. The identity
variables differ: a service plugin gets the session ID per call in
`_meta["mayfly.io/session"]` instead of in its environment.

## Local backend

The local backend does not pull images. It runs `local.command` on the host
with the manifest directory as the plugin directory:

- `inline` as on Kubernetes.
- `sidecar` as `inline`. Its secrets are then read from the `mayflyd`
  environment and written into the agent's secret directory, so the agent
  process can read them, and the `allowShell` rule applies.
- `service` as one child process of `mayflyd`, behind the same adapter, on a
  Unix socket in the state directory. `service.mcpServer` works unchanged.

## What plugins do not solve

- **Untrusted code.** Plugins are trusted operator code. An inline plugin runs
  in the agent container, a sidecar shares the pod's kernel, and a service runs
  in your cluster with its own credentials. Do not install a third-party
  plugin you would not run as a cluster workload. Stronger isolation (gVisor or
  Kata through `runtimeClassName`, or WebAssembly plugins) may come later.
- **Architecture.** Images must match the node architecture. Publish
  multi-architecture images for mixed clusters.

## Limits

- Plugins run only with the built-in harness (`harness` empty or `mayfly`).
- Inline plugins share the agent container's CPU and memory limits.
- The runtime calls tools one at a time. A service receives concurrent calls
  from different sessions, up to `service.concurrency` per replica.
- Tool results are flattened to text and capped at 64 KiB. Image and audio
  content is replaced with a placeholder.
- Only `tools` are used. Resources, prompts, sampling, roots, and
  server-initiated requests are not supported.
- No per-agent tool subsets, hostname egress allowlists, hot reload, or plugin
  registry in the first release.
- Plugins load at control-plane startup. Restart `mayflyd` after changing a
  manifest. A new image tag is picked up by the next session, or by the next
  reconcile for a service.
