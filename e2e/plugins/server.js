// Deterministic stdio MCP server for the plugin end-to-end tests. The same
// file runs inline, behind the sidecar shim, and as a service workload, so the
// three placements share one fixture image.
//
// E2E_TOOLS lists the tools the manifest declares, so a placement never
// reports a tool its manifest does not declare. Calls arrive as newline
// delimited JSON-RPC on stdin and leave the same way on stdout; logs go to
// stderr, which never carries the protocol.
"use strict";

const fs = require("fs");
const net = require("net");
const path = require("path");
const readline = require("readline");

const name = process.env.MAYFLY_PLUGIN_NAME || "fixture";
const mode = process.env.E2E_MODE || "inline";
const delayMS = Number(process.env.E2E_SLOW_MS || 120000);

// State is per process: an inline plugin gets a fresh one per container, a
// service shares it across sessions until its pod restarts.
const sessions = new Map();
let total = 0;

const tools = {
  echo: {
    description: "Echo text back to the caller.",
    inputSchema: {
      type: "object",
      properties: { text: { type: "string" } },
    },
  },
  env: {
    description: "Report the environment the plugin process can read.",
    inputSchema: { type: "object", properties: {} },
  },
  slow: {
    description: "Write a marker into the workspace, then stay in flight.",
    inputSchema: { type: "object", properties: {} },
  },
  state: {
    description: "Count calls per session in this process.",
    inputSchema: { type: "object", properties: {} },
  },
  probe: {
    description: "Dial a TCP address from the plugin process.",
    inputSchema: {
      type: "object",
      properties: {
        host: { type: "string" },
        port: { type: "number" },
      },
    },
  },
};

const enabled = (process.env.E2E_TOOLS || "echo,env")
  .split(",")
  .map((tool) => tool.trim())
  .filter((tool) => tools[tool]);

function text(value) {
  return { content: [{ type: "text", text: value }], isError: false };
}

function has(name) {
  return Object.prototype.hasOwnProperty.call(process.env, name);
}

const handlers = {
  echo(args) {
    const value = typeof args.text === "string" ? args.text : "empty";
    return text("echo: " + value);
  },
  env() {
    return text(
      JSON.stringify({
        plugin: name,
        mode,
        session: process.env.MAYFLY_SESSION_ID || "",
        workspace: process.env.MAYFLY_WORKSPACE_DIR || "",
        plugin_dir: process.env.MAYFLY_PLUGIN_DIR || "",
        home: process.env.HOME || "",
        token_set: has("MAYFLY_SESSION_TOKEN"),
        config_set: has("MAYFLY_CONFIG"),
        inline_env: process.env.E2E_INLINE_ENV || "",
        sidecar_env: process.env.E2E_SIDECAR_ENV || "",
        sidecar_secret: process.env.E2E_SIDECAR_SECRET || "",
        tool_names: enabled,
      })
    );
  },
  async slow() {
    const workspace = process.env.MAYFLY_WORKSPACE_DIR;
    let count = 1;
    if (workspace) {
      const file = path.join(workspace, ".plugin-calls");
      if (fs.existsSync(file)) {
        const seen = Number(fs.readFileSync(file, "utf8"));
        if (Number.isFinite(seen)) {
          count = seen + 1;
        }
      }
      // Write before sleeping: a test deletes the pod while this call is in
      // flight, and the file survives in the session workspace claim.
      fs.writeFileSync(file, String(count));
    }
    await new Promise((resolve) => setTimeout(resolve, delayMS));
    return text("slow done count=" + count);
  },
  state(_args, meta) {
    const session = meta["mayfly.io/session"] || "unknown";
    sessions.set(session, (sessions.get(session) || 0) + 1);
    total += 1;
    return text(
      JSON.stringify({
        total,
        sessions: Object.fromEntries(sessions),
      })
    );
  },
  async probe(args) {
    const host = String(args.host || "");
    const port = Number(args.port || 0);
    const reachable = await new Promise((resolve) => {
      const socket = net.connect({ host, port });
      const done = (ok) => {
        socket.destroy();
        resolve(ok);
      };
      socket.setTimeout(3000, () => done(false));
      socket.on("connect", () => done(true));
      socket.on("error", () => done(false));
    });
    return text(JSON.stringify({ host, port, reachable }));
  },
};

function dispatch(message) {
  switch (message.method) {
    case "initialize":
      return {
        protocolVersion: "2025-06-18",
        capabilities: { tools: {} },
        serverInfo: { name: "e2e-plugin-" + name, version: "0.1.0" },
      };
    case "ping":
      return {};
    case "tools/list":
      return {
        tools: enabled.map((tool) => Object.assign({ name: tool }, tools[tool])),
      };
    case "tools/call": {
      const params = message.params || {};
      const tool = params.name;
      if (!enabled.includes(tool)) {
        throw { code: -32602, message: "unknown tool " + tool };
      }
      const meta = params._meta || {};
      return handlers[tool](params.arguments || {}, meta);
    }
    default:
      throw { code: -32601, message: "method not found: " + message.method };
  }
}

async function handle(line) {
  let message;
  try {
    message = JSON.parse(line);
  } catch (err) {
    process.stderr.write("fixture: invalid JSON: " + err + "\n");
    return;
  }
  if (message.id === undefined || message.id === null) {
    return;
  }
  let payload;
  try {
    payload = await dispatch(message);
    payload = { jsonrpc: "2.0", id: message.id, result: payload };
  } catch (err) {
    if (err && typeof err.code === "number") {
      payload = { jsonrpc: "2.0", id: message.id, error: err };
    } else {
      process.stderr.write("fixture: tool failed: " + err + "\n");
      payload = {
        jsonrpc: "2.0",
        id: message.id,
        error: { code: -32603, message: String(err) },
      };
    }
  }
  process.stdout.write(JSON.stringify(payload) + "\n");
}

const input = readline.createInterface({ input: process.stdin });
input.on("line", (line) => {
  if (line.trim() !== "") {
    handle(line).catch((err) => {
      process.stderr.write("fixture: " + err + "\n");
    });
  }
});
input.on("close", () => process.exit(0));

process.stderr.write(
  "e2e plugin " + name + " (" + mode + ") tools=" + enabled.join(",") + "\n"
);
