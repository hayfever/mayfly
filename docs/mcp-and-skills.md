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

At session start, the runtime receives:

```json
{
  "name": "github",
  "transport": "http",
  "url": "http://mayfly.mayfly.svc.cluster.local:8080/internal/v1/mcp/github"
}
```

The runtime adds the session token. The control plane checks that `github` is assigned to the session, connects to the configured upstream URL, injects the configured headers, and streams the response. Header values using `env:NAME` are resolved once at control-plane startup and never written to a ConfigMap or pod spec.

Session pods only need network access to the control plane. Upstream MCP credentials and URLs stay outside the container.

Supported methods: `POST`, `GET`, `DELETE`. Headers forwarded: `Content-Type`, `Accept`, `Mcp-Session-Id`, `MCP-Protocol-Version`, `Last-Event-ID`. Response headers forwarded: `Content-Type`, `Cache-Control`, `Mcp-Session-Id`, `MCP-Protocol-Version`.

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

The runtime spawns the process inside the session container, performs `initialize`, then `tools/list`.

Environment values using `env:` are rejected: secrets would land in the pod ConfigMap. Use an HTTP MCP server behind the proxy when credentials are required.

The agent image includes Node, npm/npx, git, curl, jq, and ripgrep. Add tools to the image when your stdio servers need them. `HOME` and npm caches point at the writable `/tmp` mount.

### Tool names

Tool names are sanitized and namespaced:

```text
mcp_<server>_<tool>
```

Example: server `github`, tool `search_issues` becomes `mcp_github_search_issues`. Collisions receive a numeric suffix. Built-in tools are `read_file`, `write_file`, `list_files`, and `run_command` (when enabled).

### Startup failures

One unavailable MCP server does not reject the session. The runtime records a `system` event naming the server, exposes the remaining tools, and continues. Initialization is bounded by `init_timeout` (default 30 seconds).

### Per-session selection

```bash
curl -sS http://localhost:8080/api/v1/sessions \
  -H "Authorization: Bearer $MAYFLY_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"...","mcp_servers":["github"]}'
```

`opt_in: true` servers are used only when selected explicitly. Selection by unknown name rejects the request.

## Built-in tools

| Tool | Behavior |
|---|---|
| `read_file` | Reads up to 512 KiB from the workspace; paths are confined with `os.Root` |
| `write_file` | Writes up to 4 MiB; creates parent directories |
| `list_files` | Lists up to 500 entries in one directory |
| `run_command` | Runs `/bin/sh -c` with the workspace as cwd and a timeout; output is capped; can be disabled |

`run_command` is not a sandbox. The container is the boundary.

## Protocol support

Implemented MCP features: `initialize`, `notifications/initialized`, `tools/list` with pagination, `tools/call`, and `ping`. Sampling, roots, resources, prompts, and server-initiated requests are not implemented. Tool content is flattened to text; image and audio blocks are replaced with a placeholder.
