// Package e2e holds the end-to-end suite. This file has no build tag so its
// pure helpers run under `go test ./...` without a cluster; the suite itself
// is behind the e2e tag.
package e2e

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"syscall"
)

var forwardingRE = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// forwardPort returns the local port in a kubectl port-forward line.
func forwardPort(line string) (string, bool) {
	m := forwardingRE.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// forwardBroken reports whether err means the tunnel failed, not the control
// plane: the local port has no listener left, or the stream was torn down.
// kubectl keeps its local port open while the remote process is not listening
// yet, so this test alone does not justify replacing a live forward.
func forwardBroken(err error) bool {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, io.EOF),
		errors.Is(err, net.ErrClosed):
		return true
	}
	return false
}

// tunnelDecision is what a caller does after a call failed with err.
type tunnelDecision int

const (
	// tunnelRetry means the control plane never saw the call, so the caller
	// can send it again.
	tunnelRetry tunnelDecision = iota
	// tunnelAdmittedLost means the control plane admitted the call, so the
	// missing response is a lost result and the caller must not repeat it.
	tunnelAdmittedLost
	// tunnelFailOther means err is not a tunnel failure, so the caller reports
	// it unchanged.
	tunnelFailOther
)

// String names the decision in a test failure.
func (d tunnelDecision) String() string {
	switch d {
	case tunnelRetry:
		return "retry"
	case tunnelAdmittedLost:
		return "admitted-lost"
	default:
		return "fail-other"
	}
}

// tunnelRetryAfter decides whether a call that failed with err can be sent
// again. requestsBefore is the session's admitted request count read before
// the call, and requestsNow the count read after the error. A dropped tunnel
// repeats the call only while the count did not move: a moved count means the
// control plane admitted the call, and Go does not replay a POST with a body,
// so the missing response is a lost result. Any other error, and a nil error,
// is reported unchanged.
func tunnelRetryAfter(err error, requestsBefore, requestsNow int64) tunnelDecision {
	if err == nil || !forwardBroken(err) {
		return tunnelFailOther
	}
	if requestsNow > requestsBefore {
		return tunnelAdmittedLost
	}
	return tunnelRetry
}

// forwardLogSeq numbers the port-forward logs of one test process.
var forwardLogSeq atomic.Int64

// forwardLogFile creates the file that copies one kubectl port-forward's
// stderr when the harness sets E2E_ARTIFACTS, so a dropped call is provable
// after the run. It returns nil when capture is off or the file cannot be
// created; the caller then keeps the stderr in memory only. kubectl
// port-forward writes connection handling and transport errors, never a
// secret.
func forwardLogFile(index int64) *os.File {
	dir := os.Getenv("E2E_ARTIFACTS")
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: port-forward log disabled: %v\n", err)
		return nil
	}
	f, err := os.Create(filepath.Join(dir, fmt.Sprintf("port-forward-%d.log", index)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: port-forward log disabled: %v\n", err)
		return nil
	}
	return f
}
