package stub

import (
	"encoding/json"
	"net/http"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// MCPHandler returns a minimal Streamable HTTP MCP server with one echo tool.
func MCPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/mcp", handleMCP)
	return mux
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
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
