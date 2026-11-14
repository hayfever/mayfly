package pluginsvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hayfever/mayfly/internal/k8s"
	"github.com/hayfever/mayfly/internal/plugin"
)

const (
	// shimBinary is the shim's name when it is looked up on PATH.
	shimBinary = "mayfly-plugin-shim"
	// restartDelay and restartMaxDelay bound the back-off of a shim that
	// exits. The shim restarts its own child; this covers the shim itself.
	restartDelay    = time.Second
	restartMaxDelay = 30 * time.Second
	// stopTimeout bounds the wait for a shim to stop after SIGTERM.
	stopTimeout = 5 * time.Second
)

// LocalConfig configures the local backend's service children.
type LocalConfig struct {
	// StateDir is runtime.state_dir. Sockets and key files live under it.
	StateDir string
	// Manifests returns the bundle's manifests.
	Manifests func() []plugin.Manifest
	// Shim is the shim executable. Empty looks next to the running binary,
	// then on PATH, then at the installed path.
	Shim   string
	Logger *slog.Logger
}

// Local runs one service plugin per shim child on a Unix socket. The local
// backend has no cluster, so the objects a Reconciler would create have no
// meaning here.
type Local struct {
	stateDir  string
	shimSpec  string
	shim      string
	manifests func() []plugin.Manifest
	logger    *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	children map[string]*localChild
}

// localChild is one supervised shim process and the socket it serves.
type localChild struct {
	socket  string
	key     []byte
	command []string
	dir     string
	env     []string
	logger  *slog.Logger
}

// NewLocal records the state directory and manifest source. It starts no
// process, and needs no shim, until Start finds a service plugin to run.
func NewLocal(cfg LocalConfig) (*Local, error) {
	if cfg.Manifests == nil {
		return nil, errors.New("a manifest source is required")
	}
	if cfg.StateDir == "" {
		return nil, errors.New("a state directory is required")
	}
	stateDir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("state directory %s: %w", cfg.StateDir, err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Local{
		stateDir:  stateDir,
		shimSpec:  cfg.Shim,
		manifests: cfg.Manifests,
		logger:    logger,
		children:  map[string]*localChild{},
	}, nil
}

// Start launches one child per service plugin. The supervisor restarts a
// child that exits, until Close runs.
func (l *Local) Start(ctx context.Context) error {
	l.ctx, l.cancel = context.WithCancel(ctx)
	manifests := serviceManifests(l.manifests())
	if len(manifests) == 0 {
		return nil
	}
	shim, err := resolveShim(l.shimSpec)
	if err != nil {
		return err
	}
	l.shim = shim
	if err := os.MkdirAll(filepath.Join(l.stateDir, "plugins"), 0o700); err != nil {
		return fmt.Errorf("create plugin state directory: %w", err)
	}
	names := make([]string, 0, len(manifests))
	for name := range manifests {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		manifest := manifests[name]
		if manifest.Service.Transport == "http" {
			l.Close()
			return fmt.Errorf("plugin %q: service.transport http is not supported on the local backend", name)
		}
		if err := l.start(manifest); err != nil {
			l.Close()
			return err
		}
	}
	return nil
}

// start prepares one socket, key file, and supervisor.
func (l *Local) start(manifest plugin.Manifest) error {
	key, err := newGatewayKey()
	if err != nil {
		return fmt.Errorf("plugin %q: %w", manifest.Name, err)
	}
	dir := filepath.Join(l.stateDir, "plugins", manifest.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("plugin %q: create state directory: %w", manifest.Name, err)
	}
	keyFile := filepath.Join(dir, "gateway-key")
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		return fmt.Errorf("plugin %q: write gateway key: %w", manifest.Name, err)
	}
	socket := filepath.Join(l.stateDir, "plugins", manifest.Name+".sock")
	command := []string{
		l.shim, "serve",
		"--listen", "unix:" + socket,
		"--key-file", keyFile,
		"--max-concurrency", fmt.Sprint(manifest.Service.Concurrency),
		"--init-timeout", manifest.InitTimeout().String(),
		"--", manifest.Local.Command,
	}
	command = append(command, manifest.Local.Args...)

	env, err := localEnv(manifest, filepath.Join(dir, "tmp"))
	if err != nil {
		return fmt.Errorf("plugin %q: %w", manifest.Name, err)
	}
	child := &localChild{
		socket:  socket,
		key:     key,
		command: command,
		dir:     manifest.Dir,
		env:     env,
		logger:  l.logger.With("plugin", manifest.Name),
	}
	l.mu.Lock()
	l.children[manifest.Name] = child
	l.mu.Unlock()
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		child.run(l.ctx)
	}()
	return nil
}

// ServiceURL returns the socket URL and gateway key of one running child.
// The gateway speaks MCP to the shim's fixed /mcp path over the socket.
func (l *Local) ServiceURL(name string) (string, []byte, error) {
	l.mu.Lock()
	child := l.children[name]
	l.mu.Unlock()
	if child == nil {
		return "", nil, fmt.Errorf("plugin %q is not a running service plugin", name)
	}
	return "unix:" + child.socket, child.key, nil
}

// Close stops every child and waits for the supervisors to finish.
func (l *Local) Close() {
	if l.cancel != nil {
		l.cancel()
	}
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopTimeout):
		l.logger.Warn("plugin service children did not stop in time")
	}
}

// run supervises one shim process until ctx ends.
func (c *localChild) run(ctx context.Context) {
	delay := restartDelay
	for {
		if ctx.Err() != nil {
			return
		}
		cmd := exec.Command(c.command[0], c.command[1:]...)
		cmd.Dir = c.dir
		cmd.Env = c.env
		cmd.Stdout = io.Discard
		cmd.Stderr = &logWriter{logger: c.logger}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			c.logger.Error("start plugin service", "error", err)
			if !sleepContext(ctx, delay) {
				return
			}
			delay = nextDelay(delay)
			continue
		}
		stopped := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				stopGroup(cmd)
			case <-stopped:
			}
		}()
		err := cmd.Wait()
		close(stopped)
		if ctx.Err() != nil {
			return
		}
		c.logger.Warn("plugin service exited", "error", err)
		if !sleepContext(ctx, delay) {
			return
		}
		delay = nextDelay(delay)
	}
}

// stopGroup asks the process group to stop, then kills it.
func stopGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.AfterFunc(2*time.Second, func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
}

// nextDelay doubles a restart delay up to the cap.
func nextDelay(delay time.Duration) time.Duration {
	delay *= 2
	if delay > restartMaxDelay {
		return restartMaxDelay
	}
	return delay
}

// sleepContext waits for d unless ctx ends first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// localPassEnv lists the parent variables a service plugin inherits: what a
// process needs to find its programs, its locale, and a TLS client. Nothing
// else is inherited, so the control plane's provider keys, webhook secrets,
// and MAYFLY_* variables never reach the plugin.
var localPassEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TZ",
	"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "TERM",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

// localEnv is the shim's environment: the inherited toolchain variables, a
// private TMPDIR, the manifest's env, and the secrets read from the mayflyd
// environment. The shim removes every other MAYFLY_* variable before it
// starts the plugin.
func localEnv(manifest plugin.Manifest, tmpDir string) ([]string, error) {
	values := map[string]string{}
	for _, name := range localPassEnv {
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("plugin %q: create temporary directory: %w", manifest.Name, err)
	}
	values["TMPDIR"] = tmpDir
	for key, value := range manifest.Env {
		values[key] = value
	}
	for _, secret := range manifest.Secrets {
		value, ok := os.LookupEnv(secret.Env)
		if !ok {
			return nil, fmt.Errorf("environment variable %s is not set", secret.Env)
		}
		values[secret.Env] = value
	}
	values["MAYFLY_PLUGIN_NAME"] = manifest.Name
	values["MAYFLY_PLUGIN_DIR"] = manifest.Dir
	out := make([]string, 0, len(values))
	for _, key := range sortedKeys(values) {
		out = append(out, key+"="+values[key])
	}
	return out, nil
}

// resolveShim finds the shim executable.
func resolveShim(explicit string) (string, error) {
	if explicit != "" {
		path, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("plugin shim %s: %w", explicit, err)
		}
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("plugin shim %s: %w", path, err)
		}
		return path, nil
	}
	var tried []string
	if executable, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(executable), shimBinary)
		if _, err := os.Stat(sibling); err == nil {
			return sibling, nil
		}
		tried = append(tried, sibling)
	}
	if path, err := exec.LookPath(shimBinary); err == nil {
		return path, nil
	}
	tried = append(tried, shimBinary+" on PATH")
	if _, err := os.Stat(k8s.PluginShim); err == nil {
		return k8s.PluginShim, nil
	}
	tried = append(tried, k8s.PluginShim)
	return "", fmt.Errorf("plugin shim not found (tried %s); install mayfly-plugin-shim next to mayflyd or on PATH", strings.Join(tried, ", "))
}

// logWriter sends one child's stderr lines to the control-plane log.
type logWriter struct {
	logger *slog.Logger
	mu     sync.Mutex
	buffer []byte
}

// Write buffers a partial line and logs every complete one.
func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffer = append(w.buffer, p...)
	for {
		index := bytes.IndexByte(w.buffer, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimRight(string(w.buffer[:index]), "\r")
		w.buffer = w.buffer[index+1:]
		if line != "" {
			w.logger.Info("plugin stderr", "line", line)
		}
	}
	return len(p), nil
}
