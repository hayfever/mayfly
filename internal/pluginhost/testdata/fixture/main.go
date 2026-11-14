//go:build ignore

// Command fixture is a stdio MCP server that internal/pluginhost tests
// configure through the environment. It is built by the tests and never
// installed.
//
// FIXTURE_TOOLS            comma-separated tools to report (default "echo")
// FIXTURE_BAD_SCHEMA       tool whose inputSchema is an array
// FIXTURE_NO_SCHEMA        tool whose inputSchema is omitted
// FIXTURE_COUNTER          add a "counter" tool, counted per session
// FIXTURE_SLEEP_MS         how long the "sleep" tool waits
// FIXTURE_EXIT_AFTER       exit after this many tools/call requests
// FIXTURE_EXIT_IN_CALL     exit inside the next tools/call without answering
// FIXTURE_STDERR           text written to stderr at startup
// FIXTURE_LOG              JSONL file that records each call and notification
// FIXTURE_ENV_FILE         file that receives the complete environment
// FIXTURE_PID_FILE         file that receives the process id
// FIXTURE_IGNORE_STDIN_EOF keep running after stdin closes
// FIXTURE_IGNORE_SIGTERM   ignore SIGTERM
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

var (
	writeMu  sync.Mutex
	calls    atomic.Int64
	counters = map[string]int{}
)

func main() {
	if os.Getenv("FIXTURE_IGNORE_SIGTERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	writeFile(os.Getenv("FIXTURE_PID_FILE"), strconv.Itoa(os.Getpid()))
	if path := os.Getenv("FIXTURE_ENV_FILE"); path != "" {
		writeFile(path, strings.Join(os.Environ(), "\n")+"\n")
	}
	if text := os.Getenv("FIXTURE_STDERR"); text != "" {
		fmt.Fprint(os.Stderr, text)
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			handleNotification(req)
			continue
		}
		handleRequest(req)
	}
	if os.Getenv("FIXTURE_IGNORE_STDIN_EOF") == "1" {
		// Sleep rather than select{}: a bare select makes the Go runtime
		// report a deadlock and exit.
		time.Sleep(24 * time.Hour)
	}
}

func handleRequest(req request) {
	switch req.Method {
	case "initialize":
		respond(req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fixture", "version": "1"},
		})
	case "tools/list":
		respond(req.ID, map[string]any{"tools": reportTools()})
	case "tools/call":
		go handleCall(req)
	default:
		respondError(req.ID, -32601, "unknown method "+req.Method)
	}
}

func handleNotification(req request) {
	logEvent(map[string]any{"method": req.Method, "params": req.Params})
}

func handleCall(req request) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      json.RawMessage `json:"_meta"`
	}
	_ = json.Unmarshal(req.Params, &params)
	logEvent(map[string]any{
		"method":    "tools/call",
		"id":        req.ID,
		"tool":      params.Name,
		"arguments": params.Arguments,
		"meta":      params.Meta,
	})
	calls.Add(1)
	if os.Getenv("FIXTURE_EXIT_IN_CALL") == "1" {
		fmt.Fprintln(os.Stderr, "fixture: exiting during call")
		os.Exit(1)
	}
	if params.Name == "sleep" {
		if ms, err := strconv.Atoi(os.Getenv("FIXTURE_SLEEP_MS")); err == nil {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}
	respond(req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": resultText(params.Name, params.Arguments, params.Meta)}},
	})
	if limit, err := strconv.Atoi(os.Getenv("FIXTURE_EXIT_AFTER")); err == nil && limit > 0 && calls.Load() >= int64(limit) {
		os.Exit(0)
	}
}

// resultText is the text of one tool result.
func resultText(name string, arguments, meta json.RawMessage) string {
	if name == "counter" {
		var envelope struct {
			Session string `json:"mayfly.io/session"`
		}
		_ = json.Unmarshal(meta, &envelope)
		writeMu.Lock()
		counters[envelope.Session]++
		count := counters[envelope.Session]
		writeMu.Unlock()
		return strconv.Itoa(count)
	}
	var args struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(arguments, &args) == nil && args.Text != "" {
		return "echo: " + args.Text
	}
	return "echo: " + string(arguments)
}

// reportTools is the tools/list result.
func reportTools() []map[string]any {
	names := strings.Split(os.Getenv("FIXTURE_TOOLS"), ",")
	if len(names) == 1 && names[0] == "" {
		names = []string{"echo"}
	}
	bad := os.Getenv("FIXTURE_BAD_SCHEMA")
	noSchema := os.Getenv("FIXTURE_NO_SCHEMA")
	tools := make([]map[string]any, 0, len(names)+1)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		tool := map[string]any{"name": name, "description": "fixture " + name}
		switch name {
		case bad:
			tool["inputSchema"] = []string{"not", "an", "object"}
		case noSchema:
		default:
			tool["inputSchema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, tool)
	}
	if os.Getenv("FIXTURE_COUNTER") == "1" {
		tools = append(tools, map[string]any{
			"name":        "counter",
			"description": "counts calls per session",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}
	return tools
}

func respond(id json.RawMessage, result any) {
	writeMu.Lock()
	defer writeMu.Unlock()
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func respondError(id json.RawMessage, code int, message string) {
	writeMu.Lock()
	defer writeMu.Unlock()
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

// logEvent appends one JSON line to FIXTURE_LOG.
func logEvent(event map[string]any) {
	path := os.Getenv("FIXTURE_LOG")
	if path == "" {
		return
	}
	line, err := json.Marshal(event)
	if err != nil {
		return
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(line, '\n'))
	_ = file.Close()
}

func writeFile(path, content string) {
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(content), 0o600)
}
