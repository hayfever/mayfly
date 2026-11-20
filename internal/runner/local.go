package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// Local runs sessions as child processes. It is for development and
// integration tests, not for isolation.
type Local struct {
	binary   string
	stateDir string
	logger   *slog.Logger

	mu    sync.Mutex
	procs map[string]*localProc
}

type localProc struct {
	cmd  *exec.Cmd
	log  *os.File
	done chan struct{}
	mu   sync.Mutex
	err  error
}

// NewLocal returns a process backend.
func NewLocal(binary, stateDir string, logger *slog.Logger) (*Local, error) {
	absBinary, err := filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(absBinary); err != nil {
		return nil, fmt.Errorf("agent binary %s: %w", absBinary, err)
	}
	absState, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(absState, "sessions"), 0o750); err != nil {
		return nil, err
	}
	return &Local{
		binary:   absBinary,
		stateDir: absState,
		logger:   logger,
		procs:    map[string]*localProc{},
	}, nil
}

// Start launches the agent process with a session config file. A replacement
// process is started with the same StateDir, so it appends to the existing
// conversation and artifact logs and finds the journal on startup.
func (l *Local) Start(ctx context.Context, spec Spec) (Handle, error) {
	dir := spec.StateDir
	if dir == "" {
		dir = filepath.Join(l.stateDir, "sessions", spec.SessionID)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Handle{}, err
	}
	if err := writePluginSecrets(spec.Runtime.Plugins, spec.PluginSecrets); err != nil {
		return Handle{}, err
	}
	raw, err := json.MarshalIndent(spec.Runtime, "", "  ")
	if err != nil {
		return Handle{}, err
	}
	cfgPath := filepath.Join(dir, "session.json")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		return Handle{}, err
	}
	logPath := filepath.Join(dir, "agent.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return Handle{}, err
	}
	cmd := exec.Command(l.binary, "--config", cfgPath)
	cmd.Dir = dir
	env := map[string]string{}
	for key, value := range spec.Env {
		env[key] = value
	}
	env["MAYFLY_CONFIG"] = cfgPath
	env["MAYFLY_SESSION_TOKEN"] = spec.Token
	cmd.Env = setEnv(baseEnv(os.LookupEnv), env)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return Handle{}, fmt.Errorf("start agent: %w", err)
	}
	proc := &localProc{cmd: cmd, log: logFile, done: make(chan struct{})}
	l.mu.Lock()
	l.procs[spec.SessionID] = proc
	l.mu.Unlock()
	go func() {
		err := cmd.Wait()
		proc.mu.Lock()
		proc.err = err
		proc.mu.Unlock()
		_ = logFile.Close()
		close(proc.done)
		l.logger.Info("session process exited", "session", spec.SessionID, "error", err)
	}()
	return Handle{
		Backend:   "local",
		SessionID: spec.SessionID,
		StateDir:  dir,
		Done:      proc.done,
	}, nil
}

// writePluginSecrets writes each inline plugin secret to its file. The runtime
// reads the files and passes the value only to that plugin's process, so the
// agent never inherits a secret through its own environment.
func writePluginSecrets(plugins []sessioncfg.Plugin, values map[string]map[string]string) error {
	for _, p := range plugins {
		if p.Placement != "inline" || len(p.Secrets) == 0 {
			continue
		}
		if p.SecretsDir == "" {
			return fmt.Errorf("plugin %q: secrets directory is not set", p.Name)
		}
		if err := os.MkdirAll(p.SecretsDir, 0o700); err != nil {
			return fmt.Errorf("plugin %q: create secrets directory: %w", p.Name, err)
		}
		// MkdirAll leaves an existing directory's mode alone, and a directory
		// from an earlier start may have been wider.
		if err := os.Chmod(p.SecretsDir, 0o700); err != nil {
			return fmt.Errorf("plugin %q: restrict secrets directory: %w", p.Name, err)
		}
		for _, secret := range p.Secrets {
			value, ok := values[p.Name][secret.Env]
			if !ok {
				return fmt.Errorf("plugin %q: environment variable %s is not set", p.Name, secret.Env)
			}
			path := filepath.Join(p.SecretsDir, secret.Env)
			if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
				return fmt.Errorf("plugin %q: write secret %s: %w", p.Name, secret.Env, err)
			}
			// WriteFile applies its mode only when it creates the file; a
			// replacement start must not keep a wider earlier mode.
			if err := os.Chmod(path, 0o600); err != nil {
				return fmt.Errorf("plugin %q: restrict secret %s: %w", p.Name, secret.Env, err)
			}
		}
	}
	return nil
}

// Stop signals the agent and waits for it to exit.
func (l *Local) Stop(ctx context.Context, handle Handle, force bool) error {
	l.mu.Lock()
	proc := l.procs[handle.SessionID]
	l.mu.Unlock()
	if proc == nil {
		return nil
	}
	if proc.cmd.Process == nil {
		return nil
	}
	if force {
		_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGKILL)
	} else {
		_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGTERM)
	}
	select {
	case <-proc.done:
	case <-ctx.Done():
		_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGKILL)
		<-proc.done
	case <-time.After(20 * time.Second):
		_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGKILL)
		<-proc.done
	}
	l.mu.Lock()
	delete(l.procs, handle.SessionID)
	l.mu.Unlock()
	return nil
}

// Logs returns the tail of the agent process log.
func (l *Local) Logs(_ context.Context, handle Handle, maxBytes int64) (string, error) {
	return tailFile(filepath.Join(handle.StateDir, "agent.log"), maxBytes)
}

func tailFile(path string, maxBytes int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	offset := st.Size() - maxBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// passEnv lists the host variables a local session inherits: what a shell,
// a TLS client, and a stdio MCP child need to run. Everything else, including
// MAYFLY_API_TOKEN, the LLM key, and provider tokens such as NOTION_TOKEN, is
// dropped. runtime.env adds variables explicitly.
var passEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TZ",
	"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "TERM",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"MAYFLY_LOG_LEVEL",
}

// baseEnv is the minimal environment a local session starts from.
func baseEnv(lookup func(string) (string, bool)) []string {
	out := make([]string, 0, len(passEnv))
	for _, name := range passEnv {
		if value, ok := lookup(name); ok {
			out = append(out, name+"="+value)
		}
	}
	return out
}

func setEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[name]; replaced {
			continue
		}
		out = append(out, entry)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}
