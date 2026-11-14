package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// fixturePath is the fixture server, built once for the package.
var fixturePath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mayfly-pluginhost-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixture directory: %v\n", err)
		os.Exit(1)
	}
	fixturePath = filepath.Join(dir, "fixture")
	build := exec.Command("go", "build", "-o", fixturePath, "./testdata/fixture/main.go")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fixture: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fixturePlugin is one inline plugin that runs the fixture server.
func fixturePlugin(name, dir string, tools ...string) sessioncfg.Plugin {
	defs := make([]sessioncfg.PluginTool, 0, len(tools))
	for _, tool := range tools {
		defs = append(defs, sessioncfg.PluginTool{Name: tool, TimeoutSeconds: 10})
	}
	return sessioncfg.Plugin{
		Name:               name,
		Placement:          "inline",
		Dir:                dir,
		Command:            fixturePath,
		Workdir:            "plugin",
		Tools:              defs,
		InitTimeoutSeconds: 10,
		MaxRestarts:        2,
		MaxOutputBytes:     sessioncfg.PluginMaxOutputBytes,
		StderrBytes:        1 << 20,
	}
}

// startHost starts plugins and registers Close, so a failing test still stops
// its processes.
func startHost(t *testing.T, opts Options, defs ...sessioncfg.Plugin) (*Host, []error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	host, errs := Start(ctx, defs, opts)
	t.Cleanup(host.Close)
	return host, errs
}

// readEnvFile reads the environment dump the fixture writes.
func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read environment: %v", err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("environment line %q has no =", line)
		}
		values[name] = value
	}
	return values
}

// readEvents reads the fixture's JSONL call log.
func readEvents(t *testing.T, path string) []map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read log: %v", err)
	}
	var events []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var event map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

// waitFor polls until condition holds or the test gives up.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitForHostExit waits until the host has seen a plugin process exit. The
// kernel reports a pid as a zombie a few milliseconds before the host's Wait
// returns, so a test that watched /proc only would race the restart check.
func waitForHostExit(t *testing.T, host *Host, name string) {
	t.Helper()
	host.mu.Lock()
	rt := host.byPlugin[name]
	host.mu.Unlock()
	if rt == nil || rt.client == nil {
		t.Fatalf("plugin %q is not running", name)
	}
	exited := rt.client.Exited()
	waitFor(t, "the host to see the plugin exit", func() bool {
		select {
		case <-exited:
			return true
		default:
			return false
		}
	})
}

// readPID returns the process id the fixture wrote.
func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("pid %q: %v", data, err)
	}
	return pid
}

func TestHostEnvExact(t *testing.T) {
	t.Setenv("MAYFLY_SESSION_TOKEN", "session-token")
	t.Setenv("MAYFLY_CONFIG", "/etc/mayfly/config.yaml")
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("TZ", "UTC")
	t.Setenv("SSL_CERT_FILE", "/certs/ca.pem")
	t.Setenv("SSL_CERT_DIR", "/certs")
	t.Setenv("HTTP_PROXY", "http://proxy.example:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Setenv("NO_PROXY", "localhost")
	t.Setenv("https_proxy", "http://lower.example:3128")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/ssh-agent.sock")
	t.Setenv("PATH", "/usr/local/sbin:/usr/bin")

	dir := t.TempDir()
	secrets := t.TempDir()
	if err := os.WriteFile(filepath.Join(secrets, "API_TOKEN"), []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "env")
	plugin := fixturePlugin("envprobe", dir, "echo")
	plugin.Env = map[string]string{
		"PLUGIN_MODE":      "test",
		"FIXTURE_ENV_FILE": envFile,
		"FIXTURE_TOOLS":    "echo",
	}
	plugin.Secrets = []sessioncfg.PluginSecret{{Env: "API_TOKEN"}}
	plugin.SecretsDir = secrets
	opts := Options{SessionID: "sess-1", Agent: "watcher", WorkspaceDir: "/workspace/sess-1"}
	host, errs := startHost(t, opts, plugin)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if got := len(host.Tools()); got != 1 {
		t.Fatalf("tools = %d, want echo", got)
	}

	want := map[string]string{
		"PATH":                 filepath.Join(dir, "bin") + ":/usr/local/sbin:/usr/bin",
		"HOME":                 filepath.Join(pluginTmpRoot, "envprobe", "home"),
		"TMPDIR":               filepath.Join(pluginTmpRoot, "envprobe", "tmp"),
		"LANG":                 "C.UTF-8",
		"TZ":                   "UTC",
		"SSL_CERT_FILE":        "/certs/ca.pem",
		"SSL_CERT_DIR":         "/certs",
		"HTTP_PROXY":           "http://proxy.example:3128",
		"HTTPS_PROXY":          "http://proxy.example:3128",
		"NO_PROXY":             "localhost",
		"https_proxy":          "http://lower.example:3128",
		"MAYFLY_PLUGIN_NAME":   "envprobe",
		"MAYFLY_PLUGIN_DIR":    dir,
		"MAYFLY_SESSION_ID":    "sess-1",
		"MAYFLY_WORKSPACE_DIR": "/workspace/sess-1",
		"PLUGIN_MODE":          "test",
		"FIXTURE_ENV_FILE":     envFile,
		"FIXTURE_TOOLS":        "echo",
		"API_TOKEN":            "s3cret",
	}
	got := readEnvFile(t, envFile)
	if !maps.Equal(got, want) {
		for name, value := range want {
			if other, ok := got[name]; !ok {
				t.Errorf("missing %s=%q", name, value)
			} else if other != value {
				t.Errorf("%s = %q, want %q", name, other, value)
			}
		}
		for name, value := range got {
			if _, ok := want[name]; !ok {
				t.Errorf("unexpected %s=%q", name, value)
			}
		}
	}
	for _, name := range []string{"MAYFLY_SESSION_TOKEN", "MAYFLY_CONFIG", "SSH_AUTH_SOCK"} {
		if value, ok := got[name]; ok {
			t.Errorf("%s was inherited: %q", name, value)
		}
	}
}

func TestHostVerifyTools(t *testing.T) {
	t.Run("undeclared tool is hidden", func(t *testing.T) {
		plugin := fixturePlugin("verify", t.TempDir(), "echo")
		plugin.Env = map[string]string{"FIXTURE_TOOLS": "echo,secret"}
		host, errs := startHost(t, Options{}, plugin)
		if len(errs) != 0 {
			t.Fatalf("errors: %v", errs)
		}
		if tools := host.Tools(); len(tools) != 1 || tools[0].Name != "plugin_verify_echo" {
			t.Fatalf("tools = %+v", tools)
		}
		if hidden := host.Hidden()["verify"]; len(hidden) != 1 || hidden[0] != "secret" {
			t.Fatalf("hidden = %v", hidden)
		}
	})

	t.Run("missing declared tool fails start", func(t *testing.T) {
		plugin := fixturePlugin("verify", t.TempDir(), "echo", "missing")
		host, errs := startHost(t, Options{}, plugin)
		if len(errs) != 1 {
			t.Fatalf("errors = %v", errs)
		}
		for _, part := range []string{`plugin "verify"`, `declared tool "missing" is not reported`} {
			if !strings.Contains(errs[0].Error(), part) {
				t.Fatalf("error %q does not contain %q", errs[0], part)
			}
		}
		if tools := host.Tools(); len(tools) != 0 {
			t.Fatalf("tools = %+v", tools)
		}
	})

	t.Run("non-object schema fails start", func(t *testing.T) {
		plugin := fixturePlugin("verify", t.TempDir(), "echo", "odd")
		plugin.Env = map[string]string{"FIXTURE_TOOLS": "echo,odd", "FIXTURE_BAD_SCHEMA": "odd"}
		_, errs := startHost(t, Options{}, plugin)
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "input schema is not a JSON object") {
			t.Fatalf("errors = %v", errs)
		}
	})

	t.Run("empty schema becomes an object", func(t *testing.T) {
		plugin := fixturePlugin("verify", t.TempDir(), "echo")
		plugin.Env = map[string]string{"FIXTURE_TOOLS": "echo", "FIXTURE_NO_SCHEMA": "echo"}
		host, errs := startHost(t, Options{}, plugin)
		if len(errs) != 0 {
			t.Fatalf("errors: %v", errs)
		}
		tool, ok := host.Tool("plugin_verify_echo")
		if !ok {
			t.Fatal("tool is missing")
		}
		if string(tool.InputSchema) != defaultObjectSchema {
			t.Fatalf("schema = %s", tool.InputSchema)
		}
	})

	t.Run("required failure is reported", func(t *testing.T) {
		plugin := fixturePlugin("required", t.TempDir(), "echo")
		plugin.Command = "mayfly-fixture-does-not-exist"
		plugin.Required = true
		host, errs := startHost(t, Options{}, plugin)
		if len(errs) != 1 {
			t.Fatalf("errors = %v", errs)
		}
		err := host.RequiredErr()
		if err == nil {
			t.Fatal("RequiredErr is nil")
		}
		if !strings.Contains(err.Error(), `plugin "required"`) {
			t.Fatalf("RequiredErr = %q", err)
		}
	})
}

func TestHostRestart(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.jsonl")
	plugin := fixturePlugin("restart", t.TempDir(), "echo")
	plugin.Env = map[string]string{
		"FIXTURE_TOOLS":      "echo",
		"FIXTURE_EXIT_AFTER": "1",
		"FIXTURE_LOG":        logPath,
		"FIXTURE_STDERR":     "fixture stderr\n",
	}
	host, errs := startHost(t, Options{}, plugin)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	ctx := context.Background()
	for call := 1; call <= 3; call++ {
		out, isErr, err := host.Call(ctx, "plugin_restart_echo", json.RawMessage(`{"text":"call"}`), "key", "")
		if err != nil || isErr || !strings.Contains(out, "echo: call") {
			t.Fatalf("call %d = %q error=%v err=%v", call, out, isErr, err)
		}
		waitForHostExit(t, host, "restart")
	}
	_, _, err := host.Call(ctx, "plugin_restart_echo", json.RawMessage(`{"text":"four"}`), "key", "")
	if err == nil {
		t.Fatal("a fourth call succeeded after the restart budget")
	}
	for _, part := range []string{`plugin "restart"`, "restart limit of 2", "fixture stderr"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("error %q does not contain %q", err, part)
		}
	}
	if events := callsOnly(readEvents(t, logPath)); len(events) != 3 {
		t.Fatalf("fixture received %d calls, want 3", len(events))
	}
}

func TestHostCrashDuringCall(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.jsonl")
	plugin := fixturePlugin("crash", t.TempDir(), "echo")
	plugin.Env = map[string]string{
		"FIXTURE_TOOLS":        "echo",
		"FIXTURE_EXIT_IN_CALL": "1",
		"FIXTURE_LOG":          logPath,
		"FIXTURE_STDERR":       "fixture crashed during call\n",
	}
	host, errs := startHost(t, Options{}, plugin)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	out, isErr, err := host.Call(context.Background(), "plugin_crash_echo", json.RawMessage(`{"text":"x"}`), "key", "")
	if err != nil {
		t.Fatalf("call returned a dispatch error: %v", err)
	}
	if !isErr {
		t.Fatalf("call = %q, want an error result", out)
	}
	if !strings.Contains(out, "fixture crashed during call") {
		t.Fatalf("result %q does not carry stderr", out)
	}
	if events := callsOnly(readEvents(t, logPath)); len(events) != 1 {
		t.Fatalf("fixture received %d calls, want 1 (no retry)", len(events))
	}
}

func TestHostTimeoutCancels(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.jsonl")
	plugin := fixturePlugin("slow", t.TempDir(), "sleep", "echo")
	plugin.Tools[0].TimeoutSeconds = 1
	plugin.Env = map[string]string{
		"FIXTURE_TOOLS":    "sleep,echo",
		"FIXTURE_SLEEP_MS": "1500",
		"FIXTURE_LOG":      logPath,
	}
	host, errs := startHost(t, Options{}, plugin)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	ctx := context.Background()
	started := time.Now()
	out, isErr, err := host.Call(ctx, "plugin_slow_sleep", json.RawMessage(`{}`), "key", "")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	if !isErr || !strings.Contains(out, "did not answer within 1s") {
		t.Fatalf("result = %q error=%v", out, isErr)
	}
	if elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	var cancelled map[string]json.RawMessage
	waitFor(t, "cancel notification", func() bool {
		for _, event := range readEvents(t, logPath) {
			if string(event["method"]) == `"notifications/cancelled"` {
				cancelled = event
				return true
			}
		}
		return false
	})
	var params struct {
		RequestID int64  `json:"requestId"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(cancelled["params"], &params); err != nil {
		t.Fatal(err)
	}
	if params.RequestID == 0 || params.Reason != "timeout" {
		t.Fatalf("cancelled = %+v", params)
	}
	// Let the late response arrive before the next call: it must be ignored.
	time.Sleep(700 * time.Millisecond)
	out, isErr, err = host.Call(ctx, "plugin_slow_echo", json.RawMessage(`{"text":"next"}`), "key", "")
	if err != nil || isErr || out != "echo: next" {
		t.Fatalf("next call = %q error=%v err=%v", out, isErr, err)
	}
}

func TestHostMeta(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.jsonl")
	plugin := fixturePlugin("meta", t.TempDir(), "echo", "counter")
	plugin.Env = map[string]string{"FIXTURE_TOOLS": "echo", "FIXTURE_COUNTER": "1", "FIXTURE_LOG": logPath}
	opts := Options{SessionID: "sess-9", Agent: "watcher"}
	host, errs := startHost(t, opts, plugin)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	out, isErr, err := host.Call(context.Background(), "plugin_meta_echo",
		json.RawMessage(`{"text":"hi"}`), "sess-9/0/1", "")
	if err != nil || isErr || out != "echo: hi" {
		t.Fatalf("call = %q error=%v err=%v", out, isErr, err)
	}
	events := callsOnly(readEvents(t, logPath))
	if len(events) != 1 {
		t.Fatalf("events = %v", events)
	}
	var meta map[string]string
	if err := json.Unmarshal(events[0]["meta"], &meta); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"mayfly.io/idempotency-key": "sess-9/0/1",
		"mayfly.io/session":         "sess-9",
		"mayfly.io/agent":           "watcher",
	}
	if !maps.Equal(meta, want) {
		t.Fatalf("_meta = %v, want %v", meta, want)
	}
	if arguments := string(events[0]["arguments"]); arguments != `{"text":"hi"}` {
		t.Fatalf("arguments = %s", arguments)
	}
	for count := 1; count <= 2; count++ {
		out, isErr, err := host.Call(context.Background(), "plugin_meta_counter",
			json.RawMessage(`{}`), fmt.Sprintf("sess-9/0/%d", count+1), "")
		if err != nil || isErr || out != strconv.Itoa(count) {
			t.Fatalf("counter call %d = %q error=%v err=%v", count, out, isErr, err)
		}
	}
}

func TestHostStderrCap(t *testing.T) {
	t.Run("file cap keeps one marker", func(t *testing.T) {
		artifacts := t.TempDir()
		plugin := fixturePlugin("noisy", t.TempDir(), "echo")
		plugin.StderrBytes = 64
		plugin.Env = map[string]string{
			"FIXTURE_TOOLS":  "echo",
			"FIXTURE_STDERR": strings.Repeat("x", 200) + "\n",
		}
		_, errs := startHost(t, Options{ArtifactsDir: artifacts}, plugin)
		if len(errs) != 0 {
			t.Fatalf("errors: %v", errs)
		}
		path := filepath.Join(artifacts, "plugins", "noisy.stderr.log")
		var content string
		waitFor(t, "the stderr marker", func() bool {
			data, err := os.ReadFile(path)
			content = string(data)
			return err == nil && strings.Contains(content, "[stderr truncated]")
		})
		if strings.Count(content, "[stderr truncated]") != 1 {
			t.Fatalf("content = %q, want one marker", content)
		}
		if len(content) > 64+len(stderrMarker) {
			t.Fatalf("content is %d bytes, cap is 64", len(content))
		}
		if !strings.HasPrefix(content, strings.Repeat("x", 64)) {
			t.Fatalf("content = %q", content)
		}
	})

	t.Run("debug lines are cut", func(t *testing.T) {
		recorder := &lineRecorder{}
		plugin := fixturePlugin("verbose", t.TempDir(), "echo")
		plugin.Env = map[string]string{
			"FIXTURE_TOOLS":  "echo",
			"FIXTURE_STDERR": strings.Repeat("y", 3000),
		}
		_, errs := startHost(t, Options{
			ArtifactsDir: t.TempDir(),
			Logger:       slog.New(recorder),
		}, plugin)
		if len(errs) != 0 {
			t.Fatalf("errors: %v", errs)
		}
		waitFor(t, "a debug line", func() bool { return len(recorder.lines()) > 0 })
		for _, line := range recorder.lines() {
			if len(line) > maxStderrLine {
				t.Fatalf("debug line is %d bytes, cut is %d", len(line), maxStderrLine)
			}
		}
	})
}

// lineRecorder captures the plugin stderr debug lines.
type lineRecorder struct {
	mu    sync.Mutex
	saved []string
}

func (r *lineRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *lineRecorder) Handle(_ context.Context, record slog.Record) error {
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "line" {
			r.mu.Lock()
			r.saved = append(r.saved, attr.Value.String())
			r.mu.Unlock()
		}
		return true
	})
	return nil
}

func (r *lineRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *lineRecorder) WithGroup(string) slog.Handler      { return r }

func (r *lineRecorder) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.saved...)
}

func TestHostTruncatesOutput(t *testing.T) {
	plugin := fixturePlugin("terse", t.TempDir(), "echo")
	plugin.MaxOutputBytes = 16
	host, errs := startHost(t, Options{}, plugin)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	out, isErr, err := host.Call(context.Background(), "plugin_terse_echo",
		json.RawMessage(`{"text":"`+strings.Repeat("a", 64)+`"}`), "key", "")
	if err != nil || isErr {
		t.Fatalf("call error=%v err=%v", isErr, err)
	}
	marker := "\n(truncated at 16 bytes)"
	if want := "echo: " + strings.Repeat("a", 10) + marker; out != want {
		t.Fatalf("result = %q, want %q", out, want)
	}
}

func TestHostClose(t *testing.T) {
	if testing.Short() {
		t.Skip("Close waits out its two grace periods")
	}
	for _, tc := range []struct {
		name    string
		env     map[string]string
		lowest  time.Duration
		highest time.Duration
	}{
		{
			name:    "ignores stdin EOF",
			env:     map[string]string{"FIXTURE_IGNORE_STDIN_EOF": "1"},
			lowest:  1500 * time.Millisecond,
			highest: 3 * time.Second,
		},
		{
			name:    "ignores SIGTERM",
			env:     map[string]string{"FIXTURE_IGNORE_STDIN_EOF": "1", "FIXTURE_IGNORE_SIGTERM": "1"},
			lowest:  3500 * time.Millisecond,
			highest: 6 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidPath := filepath.Join(t.TempDir(), "pid")
			plugin := fixturePlugin("stubborn", t.TempDir(), "echo")
			plugin.Env = map[string]string{"FIXTURE_TOOLS": "echo", "FIXTURE_PID_FILE": pidPath}
			maps.Copy(plugin.Env, tc.env)
			host, errs := startHost(t, Options{Logger: discardLogger()}, plugin)
			if len(errs) != 0 {
				t.Fatalf("errors: %v", errs)
			}
			pid := readPID(t, pidPath)
			started := time.Now()
			host.Close()
			elapsed := time.Since(started)
			if elapsed < tc.lowest || elapsed > tc.highest {
				t.Fatalf("Close took %s, want %s to %s", elapsed, tc.lowest, tc.highest)
			}
			if err := syscall.Kill(pid, 0); err == nil {
				t.Fatalf("process %d is still running", pid)
			}
		})
	}
}

// callsOnly keeps the tools/call events of the fixture log.
func callsOnly(events []map[string]json.RawMessage) []map[string]json.RawMessage {
	var out []map[string]json.RawMessage
	for _, event := range events {
		if string(event["method"]) == `"tools/call"` {
			out = append(out, event)
		}
	}
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
