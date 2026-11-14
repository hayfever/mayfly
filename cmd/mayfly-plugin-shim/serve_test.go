package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/gateway"
)

// fixtureBin is the stdio MCP server the shim tests supervise, built once.
var fixtureBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mayfly-shim-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixture directory: %v\n", err)
		os.Exit(1)
	}
	fixtureBin = filepath.Join(dir, "fixture")
	build := exec.Command("go", "build", "-o", fixtureBin, "../../internal/pluginhost/testdata/fixture/main.go")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fixture: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// shim is one serve process running in the test.
type shim struct {
	listen string
	key    []byte
	cancel context.CancelFunc
	done   chan error
}

// startShim starts serve and waits until its child is ready.
func startShim(t *testing.T, opts serveOptions) *shim {
	t.Helper()
	key := bytes.Repeat([]byte("k"), 32)
	opts.keyFile = ""
	opts.plugin = "demo"
	opts.stderr = io.Discard
	if opts.maxConcurrency == 0 {
		opts.maxConcurrency = defaultMaxConcurrency
	}
	if opts.initTimeout == 0 {
		opts.initTimeout = 10 * time.Second
	}
	if opts.command == nil {
		opts.command = []string{fixtureBin}
	}
	if opts.listen == "" {
		opts.listen = "unix:" + filepath.Join(t.TempDir(), "shim.sock")
	}
	server := newServeServer(opts, key)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.run(ctx) }()
	s := &shim{listen: opts.listen, key: key, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve stopped with %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("serve did not stop")
		}
	})
	waitReady(t, s)
	return s
}

// waitReady waits until a probe passes.
func waitReady(t *testing.T, s *shim) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := probe(context.Background(), s.listen, 500*time.Millisecond); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("shim never became ready: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// shimClient talks to a serve process over its socket.
type shimClient struct {
	t      *testing.T
	listen string
	http   *http.Client
}

// newShimClient returns a client for one shim.
func newShimClient(t *testing.T, listen string) *shimClient {
	t.Helper()
	network, addr, err := parseListen(listen)
	if err != nil {
		t.Fatalf("listen %q: %v", listen, err)
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, addr)
		},
	}
	return &shimClient{t: t, listen: listen, http: &http.Client{Timeout: 30 * time.Second, Transport: transport}}
}

// post sends one JSON-RPC message. A zero id means a notification.
func (c *shimClient) post(method string, id int64, params any, token string) (int, []byte) {
	c.t.Helper()
	message := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != 0 {
		message["id"] = id
	}
	if params != nil {
		message["params"] = params
	}
	body, err := json.Marshal(message)
	if err != nil {
		c.t.Fatalf("encode %s: %v", method, err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://plugin"+mcpPath, bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		c.t.Fatalf("read %s: %v", method, err)
	}
	return response.StatusCode, raw
}

// discoveryToken signs the token that authorizes initialize and tools/list.
func discoveryToken(t *testing.T, key []byte, session string) string {
	t.Helper()
	return signToken(t, key, gateway.Claims{
		SessionID: session,
		Plugin:    "demo",
		Tool:      "*",
		Expires:   time.Now().Add(time.Minute).Unix(),
	})
}

// callToken signs the token that authorizes one tool call.
func callToken(t *testing.T, key []byte, session, tool string, args json.RawMessage, idem string) string {
	t.Helper()
	hash, err := gateway.ArgsHash(args)
	if err != nil {
		t.Fatalf("hash arguments: %v", err)
	}
	return signToken(t, key, gateway.Claims{
		SessionID:  session,
		Plugin:     "demo",
		Tool:       tool,
		Key:        idem,
		ArgsSHA256: hash,
		Expires:    time.Now().Add(time.Minute).Unix(),
	})
}

// signToken signs arbitrary claims.
func signToken(t *testing.T, key []byte, claims gateway.Claims) string {
	t.Helper()
	token, err := gateway.SignToken(key, claims)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return token
}

// toolContent is the tools/call result the tests read.
type toolContent struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// toolText returns the text of a tools/call response body.
func toolText(t *testing.T, body []byte) (string, bool) {
	t.Helper()
	var message struct {
		Result toolContent `json:"result"`
	}
	if err := json.Unmarshal(body, &message); err != nil {
		t.Fatalf("decode tools/call response %s: %v", body, err)
	}
	if len(message.Result.Content) == 0 {
		t.Fatalf("tools/call response has no content: %s", body)
	}
	return message.Result.Content[0].Text, message.Result.IsError
}

// listedTools returns the tool names of a tools/list response body.
func listedTools(t *testing.T, body []byte) []string {
	t.Helper()
	var message struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &message); err != nil {
		t.Fatalf("decode tools/list response %s: %v", body, err)
	}
	names := make([]string, 0, len(message.Result.Tools))
	for _, tool := range message.Result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestServeDiscoveryToken(t *testing.T) {
	t.Setenv("FIXTURE_TOOLS", "echo")
	s := startShim(t, serveOptions{listen: "", session: "s1"})
	client := newShimClient(t, s.listen)
	token := discoveryToken(t, s.key, "s1")

	status, body := client.post("initialize", 1, map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "test", "version": "1"},
	}, token)
	if status != http.StatusOK {
		t.Fatalf("initialize: status %d: %s", status, body)
	}
	var handshake struct {
		Result struct {
			ProtocolVersion string          `json:"protocolVersion"`
			Capabilities    json.RawMessage `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &handshake); err != nil {
		t.Fatalf("decode initialize response %s: %v", body, err)
	}
	if handshake.Result.ProtocolVersion == "" || len(handshake.Result.Capabilities) == 0 {
		t.Fatalf("initialize response lacks the child's handshake: %s", body)
	}

	status, body = client.post("tools/list", 2, nil, token)
	if status != http.StatusOK {
		t.Fatalf("tools/list: status %d: %s", status, body)
	}
	if names := listedTools(t, body); len(names) != 1 || names[0] != "echo" {
		t.Fatalf("tools/list = %v, want [echo]", names)
	}

	for _, method := range []string{"initialize", "tools/list"} {
		status, body = client.post(method, 3, nil, "")
		if status != http.StatusUnauthorized {
			t.Fatalf("%s without a token: status %d: %s", method, status, body)
		}
	}
}

func TestServeCallToken(t *testing.T) {
	t.Setenv("FIXTURE_TOOLS", "echo")
	s := startShim(t, serveOptions{listen: "", session: "s1"})
	client := newShimClient(t, s.listen)
	args := json.RawMessage(`{"text":"hi"}`)

	status, body := client.post("tools/call", 1, map[string]any{"name": "echo", "arguments": args},
		callToken(t, s.key, "s1", "echo", args, "s1/1/1"))
	if status != http.StatusOK {
		t.Fatalf("tools/call: status %d: %s", status, body)
	}
	if text, isError := toolText(t, body); text != "echo: hi" || isError {
		t.Fatalf("tools/call = %q isError=%v, want echo: hi", text, isError)
	}

	other := json.RawMessage(`{"text":"bye"}`)
	cases := []struct {
		name  string
		token string
	}{
		{"token for another tool", callToken(t, s.key, "s1", "sleep", args, "s1/1/2")},
		{"token for other arguments", callToken(t, s.key, "s1", "echo", other, "s1/1/3")},
		{"expired token", signToken(t, s.key, gateway.Claims{
			SessionID: "s1", Plugin: "demo", Tool: "echo", Key: "s1/1/4",
			ArgsSHA256: mustHash(t, args), Expires: time.Now().Add(-time.Minute).Unix(),
		})},
		{"token from another key", callToken(t, bytes.Repeat([]byte("x"), 32), "s1", "echo", args, "s1/1/5")},
		{"discovery token", discoveryToken(t, s.key, "s1")},
		{"missing token", ""},
	}
	for _, test := range cases {
		status, body = client.post("tools/call", 2, map[string]any{"name": "echo", "arguments": args}, test.token)
		if status != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401: %s", test.name, status, body)
		}
	}
}

// mustHash returns the argument hash a call token carries.
func mustHash(t *testing.T, args json.RawMessage) string {
	t.Helper()
	hash, err := gateway.ArgsHash(args)
	if err != nil {
		t.Fatalf("hash arguments: %v", err)
	}
	return hash
}

func TestServeReplayDedupe(t *testing.T) {
	t.Run("replay returns the stored result", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "calls.jsonl")
		t.Setenv("FIXTURE_TOOLS", "echo")
		t.Setenv("FIXTURE_LOG", logPath)
		s := startShim(t, serveOptions{listen: "", session: "s1"})
		client := newShimClient(t, s.listen)
		args := json.RawMessage(`{"text":"once"}`)
		token := callToken(t, s.key, "s1", "echo", args, "s1/1/1")

		status, first := client.post("tools/call", 1, map[string]any{"name": "echo", "arguments": args}, token)
		if status != http.StatusOK {
			t.Fatalf("first call: status %d: %s", status, first)
		}
		status, second := client.post("tools/call", 2, map[string]any{"name": "echo", "arguments": args},
			callToken(t, s.key, "s1", "echo", args, "s1/1/1"))
		if status != http.StatusOK {
			t.Fatalf("replay: status %d: %s", status, second)
		}
		if text, _ := toolText(t, second); text != "echo: once" {
			t.Fatalf("replay result = %q, want the stored result", text)
		}
		if calls := countCalls(t, logPath); calls != 1 {
			t.Fatalf("the child received %d calls, want 1", calls)
		}
	})

	t.Run("concurrent duplicate is refused", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "calls.jsonl")
		t.Setenv("FIXTURE_TOOLS", "echo,sleep")
		t.Setenv("FIXTURE_LOG", logPath)
		t.Setenv("FIXTURE_SLEEP_MS", "500")
		s := startShim(t, serveOptions{listen: "", session: "s1"})
		client := newShimClient(t, s.listen)
		args := json.RawMessage(`{"text":"dup"}`)

		start := make(chan struct{})
		statuses := make(chan int, 2)
		var wait sync.WaitGroup
		for i := range 2 {
			wait.Add(1)
			go func(id int64) {
				defer wait.Done()
				<-start
				status, _ := client.post("tools/call", id, map[string]any{"name": "sleep", "arguments": args},
					callToken(t, s.key, "s1", "sleep", args, "s1/1/1"))
				statuses <- status
			}(int64(i + 1))
		}
		close(start)
		wait.Wait()
		close(statuses)

		var ok, conflict int
		for status := range statuses {
			switch status {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
				conflict++
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatalf("statuses = %d ok, %d conflict, want one each", ok, conflict)
		}
	})
}

// countCalls counts the tools/call messages the fixture logged.
func countCalls(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read call log: %v", err)
	}
	calls := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var event struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode call log line %q: %v", line, err)
		}
		if event.Method == "tools/call" {
			calls++
		}
	}
	return calls
}

func TestServeMultiplex(t *testing.T) {
	const calls = 16
	t.Setenv("FIXTURE_TOOLS", "echo,sleep")
	t.Setenv("FIXTURE_SLEEP_MS", "200")
	s := startShim(t, serveOptions{listen: "", session: "s1", maxConcurrency: calls})
	client := newShimClient(t, s.listen)

	started := time.Now()
	texts := make(chan string, calls)
	errs := make(chan error, calls)
	var wait sync.WaitGroup
	for i := range calls {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			args, err := json.Marshal(map[string]string{"text": fmt.Sprintf("call-%d", i)})
			if err != nil {
				errs <- err
				return
			}
			status, body := client.post("tools/call", int64(i+1), map[string]any{
				"name": "sleep", "arguments": json.RawMessage(args),
			}, callToken(t, s.key, "s1", "sleep", args, fmt.Sprintf("s1/1/%d", i+1)))
			if status != http.StatusOK {
				errs <- fmt.Errorf("call %d: status %d: %s", i, status, body)
				return
			}
			var message struct {
				Result toolContent `json:"result"`
			}
			if err := json.Unmarshal(body, &message); err != nil {
				errs <- err
				return
			}
			texts <- message.Result.Content[0].Text
		}(i)
	}
	wait.Wait()
	close(texts)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for text := range texts {
		seen[text] = true
	}
	for i := range calls {
		want := fmt.Sprintf("echo: call-%d", i)
		if !seen[want] {
			t.Fatalf("missing result %q among %d results", want, len(seen))
		}
	}
	// The child answers calls concurrently; a serialized child would need
	// calls * FIXTURE_SLEEP_MS.
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("%d calls took %v, want them multiplexed", calls, elapsed)
	}
}

func TestServeChildRestart(t *testing.T) {
	t.Setenv("FIXTURE_TOOLS", "echo")
	t.Setenv("FIXTURE_EXIT_IN_CALL", "1")
	t.Setenv("FIXTURE_STDERR", "fixture: exiting during call\n")
	s := startShim(t, serveOptions{listen: "", session: "s1"})
	client := newShimClient(t, s.listen)
	args := json.RawMessage(`{"text":"boom"}`)

	status, body := client.post("tools/call", 1, map[string]any{"name": "echo", "arguments": args},
		callToken(t, s.key, "s1", "echo", args, "s1/1/1"))
	if status != http.StatusOK {
		t.Fatalf("tools/call: status %d: %s", status, body)
	}
	text, isError := toolText(t, body)
	if !isError || !strings.Contains(text, "fixture: exiting during call") {
		t.Fatalf("tools/call = %q isError=%v, want the child's stderr", text, isError)
	}

	if err := probe(context.Background(), s.listen, 500*time.Millisecond); err == nil {
		t.Fatal("probe passed while the child was down")
	}
	waitReady(t, s)
	if err := probe(context.Background(), s.listen, 2*time.Second); err != nil {
		t.Fatalf("probe after restart: %v", err)
	}
}

func TestServeEnv(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("FIXTURE_TOOLS", "echo")
	t.Setenv("FIXTURE_ENV_FILE", envPath)
	t.Setenv("MAYFLY_SESSION_TOKEN", "session-token-value")
	t.Setenv("MAYFLY_CONFIG", "/etc/mayfly/config.yaml")
	t.Setenv("MAYFLY_SESSION_ID", "s1")
	t.Setenv("MAYFLY_PLUGIN_NAME", "demo")
	t.Setenv("MAYFLY_PLUGIN_DIR", "/mayfly-plugin")
	startShim(t, serveOptions{listen: "", session: "s1"})

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read child environment: %v", err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			values[name] = value
		}
	}
	for _, name := range []string{"MAYFLY_SESSION_TOKEN", "MAYFLY_CONFIG"} {
		if value, ok := values[name]; ok {
			t.Errorf("child environment has %s=%q", name, value)
		}
	}
	for name, want := range map[string]string{
		"MAYFLY_SESSION_ID":  "s1",
		"MAYFLY_PLUGIN_NAME": "demo",
		"MAYFLY_PLUGIN_DIR":  "/mayfly-plugin",
	} {
		if values[name] != want {
			t.Errorf("child environment has %s=%q, want %q", name, values[name], want)
		}
	}
}

func TestProbe(t *testing.T) {
	t.Run("unix", func(t *testing.T) {
		sock := filepath.Join(t.TempDir(), "shim.sock")
		// A restarted shim replaces the previous socket file.
		if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := startShim(t, serveOptions{listen: "unix:" + sock, session: "s1"})

		info, err := os.Stat(sock)
		if err != nil {
			t.Fatalf("stat socket: %v", err)
		}
		if info.Mode().Perm() != 0o660 {
			t.Fatalf("socket mode = %o, want 660", info.Mode().Perm())
		}
		var stderr bytes.Buffer
		if code := runProbe([]string{"--listen", s.listen}, &stderr); code != 0 {
			t.Fatalf("probe exit %d: %s", code, stderr.String())
		}
	})

	t.Run("missing server", func(t *testing.T) {
		var stderr bytes.Buffer
		missing := "unix:" + filepath.Join(t.TempDir(), "absent.sock")
		if code := runProbe([]string{"--listen", missing}, &stderr); code != 1 {
			t.Fatalf("probe exit %d, want 1: %s", code, stderr.String())
		}
	})

	t.Run("bad listen", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := runProbe([]string{"--listen", "localhost"}, &stderr); code != 2 {
			t.Fatalf("probe exit %d, want 2: %s", code, stderr.String())
		}
	})

	t.Run("tcp", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := listener.Addr().String()
		listener.Close()
		s := startShim(t, serveOptions{listen: "tcp:" + addr, session: "s1"})
		var stderr bytes.Buffer
		if code := runProbe([]string{"--listen", s.listen}, &stderr); code != 0 {
			t.Fatalf("probe exit %d: %s", code, stderr.String())
		}
	})
}
