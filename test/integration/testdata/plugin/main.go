// Command plugin is a stdio MCP server fixture for the plugin integration
// tests. It reports three tools: echo, record (idempotent), and sleep.
// CALL_LOG receives one JSON line per call so a test can prove a tool ran, or
// did not run again.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 1<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for in.Scan() {
		var req request
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue // a notification carries no answer
		}
		response := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		result, rpcErr := dispatch(req)
		if rpcErr != nil {
			response["error"] = rpcErr
		} else {
			response["result"] = result
		}
		line, err := json.Marshal(response)
		if err != nil {
			continue
		}
		_, _ = out.Write(line)
		_ = out.WriteByte('\n')
		_ = out.Flush()
	}
}

// toolNames is the fixed tool list every instance reports.
var toolNames = []string{"echo", "record", "sleep"}

func dispatch(req request) (any, map[string]any) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "fixture", "version": "dev"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		list := make([]toolDef, 0, len(toolNames))
		for _, name := range toolNames {
			list = append(list, toolDef{
				Name:        name,
				Description: "Fixture tool " + name + ".",
				InputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}},
				},
			})
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		return call(req.Params)
	}
	return nil, map[string]any{"code": -32601, "message": "method not found: " + req.Method}
}

func call(raw json.RawMessage) (any, map[string]any) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
		Meta      map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, map[string]any{"code": -32602, "message": "invalid params"}
	}
	switch params.Name {
	case "echo", "record", "sleep":
	default:
		return nil, map[string]any{"code": -32602, "message": "unknown tool " + params.Name}
	}
	key, _ := params.Meta["mayfly.io/idempotency-key"].(string)
	logCall(params.Name, key)
	var text string
	switch params.Name {
	case "record":
		time.Sleep(sleepDuration())
		text = "recorded " + key
	case "sleep":
		time.Sleep(sleepDuration())
		text = "slept"
	case "echo":
		arg, _ := params.Arguments["text"].(string)
		text = "echo: " + arg
	default:
		text = params.Name
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": false,
	}, nil
}

func sleepDuration() time.Duration {
	ms, _ := strconv.Atoi(os.Getenv("FIXTURE_SLEEP_MS"))
	if ms <= 0 {
		ms = 5000
	}
	return time.Duration(ms) * time.Millisecond
}

// logCall appends one line to CALL_LOG. A missing log is not an error: the
// same binary also serves the MCP-server test, which does not read one.
func logCall(tool, key string) {
	path := os.Getenv("CALL_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	line, err := json.Marshal(map[string]string{"tool": tool, "key": key, "pid": fmt.Sprint(os.Getpid())})
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
}
