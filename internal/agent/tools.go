package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hayden/mayfly/internal/llm"
	"github.com/hayden/mayfly/internal/mcp"
)

const (
	maxReadBytes     = 512 << 10
	maxWriteBytes    = 4 << 20
	maxToolOutput    = 64 << 10
	maxListedEntries = 500
	mcpCallTimeout   = 120 * time.Second
)

type toolset struct {
	root         *os.Root
	rootPath     string
	allowShell   bool
	shellTimeout time.Duration
	mcp          *mcp.Manager
}

func newToolset(workspace string, allowShell bool, shellTimeout time.Duration, manager *mcp.Manager) (*toolset, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, fmt.Errorf("open workspace: %w", err)
	}
	return &toolset{
		root:         root,
		rootPath:     workspace,
		allowShell:   allowShell,
		shellTimeout: shellTimeout,
		mcp:          manager,
	}, nil
}

func (t *toolset) Close() error { return t.root.Close() }

// Tools returns the OpenAI tool declarations for built-ins and MCP tools.
func (t *toolset) Tools() []llm.Tool {
	tools := []llm.Tool{
		llm.NewTool("read_file", "Read a UTF-8 text file from the workspace.", json.RawMessage(`{
			"type":"object",
			"properties":{"path":{"type":"string","description":"Path relative to the workspace"}},
			"required":["path"]
		}`)),
		llm.NewTool("write_file", "Create or replace a file in the workspace. Parent directories are created.", json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"Path relative to the workspace"},
				"content":{"type":"string","description":"Full file content"}
			},
			"required":["path","content"]
		}`)),
		llm.NewTool("list_files", "List one directory in the workspace.", json.RawMessage(`{
			"type":"object",
			"properties":{"path":{"type":"string","description":"Directory relative to the workspace (default .)"}}
		}`)),
	}
	if t.allowShell {
		tools = append(tools, llm.NewTool("run_command", "Run a shell command inside the workspace. Returns exit code, stdout, and stderr.", json.RawMessage(`{
			"type":"object",
			"properties":{
				"command":{"type":"string","description":"Command passed to /bin/sh -c"},
				"timeout_seconds":{"type":"integer","description":"Optional timeout in seconds"}
			},
			"required":["command"]
		}`)))
	}
	if t.mcp != nil {
		for _, mt := range t.mcp.Tools() {
			schema := mt.InputSchema
			if len(schema) == 0 || string(schema) == "null" {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			desc := mt.Description
			if desc == "" {
				desc = "MCP tool " + mt.Name + " from server " + mt.Server
			}
			tools = append(tools, llm.NewTool(mt.Name, desc, schema))
		}
	}
	return tools
}

// Call executes a built-in or MCP tool.
func (t *toolset) Call(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	switch name {
	case "read_file":
		return t.readFile(args)
	case "write_file":
		return t.writeFile(args)
	case "list_files":
		return t.listFiles(args)
	case "run_command":
		if !t.allowShell {
			return "", false, errors.New("run_command is disabled for this session")
		}
		return t.runCommand(ctx, args)
	}
	if t.mcp != nil {
		callCtx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
		defer cancel()
		out, isErr, err := t.mcp.Call(callCtx, name, args)
		if err != nil {
			return "", false, err
		}
		return truncate(out, maxToolOutput), isErr, nil
	}
	return "", false, fmt.Errorf("unknown tool %q", name)
}

func (t *toolset) readFile(args json.RawMessage) (string, bool, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", false, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.Path == "" {
		return "", false, errors.New("path is required")
	}
	info, err := t.root.Stat(in.Path)
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return "", false, fmt.Errorf("%s is a directory", in.Path)
	}
	if info.Size() > maxReadBytes {
		return "", false, fmt.Errorf("file is too large (%d bytes, limit %d)", info.Size(), maxReadBytes)
	}
	data, err := t.root.ReadFile(in.Path)
	if err != nil {
		return "", false, err
	}
	return string(data), false, nil
}

func (t *toolset) writeFile(args json.RawMessage) (string, bool, error) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", false, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.Path == "" {
		return "", false, errors.New("path is required")
	}
	if len(in.Content) > maxWriteBytes {
		return "", false, fmt.Errorf("content is too large (%d bytes, limit %d)", len(in.Content), maxWriteBytes)
	}
	if dir := filepath.Dir(in.Path); dir != "." && dir != "" {
		if err := t.root.MkdirAll(dir, 0o755); err != nil {
			return "", false, err
		}
	}
	if err := t.root.WriteFile(in.Path, []byte(in.Content), 0o644); err != nil {
		return "", false, err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), false, nil
}

func (t *toolset) listFiles(args json.RawMessage) (string, bool, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", false, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.Path == "" {
		in.Path = "."
	}
	f, err := t.root.Open(in.Path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	entries, err := f.ReadDir(maxListedEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, err
	}
	type row struct {
		Name string `json:"name"`
		Size int64  `json:"size,omitempty"`
		Dir  bool   `json:"dir,omitempty"`
	}
	rows := make([]row, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		r := row{Name: filepath.ToSlash(filepath.Join(in.Path, e.Name())), Dir: e.IsDir()}
		if !e.IsDir() {
			r.Size = info.Size()
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	out, _ := json.MarshalIndent(rows, "", "  ")
	if len(entries) > maxListedEntries {
		return string(out) + "\n(truncated)", false, nil
	}
	return string(out), false, nil
}

func (t *toolset) runCommand(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var in struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", false, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(in.Command) == "" {
		return "", false, errors.New("command is required")
	}
	timeout := t.shellTimeout
	if in.TimeoutSeconds > 0 {
		requested := time.Duration(in.TimeoutSeconds) * time.Second
		if requested < timeout {
			timeout = requested
		}
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command("/bin/sh", "-c", in.Command)
	cmd.Dir = t.rootPath
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout := newCapBuffer(maxToolOutput)
	stderr := newCapBuffer(maxToolOutput)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return "", false, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		waitErr = <-done
	}
	exitCode := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if waitErr != nil {
		return "", false, waitErr
	}
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d", exitCode)
	if runCtx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(&b, "\ntimed_out: true")
	}
	b.WriteString("\nstdout:\n")
	b.WriteString(stdout.String())
	if stdout.Truncated() {
		b.WriteString("\n(stdout truncated)")
	}
	b.WriteString("\nstderr:\n")
	b.WriteString(stderr.String())
	if stderr.Truncated() {
		b.WriteString("\n(stderr truncated)")
	}
	return b.String(), exitCode != 0, nil
}

// capBuffer stores at most max bytes and remembers truncation.
type capBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func newCapBuffer(max int) *capBuffer { return &capBuffer{max: max} }

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.max - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *capBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n(truncated at %d bytes)", max)
}
