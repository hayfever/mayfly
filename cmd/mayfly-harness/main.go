// Command mayfly-harness is the agent image entrypoint. It selects the agent
// loop from session.json: a named executable in /opt/mayfly/harnesses, or the
// built-in Mayfly runtime when no harness is selected.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hayden/mayfly/internal/harness"
)

func main() {
	configPath := os.Getenv("MAYFLY_CONFIG")
	harnessDir := envOr("MAYFLY_HARNESS_DIR", harness.DefaultDir)

	executable, err := os.Executable()
	if err != nil {
		fail("resolve executable: " + err.Error())
	}
	agentBinary := envOr("MAYFLY_AGENT_BINARY", filepath.Join(filepath.Dir(executable), "mayfly-agent"))

	target, err := harness.Resolve(configPath, harnessDir, agentBinary)
	if err != nil {
		fail(err.Error())
	}
	args := append([]string{target}, os.Args[1:]...)
	if err := syscall.Exec(target, args, os.Environ()); err != nil {
		fail(fmt.Sprintf("exec %s: %v", target, err))
	}
}

func fail(message string) {
	line := "mayfly-harness: " + message
	fmt.Fprintln(os.Stderr, line)
	// Kubernetes copies this file into the container's terminated message, so
	// the control plane can report why the harness did not start.
	_ = os.WriteFile("/dev/termination-log", []byte(line), 0o644)
	os.Exit(127)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
