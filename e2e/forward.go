// Package e2e holds the end-to-end suite. This file has no build tag so its
// pure helpers run under `go test ./...` without a cluster; the suite itself
// is behind the e2e tag.
package e2e

import (
	"errors"
	"io"
	"net"
	"regexp"
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
