package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadManifestWithPromptFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "prompt.md", "You are {{.Agent}} for {{.SessionID}}.")
	write(t, dir, "agent.yaml", `schemaVersion: 1
name: acme/agents
version: 1.2.3
agents:
  - name: triage
    description: Triage
    systemPromptFile: prompt.md
    skills: [incident-triage]
    maxSteps: 40
    default: true
`)
	loaded, err := LoadDirs([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded = %d", len(loaded))
	}
	agent := loaded[0]
	if agent.Name != "triage" || !agent.Default || agent.MaxSteps != 40 {
		t.Fatalf("agent = %+v", agent)
	}
	prompt, err := agent.RenderPrompt(map[string]string{"Agent": "triage", "SessionID": "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "You are triage for s1." {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestLoadManifestRejectsDuplicatesAndTraversal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", `schemaVersion: 1
name: a
agents:
  - name: one
`)
	if _, err := LoadDirs([]string{dir}); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "b.yaml", `schemaVersion: 1
name: b
agents:
  - name: one
`)
	if _, err := LoadDirs([]string{dir}); err == nil {
		t.Fatal("expected duplicate error")
	}

	other := t.TempDir()
	write(t, other, "c.yaml", `schemaVersion: 1
name: c
agents:
  - name: two
    systemPromptFile: ../outside.md
`)
	if _, err := LoadDirs([]string{other}); err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestLoadManifestSkipsHiddenDirectoriesAndValidatesProtocol(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".20260101/agent.yaml", `schemaVersion: 1
name: hidden
agents:
  - name: hidden
`)
	write(t, dir, "agent.yaml", `schemaVersion: 1
name: visible
agents:
  - name: visible
    runtime:
      protocol: 1
`)
	loaded, err := LoadDirs([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Name != "visible" {
		t.Fatalf("loaded = %+v", loaded)
	}

	bad := t.TempDir()
	write(t, bad, "agent.yaml", `schemaVersion: 1
name: bad
agents:
  - name: bad
    runtime:
      protocol: 2
`)
	if _, err := LoadDirs([]string{bad}); err == nil {
		t.Fatal("expected protocol error")
	}
}

func TestLoadManifestRejectsReservedFields(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "agent.yaml", `schemaVersion: 1
name: bad
unknownField: true
agents:
  - name: BadName
`)
	if _, err := LoadDirs([]string{dir}); err == nil {
		t.Fatal("expected strict manifest error")
	}
	write(t, dir, "agent.yaml", `schemaVersion: 1
name: bad
agents:
  - name: ok
    systemPrompt: one
    systemPromptFile: two.md
`)
	if _, err := LoadDirs([]string{dir}); err == nil {
		t.Fatal("expected prompt conflict error")
	}
}

func TestLoadManifestFollowsConfigMapSymlink(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".data/agent.yaml", `schemaVersion: 1
name: mounted
agents:
  - name: mounted
`)
	if err := os.Symlink(".data/agent.yaml", filepath.Join(dir, "agent.yaml")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	loaded, err := LoadDirs([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Name != "mounted" {
		t.Fatalf("loaded = %+v", loaded)
	}
}

func TestLoadRuntimeBlock(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "pi.yaml", `schemaVersion: 1
name: acme/pi
agents:
  - name: pi
    harness: pi
    runtime:
      image: ghcr.io/acme/mayfly-pi:1
      protocol: 1
      command: ["/usr/local/bin/pi"]
      args: ["--mode", "rpc"]
      env:
        PI_LOG_LEVEL: info
      config:
        thinking: high
        maxTurns: 80
`)
	loaded, err := LoadDirs([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if loaded[0].Harness != "pi" {
		t.Fatalf("harness = %q", loaded[0].Harness)
	}
	runtime := loaded[0].Runtime
	if runtime.Image != "ghcr.io/acme/mayfly-pi:1" || runtime.Protocol != 1 {
		t.Fatalf("runtime = %+v", runtime)
	}
	if len(runtime.Command) != 1 || runtime.Command[0] != "/usr/local/bin/pi" || len(runtime.Args) != 2 {
		t.Fatalf("command = %v args=%v", runtime.Command, runtime.Args)
	}
	if runtime.Env["PI_LOG_LEVEL"] != "info" || runtime.Config["thinking"] != "high" {
		t.Fatalf("env/config = %+v %+v", runtime.Env, runtime.Config)
	}

	bad := t.TempDir()
	write(t, bad, "pi.yaml", `schemaVersion: 1
name: acme/pi
agents:
  - name: pi
    runtime:
      env:
        MAYFLY_SESSION_TOKEN: stolen
`)
	if _, err := LoadDirs([]string{bad}); err == nil {
		t.Fatal("expected reserved env error")
	}
}
