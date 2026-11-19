package e2e

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
)

func TestForwardPort(t *testing.T) {
	for _, tc := range []struct {
		line string
		port string
		ok   bool
	}{
		{"Forwarding from 127.0.0.1:40627 -> 8080", "40627", true},
		{"Forwarding from 127.0.0.1:40627 -> 8080\n", "40627", true},
		{"Forwarding from [::1]:40627 -> 8080", "", false},
		{"Handling connection for 8080", "", false},
		{"", "", false},
	} {
		port, ok := forwardPort(tc.line)
		if ok != tc.ok || port != tc.port {
			t.Errorf("forwardPort(%q) = %q, %v; want %q, %v", tc.line, port, ok, tc.port, tc.ok)
		}
	}
}

func TestForwardBroken(t *testing.T) {
	refused := &url.Error{
		Op:  "Get",
		URL: "http://127.0.0.1:40627/readyz",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"refused", refused, true},
		{"reset", fmt.Errorf("GET /readyz: %w", syscall.ECONNRESET), true},
		{"unexpected eof", io.ErrUnexpectedEOF, false},
		{"eof", io.EOF, true},
		{"closed", net.ErrClosed, true},
		{"timeout", &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, false},
		{"unrelated", errors.New("boom"), false},
	} {
		if got := forwardBroken(tc.err); got != tc.want {
			t.Errorf("%s: forwardBroken(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
