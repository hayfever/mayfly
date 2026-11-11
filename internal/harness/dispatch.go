// Package harness resolves which agent loop to run inside the session image.
// The image entrypoint is mayfly-harness, which falls back to the built-in
// Mayfly runtime when no harness is selected.
package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// DefaultDir contains in-image harness executables.
	DefaultDir = "/opt/mayfly/harnesses"
	// Builtin is the built-in Mayfly harness name.
	Builtin = "mayfly"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type config struct {
	Harness string `json:"harness"`
}

// Resolve returns the executable for the configured harness. An empty or
// "mayfly" harness selects agentBinary. A selected harness that is missing or
// not executable is an error; it never silently falls back.
func Resolve(configPath, harnessDir, agentBinary string) (string, error) {
	if configPath == "" {
		return "", fmt.Errorf("session config path is empty")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("read session config: %w", err)
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("parse session config: %w", err)
	}
	name := strings.TrimSpace(cfg.Harness)
	if name == "" || name == Builtin {
		return executable(agentBinary, "Mayfly runtime")
	}
	if !namePattern.MatchString(name) {
		return "", fmt.Errorf("invalid harness name %q", name)
	}
	return executable(filepath.Join(harnessDir, name), "harness "+name)
}

func executable(path, label string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s path is empty", label)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", label, abs, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s %s is a directory", label, abs)
	}
	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s %s is not executable", label, abs)
	}
	return abs, nil
}
