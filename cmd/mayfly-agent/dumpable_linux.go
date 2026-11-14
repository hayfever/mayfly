//go:build linux

package main

import (
	"fmt"
	"syscall"
)

// prSetDumpable is PR_SET_DUMPABLE from <linux/prctl.h>. The syscall package
// does not define it. The argument is SUID_DUMP_DISABLE.
const prSetDumpable = 4

// disableDumpable clears the process dumpable flag. A process that is not
// dumpable has its /proc entries owned by root, so a same-UID process cannot
// read /proc/<pid>/environ, /proc/<pid>/mem, or ptrace it.
//
// The runtime reads MAYFLY_SESSION_TOKEN from its environment, and an inline
// plugin runs as the same UID in the same container. Without this flag the
// plugin could read the token from /proc/<ppid>/environ and use it to dispatch
// gateway calls of the session.
func disableDumpable() error {
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetDumpable, 0, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_DUMPABLE, 0): %w", errno)
	}
	return nil
}
