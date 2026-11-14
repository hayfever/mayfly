// word-count is a Mayfly plugin: a stdio MCP server that counts the words in
// one workspace file.
//
// The runtime speaks MCP to this process over stdin and stdout. Stdout carries
// the protocol; write logs to stderr. Node strips the type annotations when it
// loads a .ts file, so this file runs without a build step.

import * as fs from "node:fs/promises";
import * as path from "node:path";
import * as readline from "node:readline";

const PROTOCOL_VERSION = "2025-06-18";
const SERVER_INFO = { name: "word-count", version: "0.1.0" };

type ToolDefinition = {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
};

type CallParams = {
  name?: string;
  arguments?: Record<string, unknown>;
  _meta?: Record<string, unknown>;
};

type Request = {
  id?: unknown;
  method?: string;
  params?: CallParams;
};

const TOOLS: ToolDefinition[] = [
  {
    name: "count_words",
    description: "Count the words in a file inside the workspace.",
    inputSchema: {
      type: "object",
      properties: {
        path: { type: "string", description: "path relative to the workspace directory" },
      },
      required: ["path"],
    },
  },
];

// countWords reads one file and counts whitespace-separated words. The path
// must stay inside the workspace: a tool that can read /etc/shadow is a tool
// that can leak it into the transcript.
async function countWords(input: Record<string, unknown>): Promise<string> {
  const workspace = process.env.MAYFLY_WORKSPACE_DIR ?? "";
  if (workspace === "") {
    throw new Error("MAYFLY_WORKSPACE_DIR is not set");
  }
  const root = path.resolve(workspace);
  const target = path.resolve(root, typeof input.path === "string" ? input.path : "");
  if (target !== root && !target.startsWith(root + path.sep)) {
    throw new Error("path must stay inside the workspace");
  }
  const text = await fs.readFile(target, "utf8");
  return String(text.split(/\s+/).filter((word) => word !== "").length);
}

async function callTool(params: CallParams): Promise<Record<string, unknown>> {
  const name = params.name ?? "";
  if (name !== "count_words") {
    throw new Error(`unknown tool ${JSON.stringify(name)}`);
  }
  return textResult(await countWords(params.arguments ?? {}));
}

function textResult(text: string): Record<string, unknown> {
  return { content: [{ type: "text", text }] };
}

function errorResult(error: unknown): Record<string, unknown> {
  const text = error instanceof Error ? error.message : String(error);
  return { content: [{ type: "text", text }], isError: true };
}

async function handle(message: Request): Promise<Record<string, unknown>> {
  const response: Record<string, unknown> = { jsonrpc: "2.0", id: message.id };
  switch (message.method) {
    case "initialize":
      response.result = {
        protocolVersion: PROTOCOL_VERSION,
        capabilities: { tools: {} },
        serverInfo: SERVER_INFO,
      };
      break;
    case "tools/list":
      response.result = { tools: TOOLS };
      break;
    case "tools/call":
      try {
        response.result = await callTool(message.params ?? {});
      } catch (error) {
        // A failed tool is an error result, not a crash.
        response.result = errorResult(error);
      }
      break;
    default:
      response.error = { code: -32601, message: `method not found: ${message.method ?? ""}` };
  }
  return response;
}

const lines = readline.createInterface({ input: process.stdin });
lines.on("line", async (line: string) => {
  if (line.trim() === "") {
    return;
  }
  let message: Request;
  try {
    message = JSON.parse(line) as Request;
  } catch (error) {
    console.error(`decode request: ${String(error)}`);
    return;
  }
  if (message.id === undefined) {
    return; // a notification, for example notifications/initialized
  }
  process.stdout.write(JSON.stringify(await handle(message)) + "\n");
});
