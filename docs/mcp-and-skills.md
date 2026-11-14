# MCP servers and skills

Both are injected at session start from the control plane configuration. A session can use a subset by name. Tool definitions become part of the model request immediately after the runtime connects.

## Skills

A skill is a Markdown file with optional YAML frontmatter:

```markdown
---
name: incident-triage
description: Triage a production incident with evidence.
---
# Incident triage

1. State what is failing and who is affected.
2. Separate facts from assumptions.
```

The control plane reads `skills.dirs` at startup. The file basename becomes the skill name when `name` is absent. Duplicate names are an error. The description and body are appended to the system prompt under `## Skill: <name>`.

Selection:

- Agent manifest: `skills: [incident-triage]` or `mcpServers: [github]`.
- Request: `"skills": ["incident-triage"]`.
- Trigger: `skills: [incident-triage]`.
- Default: `skills.default`, or every discovered skill when the list is empty.
- Unknown names reject session creation.

The agent is told that deliverables belong in the workspace. Workspace files are exported in the bundle.

## MCP servers

### HTTP servers (recommended)

```yaml
mcp:
  servers:
    - name: github
      transport: http
      url: https://mcp.example.com/mcp
      headers:
        Authorization: "env:GITHUB_MCP_TOKEN"
```

The gateway checks that `github` is assigned to the session, connects to the
configured upstream URL, injects the configured headers, and lists the
server's tools. It adds them to the session's grant. The runtime receives the
granted tools and never connects to the server itself. Header values using
`env:NAME` are resolved once at control-plane startup and never written to a
ConfigMap or pod spec.

Session pods only need network access to the control plane. Upstream MCP credentials and URLs stay outside the container.

A `tools/call` is a granted tool call: the runtime asks the tool gateway, and the gateway
calls the upstream itself with the resolved headers. The call is recorded in
the gateway's ledger before the model sees a result. When gateway discovery
fails, the server's tools are absent from the session instead of falling back
to the proxy, so no call can skip the ledger. `* /internal/v1/mcp/{server}`
remains for alternate harnesses, which do not use the gateway.

Supported methods on the proxy: `POST`, `GET`, `DELETE`. Request bodies are
limited to `llm.max_body_bytes` (default 16 MiB); larger requests get `413`.
Headers forwarded: `Content-Type`, `Accept`, `Mcp-Session-Id`,
`MCP-Protocol-Version`, `Last-Event-ID`. Response headers forwarded:
`Content-Type`, `Cache-Control`, `Mcp-Session-Id`, `MCP-Protocol-Version`.

### stdio servers

```yaml
mcp:
  servers:
    - name: filesystem
      transport: stdio
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/workspace"]
      env:
        LOG_LEVEL: info
      opt_in: true
```

The runtime spawns the process inside the session container, performs `initialize`, then `tools/list`. Each `tools/call` is authorized and recorded by the tool gateway, which returns a dispatch; the runtime calls the process and reports the result back.

The child does not inherit the runtime's environment. It receives `PATH`, `HOME`, `TMPDIR`, `LANG`, `TZ`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, and the proxy variables of the container, plus the entry's `env`. It does not receive `MAYFLY_SESSION_TOKEN`, `MAYFLY_CONFIG`, provider keys, or webhook secrets: the session token authorizes the tool gateway, so handing it to a server would let that server dispatch calls of the session.

Environment values using `env:` are rejected: secrets would land in the pod ConfigMap. Use an HTTP MCP server behind the proxy when credentials are required.

The agent image includes Node, npm/npx, git, curl, jq, and ripgrep. Add tools to the image when your stdio servers need them. `HOME` and npm caches point at the writable `/tmp` mount.

### Tool allowlist and idempotency

Two optional keys narrow and classify a server's tools:

```yaml
mcp:
  servers:
    - name: github
      transport: http
      url: https://mcp.example.com/mcp
      tools: [search_issues, get_issue]
      idempotent_tools: [search_issues, get_issue]
```

| Key | Default | Meaning |
|---|---|---|
| `tools` | `[]` | Model-visible tools this server contributes. Empty means every tool the server reports |
| `idempotent_tools` | `[]` | Tools that are safe to repeat after a container restart. Must be a subset of `tools` when `tools` is set. Every other tool is non-idempotent |

A tool outside `tools` is not offered to the model, and the gateway refuses a call to it. A stdio server without `tools` is granted by name prefix: the runtime resolves each tool its own manager listed, and the gateway checks the prefix when the call begins. An HTTP server is always discovered by the gateway.

### Tool names

Tool names are sanitized and namespaced:

```text
mcp_<server>_<tool>
```

Example: server `github`, tool `search_issues` becomes `mcp_github_search_issues`. Collisions receive a numeric suffix. Built-in tools are `read_file`, `write_file`, `list_files`, and `run_command` (when enabled).

Plugin tools use the same scheme with a different prefix: `plugin_<plugin>_<tool>`. A plugin is declared in the bundle, not under `mcp.servers`; see [plugins.md](plugins.md).

### Startup failures

One unavailable MCP server does not reject the session. The runtime records a `system` event naming the server, exposes the remaining tools, and continues. Initialization is bounded by `init_timeout` (default 30 seconds). A tool gateway discovery failure is reported the same way, and fails the session only when a required plugin caused it.

### Per-session selection

```bash
curl -sS http://localhost:8080/api/v1/sessions \
  -H "Authorization: Bearer $MAYFLY_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"...","mcp_servers":["github"]}'
```

`opt_in: true` servers are used only when selected explicitly. Selection by unknown name rejects the request.

## Tool calls through the gateway

With the built-in harness, every `mcp.servers` call and every plugin call goes
through the tool gateway in `mayflyd`. For an MCP server, the gateway:

- checks that the tool is in the session's grant: the `tools` allowlist, or
  every tool the server reports when no allowlist is set,
- applies the in-flight cap and, for a plugin, its declared rate limit,
- records the call in its ledger under `<session>/<turn>/<step>` and hashes the
  arguments; it never stores them,
- calls an HTTP server itself, or hands a stdio call to the runtime and stores
  the result the runtime reports,
- writes one `call.begin` and one `call.done` audit record without arguments or
  results.

The session token is the gateway credential. A tool outside the grant gets
`403`. A call key reused for another tool or other arguments gets `409`, which
is fatal for the turn.

Resume behavior changed with the gateway. Before, an `mcp_*` call that was in
flight when the container died ran again after resume. Now it does not: the
model gets an error result saying the call may already have taken effect, and
a `system` event says the same in the thread. List read-only tools and tools
that forward the idempotency key in `idempotent_tools` to keep a repeat for
them. A call that finished but whose journal step did not land replays the
recorded result instead of running again.

Alternate harnesses bypass the gateway through `* /internal/v1/mcp/{server}`
and keep the old behavior.

## Built-in tools

| Tool | Behavior |
|---|---|
| `read_file` | Reads up to 512 KiB from the workspace; paths are confined with `os.Root` |
| `write_file` | Writes up to 4 MiB; creates parent directories |
| `list_files` | Lists up to 500 entries in one directory |
| `run_command` | Runs `/bin/sh -c` with the workspace as cwd and a timeout; output is capped; off unless `sessions.allow_shell` or an agent `allowShell` enables it |

`run_command` is not a sandbox. The container is the boundary.

## Protocol support

Implemented MCP features: `initialize`, `notifications/initialized`, `tools/list` with pagination, `tools/call`, and `ping`. Sampling, roots, resources, prompts, and server-initiated requests are not implemented. Tool content is flattened to text; image and audio blocks are replaced with a placeholder.
