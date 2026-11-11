package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResolveFallsBackToBuiltin(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "session.json")
	agent := filepath.Join(dir, "mayfly-agent")
	writeExecutable(t, agent)
	if err := os.WriteFile(cfgPath, []byte(`{"harness":"mayfly"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(cfgPath, dir, agent)
	if err != nil {
		t.Fatal(err)
	}
	if got != agent {
		t.Fatalf("resolved %q", got)
	}
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = Resolve(cfgPath, dir, agent)
	if err != nil || got != agent {
		t.Fatalf("empty harness: %q %v", got, err)
	}
}

func TestResolveNamedHarness(t *testing.T) {
	dir := t.TempDir()
	harnessDir := filepath.Join(dir, "harnesses")
	if err := os.MkdirAll(harnessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "session.json")
	agent := filepath.Join(dir, "mayfly-agent")
	writeExecutable(t, agent)
	pi := filepath.Join(harnessDir, "pi")
	writeExecutable(t, pi)
	if err := os.WriteFile(cfgPath, []byte(`{"harness":"pi"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(cfgPath, harnessDir, agent)
	if err != nil {
		t.Fatal(err)
	}
	if got != pi {
		t.Fatalf("resolved %q", got)
	}
}

func TestResolveMissingHarnessFails(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "session.json")
	agent := filepath.Join(dir, "mayfly-agent")
	writeExecutable(t, agent)
	if err := os.WriteFile(cfgPath, []byte(`{"harness":"pi"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(cfgPath, filepath.Join(dir, "harnesses"), agent); err == nil {
		t.Fatal("expected missing harness error")
	}
	if err := os.WriteFile(cfgPath, []byte(`{"harness":"../escape"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(cfgPath, filepath.Join(dir, "harnesses"), agent); err == nil {
		t.Fatal("expected invalid harness error")
	}
}
