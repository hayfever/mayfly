//go:build linux

package main

import (
	"os"
	"testing"
)

// TestDisableDumpable proves the flag a running runtime sets: after it, a
// same-UID process cannot read this process's environment, which holds
// MAYFLY_SESSION_TOKEN. The test process itself stands in for that reader.
func TestDisableDumpable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root passes the ptrace check regardless of the dumpable flag")
	}
	if _, err := os.ReadFile("/proc/self/environ"); err != nil {
		t.Skipf("the test environment already blocks /proc/self/environ: %v", err)
	}
	if err := disableDumpable(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile("/proc/self/environ"); err == nil {
		t.Fatal("the process environment is still readable after PR_SET_DUMPABLE 0")
	}
}
