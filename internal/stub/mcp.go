package stub

import (
	"encoding/json"
	"net/http"
	"sync"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// MCPCounts records tools/call executions by remote tool name. The e2e suite
// uses it to prove a resumed turn did not repeat a completed tool call.
type MCPCounts struct {
	mu    sync.Mutex
	calls map[string]int
}

// NewMCPCounts returns an empty counter set.
func NewMCPCounts() *MCPCounts {
	return &MCPCounts{calls: map[string]int{}}
}

// Add records one call.
func (c *MCPCounts) Add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[name]++
}

// Reset clears every count.
func (c *MCPCounts) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = map[string]int{}
}

// Snapshot returns a copy of the counters.
func (c *MCPCounts) Snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.calls))
	for k, v := range c.calls {
		out[k] = v
	}
	return out
}

// MCPHandler returns a minimal Streamable HTTP MCP server with one echo tool.
func MCPHandler() http.Handler {
	return MCPHandlerWithCounts(NewMCPCounts())
}

// MCPHandlerWithCounts returns the MCP server and reports its counters at
// GET /counts. POST /counts/reset clears them.
func MCPHandlerWithCounts(counts *MCPCounts) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /counts", func(w http.ResponseWriter, r *http.Request) {
		writeStubJSON(w, map[string]any{"counts": counts.Snapshot()})
	})
	mux.HandleFunc("POST /counts/reset", func(w http.ResponseWriter, r *http.Request) {
		counts.Reset()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		handleMCP(w, r, counts)
	})
	return mux
}

func handleMCP(w http.ResponseWriter, r *http.Request, counts *MCPCounts) {
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rpcErr := dispatchMCP(req)
	if rpcErr == nil && req.Method == "tools/call" {
		var params struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(req.Params, &params) == nil {
			counts.Add(params.Name)
		}
	}
	if rpcErr != nil {
		writeStubJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": rpcErr})
		return
	}
	writeStubJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func dispatchMCP(req rpcRequest) (any, map[string]any) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "mcpstub", "version": "dev"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": []map[string]any{{
			"name":        "echo",
			"description": "Echo text back to the caller.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string", "description": "Text to echo"},
				},
			},
		}}}, nil
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, map[string]any{"code": -32602, "message": "invalid params"}
		}
		if params.Name != "echo" {
			return nil, map[string]any{"code": -32602, "message": "unknown tool " + params.Name}
		}
		text, _ := params.Arguments["text"].(string)
		if text == "" {
			text = "empty"
		}
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "echo: " + text}},
			"isError": false,
		}, nil
	default:
		return nil, map[string]any{"code": -32601, "message": "method not found: " + req.Method}
	}
}
