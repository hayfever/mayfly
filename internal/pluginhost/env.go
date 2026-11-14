package pluginhost

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// pluginTmpRoot holds each inline plugin's private HOME and TMPDIR.
const pluginTmpRoot = "/tmp/plugins"

// defaultPath is used when the parent process has no PATH.
const defaultPath = sessioncfg.DefaultPath

// baseEnv returns the complete environment for one inline plugin:
//
//	PATH, the plugin's own bin directory first
//	HOME and TMPDIR, private to the plugin
//	the inherited toolchain variables
//	the MAYFLY_* identity variables
//	the manifest's env
//	the secrets stored in SecretsDir
//
// Nothing else is inherited. In particular a plugin never sees
// MAYFLY_SESSION_TOKEN or MAYFLY_CONFIG, even when the parent sets them.
func baseEnv(p sessioncfg.Plugin, sessionID, workspaceDir string) ([]string, error) {
	values := map[string]string{
		"PATH": pluginPath(p.Dir),
	}
	root := filepath.Join(pluginTmpRoot, p.Name)
	for name, dir := range map[string]string{
		"HOME":   filepath.Join(root, "home"),
		"TMPDIR": filepath.Join(root, "tmp"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("plugin %q: %s: %w", p.Name, dir, err)
		}
		values[name] = dir
	}
	for name, value := range sessioncfg.PassthroughEnv() {
		values[name] = value
	}
	values["MAYFLY_PLUGIN_NAME"] = p.Name
	values["MAYFLY_PLUGIN_DIR"] = p.Dir
	values["MAYFLY_SESSION_ID"] = sessionID
	values["MAYFLY_WORKSPACE_DIR"] = workspaceDir
	for key, value := range p.Env {
		values[key] = value
	}
	for _, secret := range p.Secrets {
		path := filepath.Join(p.SecretsDir, secret.Env)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("plugin %q: read secret %s: %w", p.Name, secret.Env, err)
		}
		values[secret.Env] = strings.TrimSuffix(string(raw), "\n")
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+values[name])
	}
	return env, nil
}

// pluginPath is the PATH a plugin is started and looked up with: the plugin's
// bin directory, then the container's PATH.
func pluginPath(dir string) string {
	path := os.Getenv("PATH")
	if path == "" {
		path = defaultPath
	}
	return filepath.Join(dir, "bin") + string(os.PathListSeparator) + path
}
