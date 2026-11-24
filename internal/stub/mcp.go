package stub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// MCPCounts records tools/call executions by remote tool name, and the _meta
// block of the last call of each tool. The e2e suite uses the counters to prove
// a resumed turn did not repeat a completed call, and the metadata to prove the
// gateway attached an idempotency key.
type MCPCounts struct {
	mu    sync.Mutex
	calls map[string]int
	meta  map[string]map[string]any
}

// NewMCPCounts returns an empty counter set.
func NewMCPCounts() *MCPCounts {
	return &MCPCounts{calls: map[string]int{}, meta: map[string]map[string]any{}}
}

// Add records one call.
func (c *MCPCounts) Add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[name]++
}

// RecordMeta stores the _meta block of the last call of one tool.
func (c *MCPCounts) RecordMeta(name string, meta map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(meta) == 0 {
		return
	}
	c.meta[name] = meta
}

// Meta returns a copy of the last metadata seen per tool.
func (c *MCPCounts) Meta() map[string]map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]map[string]any, len(c.meta))
	for name, meta := range c.meta {
		copied := make(map[string]any, len(meta))
		for k, v := range meta {
			copied[k] = v
		}
		out[name] = copied
	}
	return out
}

// Reset clears every count and the recorded metadata.
func (c *MCPCounts) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = map[string]int{}
	c.meta = map[string]map[string]any{}
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

// ImagePNGBase64 is a 1x1 transparent PNG returned by the image tool.
const ImagePNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// ImagePNGMIME is the MIME type of ImagePNGBase64.
const ImagePNGMIME = "image/png"

// MCPOptions selects how the stub frames its responses and how large the
// image tool's payload is.
type MCPOptions struct {
	// SSE frames every JSON-RPC response as one text/event-stream message.
	SSE bool
	// ImageBytes is the decoded size of the image tool's payload. Zero uses
	// ImagePNGBase64.
	ImageBytes int
}

// MCPHandler returns a minimal Streamable HTTP MCP server with an echo tool
// and an image tool.
func MCPHandler() http.Handler {
	return MCPHandlerWithCounts(NewMCPCounts())
}

// MCPHandlerWithOptions returns the stub MCP server with the given response
// framing and image size.
func MCPHandlerWithOptions(opts MCPOptions) http.Handler {
	return MCPHandlerWithCountsOptions(NewMCPCounts(), opts)
}

// MCPHandlerWithCounts returns the MCP server, reports its counters at
// GET /counts and the last call metadata at GET /meta, and clears both at
// POST /counts/reset.
func MCPHandlerWithCounts(counts *MCPCounts) http.Handler {
	return MCPHandlerWithCountsOptions(counts, MCPOptions{})
}

// MCPHandlerWithCountsOptions is MCPHandlerWithCounts with response framing
// and image size options.
func MCPHandlerWithCountsOptions(counts *MCPCounts, opts MCPOptions) http.Handler {
	server := &mcpServer{counts: counts, opts: opts, image: imagePayload(opts.ImageBytes)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /counts", func(w http.ResponseWriter, r *http.Request) {
		writeStubJSON(w, map[string]any{"counts": server.counts.Snapshot()})
	})
	mux.HandleFunc("GET /meta", func(w http.ResponseWriter, r *http.Request) {
		writeStubJSON(w, map[string]any{"meta": server.counts.Meta()})
	})
	mux.HandleFunc("POST /counts/reset", func(w http.ResponseWriter, r *http.Request) {
		server.counts.Reset()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/mcp", server.handle)
	return mux
}

// imagePayload returns the base64 image the image tool answers with.
func imagePayload(size int) string {
	if size <= 0 {
		return ImagePNGBase64
	}
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x89}, size))
}

// mcpServer is the stub's state: counters, response framing, and the image
// payload the image tool returns.
type mcpServer struct {
	counts *MCPCounts
	opts   MCPOptions
	image  string
}

// reply writes one JSON-RPC response, as JSON or as one SSE message.
func (s *mcpServer) reply(w http.ResponseWriter, value any) {
	if !s.opts.SSE {
		writeStubJSON(w, value)
		return
	}
	payload, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
}

func (s *mcpServer) handle(w http.ResponseWriter, r *http.Request) {
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
	result, rpcErr := s.dispatch(req)
	if rpcErr == nil && req.Method == "tools/call" {
		var params struct {
			Name string         `json:"name"`
			Meta map[string]any `json:"_meta"`
		}
		if json.Unmarshal(req.Params, &params) == nil {
			s.counts.Add(params.Name)
			s.counts.RecordMeta(params.Name, params.Meta)
		}
	}
	if rpcErr != nil {
		s.reply(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": rpcErr})
		return
	}
	s.reply(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func (s *mcpServer) dispatch(req rpcRequest) (any, map[string]any) {
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
		return map[string]any{"tools": []map[string]any{
			{
				"name":        "echo",
				"description": "Echo text back to the caller.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": map[string]any{"type": "string", "description": "Text to echo"},
					},
				},
			},
			{
				"name":        "image",
				"description": "Return an image with a caption.",
				"inputSchema": map[string]any{"type": "object"},
			},
		}}, nil
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, map[string]any{"code": -32602, "message": "invalid params"}
		}
		if params.Name == "image" {
			return map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "image follows"},
					{"type": "image", "mimeType": ImagePNGMIME, "data": s.image},
				},
				"isError": false,
			}, nil
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
