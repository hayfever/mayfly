# Agents

An agent plugin is a manifest that declares one or more selectable agents. It can also select a runtime image. The control plane parses manifests and builds session configuration; it never executes plugin code.

This is the only plugin surface in the current version. Tools remain MCP servers, and prompt extensions remain skills.

## Select an agent

```bash
./bin/mayfly agents
./bin/mayfly run --agent triage --prompt "Triage the checkout outage"
```

```json
{"agent": "triage", "prompt": "Triage the checkout outage"}
```

```yaml
triggers:
  - name: github
    type: github
    agent: triage
```

The web UI shows the same list in the create dialog.

Empty `agent` selects the agent marked `default: true` in a manifest, otherwise the built-in `default` agent. A request that names an unknown agent is rejected.

## Manifest

```yaml
schemaVersion: 1
name: acme/agents
version: 1.0.0
agents:
  - name: triage
    description: Incident triage with evidence and impact.
    systemPrompt: |
      You are the triage agent. Investigate before you recommend.
    skills:
      - incident-triage
    mcpServers:
      - github
    allowShell: true
    maxSteps: 40
    maxHistoryMessages: 120
    runtime:
      env:
        MAYFLY_AGENT_EXAMPLE: triage
      config:
        triage: true
    default: false
```

| Field | Meaning |
|---|---|
| `schemaVersion` | Must be `1` |
| `name`, `version` | Manifest identity, not the agent name |
| `agents[].name` | Selection name; lowercase letters, digits, dot, underscore, hyphen |
| `description` | UI and list text |
| `harness` | Agent loop executable name inside the image. Empty means `mayfly` |
| `model` | Model override; must pass `llm.allow_models` |
| `systemPrompt` | Inline prompt template |
| `systemPromptFile` | Prompt template file relative to the manifest; mutually exclusive with `systemPrompt` |
| `skills` | Skill names from the global registry (local directories or verified remote sources) |
| `mcpServers` | MCP server names from the global MCP registry |
| `allowShell` | Enables the `run_command` tool for this agent |
| `maxSteps` | Model/tool iterations per user message |
| `maxHistoryMessages` | Messages sent to the model |
| `runtime` | Optional harness image, command, environment, and harness-specific config |
| `default` | At most one agent across all manifests may be default |

Unknown fields are rejected. Duplicate agent names are rejected. `default` is reserved for the built-in agent.

## Harness selection

The standard agent image runs `/usr/local/bin/mayfly-harness` as its entrypoint. The dispatcher reads `harness` from `session.json`:

- empty or `mayfly` → `/usr/local/bin/mayfly-agent` (built-in Go loop),
- `pi` → `/opt/mayfly/harnesses/pi`,
- any named harness → `/opt/mayfly/harnesses/<name>`.

Set `MAYFLY_HARNESS_DIR` to change the directory. The image builder places harness executables there.

A selected harness that is missing or not executable fails with exit code 127. It never silently falls back to Mayfly. Falling back when no harness is selected is intentional; falling back when a requested harness is broken hides a deployment mistake.

`runtime.command` overrides the image entrypoint and bypasses the dispatcher. Do not combine it with `harness` unless you know the command implements the full session protocol.

One image can contain Mayfly plus several harnesses. No per-agent image is required.

## Runtime block

| Field | Meaning |
|---|---|
| `runtime.image` | Optional image override; omit to use the shared global image |
| `runtime.protocol` | Session runtime protocol; must be `1` |
| `runtime.command` | Container command; bypasses the harness dispatcher |
| `runtime.args` | Arguments forwarded to the dispatcher and then to the selected harness |
| `runtime.env` | Literal environment variables. `MAYFLY_SESSION_TOKEN` and `MAYFLY_CONFIG` are reserved |
| `runtime.config` | Arbitrary JSON passed to the harness as `agent_config` in `session.json` |

```yaml
harness: pi
runtime:
  args: ["--mode", "rpc"]
  env:
    PI_LOG_LEVEL: info
  config:
    thinking: high
    maxTurns: 80
```

The built-in runtime ignores `runtime.config`. An alternate harness reads it from `session.json`. Environment values and runtime config are visible in the pod spec or ConfigMap; do not put secrets there. Use the LLM/MCP proxies or a harness that mounts secrets deliberately.

The local backend ignores `harness`, `image`, `command`, and `args`; it runs `runtime.agent_binary` directly. It applies `env` and `config`. Selecting a non-Mayfly harness with the local backend fails because the dispatcher is not involved.

## Selecting Pi

Build one agent image that contains Mayfly and a Pi adapter at `/opt/mayfly/harnesses/pi`. Then declare the agent:

```yaml
agents:
  - name: pi
    harness: pi
    description: Pi coding agent.
    skills: [incident-triage]
    runtime:
      config:
        thinking: high
```

Add the manifest under `agents.dirs` and restart `mayflyd`:

```bash
./bin/mayfly run --agent pi --prompt "Fix the failing deploy"
```

[examples/pi-agent/agent.yaml](../examples/pi-agent/agent.yaml) shows the shape. It is not loaded by default.

The adapter is responsible for the session contract: read `session.json` and `MAYFLY_SESSION_TOKEN`, consume the inbox, emit events, use the LLM/MCP proxies, write the workspace, upload the artifact, and finalize.

## Prompt templates

The prompt is a Go `text/template` with these fields:

```text
.SessionID  .Title  .Agent  .Harness  .Model  .Trigger  .Now
```

`.Trigger` has `Kind`, `Name`, `Event`, `Delivery`, and `Actor`.

A custom prompt replaces the built-in persona. The runtime appends the standard environment and rules block, then every selected skill.

```markdown
You are the {{.Agent}} agent for {{.Title}}.
```

## Precedence

```text
request model      > agent model      > llm.model
request skills     > agent skills     > skills.default
request mcp_servers> agent mcpServers > default non-opt-in MCP servers
agent allowShell   > sessions.allow_shell
agent maxSteps     > sessions.max_steps
agent maxHistory   > sessions.max_history_messages
```

Request skills and MCP servers replace the agent lists when non-empty. They do not append. Unknown names are rejected.

## Discovery

```yaml
agents:
  dirs: [./agents]
  allowed_images:
    - mayfly-agent:*
    - ghcr.io/acme/*
```

All `.yaml`, `.yml`, and `.json` files under the directories are parsed. Hidden directories are skipped so Kubernetes ConfigMap mounts work. A manifest may declare several agents. Manifests load at control-plane startup; restart `mayflyd` after changing them. Sessions are short-lived, so there is no hot reload.

`allowed_images` uses `path.Match` globs. An empty list allows any image declared in a manifest. Keep it non-empty in shared clusters and pin images by digest.

## Harness contract

A harness runs inside the session image and implements runtime protocol 1:

- read the mounted `session.json` and `MAYFLY_SESSION_TOKEN`,
- consume `GET /internal/v1/sessions/{id}/inbox`,
- post assistant/tool events to `/events`,
- use the LLM and MCP proxy endpoints,
- upload a tar.gz bundle to `/artifacts`, then call `/finalize`.

The standard image entrypoint is `/usr/local/bin/mayfly-harness`. It selects `/opt/mayfly/harnesses/<name>` or falls back to the built-in `/usr/local/bin/mayfly-agent`. A custom image must include the dispatcher and the harness executables it declares.

The session gets the same pod security context, NetworkPolicy, session token, and workspace mounts regardless of harness. It never receives LLM, MCP, or object-store credentials.

The local backend ignores `harness`, `image`, `command`, and `args` and runs `runtime.agent_binary`. Selecting a non-Mayfly harness locally fails because the dispatcher is not involved.

Do not run arbitrary plugin code in `mayflyd`.

## Failure behavior

- Unknown agent name: session creation fails with HTTP 400.
- Image not in `agents.allowed_images`: control-plane startup fails.
- Selected harness missing or not executable: the dispatcher exits 127; after a 20-second grace the session is marked failed with the container reason, an error event is recorded, and a partial artifact is exported.
- Image cannot be pulled or the container cannot start: after 30 seconds the session is marked failed with the container reason.
- Container exits without finalizing: after a 20-second grace the session is marked failed with the terminated reason.

Mayfly does not install harness packages or build images during a session. Kubernetes pulls the referenced image automatically. If the image is absent, not allowlisted, or the command inside it is missing, the session fails with the container reason. Build and publish the harness image once, then reference it by digest. Installing packages at session start would require mutable egress, a writable root filesystem, and trust in the package source; the session contract deliberately avoids that.

Private registry credentials and image pull secrets are not managed by Mayfly yet. Configure node-level registry access or add an `imagePullSecrets` field before relying on private harness images.

## Bundled example

[examples/agents/triage.yaml](../examples/agents/triage.yaml) selects the built-in runtime, the `incident-triage` skill, and a small `runtime.config` block. The kind overlay mounts it at `/etc/mayfly-agents`.

[examples/pi-agent/agent.yaml](../examples/pi-agent/agent.yaml) shows an alternate harness with command, args, env, and config.

```bash
./bin/mayfly run --agent triage --prompt "Why did the deploy fail?"
```

## Limits

- No executable hooks or custom tools in the control plane or Go runtime.
- Custom tools belong in MCP servers.
- Custom harnesses belong in runtime images.
- No package manager, hot reload, or plugin registry yet.
