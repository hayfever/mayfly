"""tickets is a Mayfly plugin: a stdio MCP server over a ticket API.

The runtime speaks MCP to this process over stdin and stdout. Stdout carries
the protocol; write logs to stderr. The server uses only the standard library.

The cache is shared by every session that reaches this process, which is what
a service placement buys: the ticket API is rate limited, and a warm cache
saves a call. Data that belongs to one session is keyed on
meta["mayfly.io/session"], which the gateway sends with every call.
"""

import json
import os
import sys
import urllib.request

PROTOCOL_VERSION = "2025-06-18"
SERVER_INFO = {"name": "tickets", "version": "0.1.0"}

TOOLS = [
    {
        "name": "get_ticket",
        "description": "Look up one ticket by id.",
        "inputSchema": {
            "type": "object",
            "properties": {"id": {"type": "string", "description": "ticket id"}},
            "required": ["id"],
        },
    },
    {
        "name": "add_comment",
        "description": "Add a comment to one ticket.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "id": {"type": "string", "description": "ticket id"},
                "text": {"type": "string", "description": "comment body"},
            },
            "required": ["id", "text"],
        },
    },
]

# CACHE holds one record per ticket for the life of the process.
CACHE = {}


def sample(ticket):
    """Build one ticket for the offline path."""
    return {"id": ticket, "subject": "Sample ticket %s" % ticket, "status": "open", "comments": []}


def fetch(path, payload=None):
    """Call the ticket API, or return None when TICKETS_URL is not set."""
    base = os.environ.get("TICKETS_URL", "")
    if not base:
        return None
    data = json.dumps(payload).encode() if payload is not None else None
    request = urllib.request.Request(
        base + path,
        data=data,
        method="POST" if data is not None else "GET",
    )
    request.add_header("Content-Type", "application/json")
    token = os.environ.get("TICKETS_TOKEN", "")
    if token:
        request.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(request, timeout=20) as response:
        return json.load(response)


def get_ticket(arguments, meta):
    ticket = arguments.get("id", "")
    if ticket in CACHE:
        return json.dumps(CACHE[ticket])
    record = fetch("/tickets/%s" % ticket) or sample(ticket)
    CACHE[ticket] = record
    return json.dumps(record)


def add_comment(arguments, meta):
    ticket = arguments.get("id", "")
    entry = {
        "text": arguments.get("text", ""),
        "session": meta.get("mayfly.io/session", ""),
    }
    result = fetch("/tickets/%s/comments" % ticket, entry)
    if result is not None:
        return json.dumps(result)
    record = CACHE.setdefault(ticket, sample(ticket))
    record["comments"].append(entry)
    return json.dumps({"ticket": ticket, "comments": len(record["comments"])})


HANDLERS = {"get_ticket": get_ticket, "add_comment": add_comment}


def call_tool(params):
    name = params.get("name", "")
    handler = HANDLERS.get(name)
    if handler is None:
        raise ValueError("unknown tool %r" % name)
    text = handler(params.get("arguments") or {}, params.get("_meta") or {})
    return {"content": [{"type": "text", "text": text}]}


def error_result(error):
    return {"content": [{"type": "text", "text": str(error)}], "isError": True}


def handle(message):
    """Return the JSON-RPC response object for one request."""
    response = {"jsonrpc": "2.0", "id": message["id"]}
    method = message.get("method", "")
    if method == "initialize":
        response["result"] = {
            "protocolVersion": PROTOCOL_VERSION,
            "capabilities": {"tools": {}},
            "serverInfo": SERVER_INFO,
        }
    elif method == "tools/list":
        response["result"] = {"tools": TOOLS}
    elif method == "tools/call":
        try:
            response["result"] = call_tool(message.get("params") or {})
        except Exception as error:  # a failed tool is an error result, not a crash
            response["result"] = error_result(error)
    else:
        response["error"] = {"code": -32601, "message": "method not found: %s" % method}
    return response


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            message = json.loads(line)
        except ValueError as error:
            print("decode request: %s" % error, file=sys.stderr)
            continue
        if "id" not in message:
            continue  # a notification, for example notifications/initialized
        sys.stdout.write(json.dumps(handle(message)) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
