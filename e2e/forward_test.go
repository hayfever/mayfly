package e2e

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
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

func TestTunnelRetryAfter(t *testing.T) {
	wrapped := &url.Error{Op: "Post", URL: "http://127.0.0.1:40627/internal/v1/llm/chat/completions", Err: io.EOF}
	for _, tc := range []struct {
		name   string
		err    error
		before int64
		now    int64
		want   tunnelDecision
	}{
		{"no error", nil, 3, 3, tunnelFailOther},
		{"no error, count moved", nil, 3, 4, tunnelFailOther},
		{"eof, nothing admitted", io.EOF, 3, 3, tunnelRetry},
		{"eof, moved", io.EOF, 3, 4, tunnelAdmittedLost},
		{"eof, moved by an agent call", io.EOF, 0, 1, tunnelAdmittedLost},
		{"wrapped eof, nothing admitted", wrapped, 3, 3, tunnelRetry},
		{"wrapped eof, moved", wrapped, 3, 4, tunnelAdmittedLost},
		{"reset, nothing admitted", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, 3, 3, tunnelRetry},
		{"reset, moved", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, 3, 4, tunnelAdmittedLost},
		{"refused, nothing admitted", &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, 3, 3, tunnelRetry},
		{"closed, nothing admitted", net.ErrClosed, 3, 3, tunnelRetry},
		{"deadline", &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, 3, 3, tunnelFailOther},
		{"unexpected eof", io.ErrUnexpectedEOF, 3, 3, tunnelFailOther},
		{"unrelated", errors.New("boom"), 3, 4, tunnelFailOther},
	} {
		if got := tunnelRetryAfter(tc.err, tc.before, tc.now); got != tc.want {
			t.Errorf("%s: tunnelRetryAfter(%v, %d, %d) = %v, want %v", tc.name, tc.err, tc.before, tc.now, got, tc.want)
		}
	}
}

func TestForwardLogFile(t *testing.T) {
	t.Setenv("E2E_ARTIFACTS", "")
	if f := forwardLogFile(1); f != nil {
		_ = f.Close()
		t.Fatal("forwardLogFile created a file without E2E_ARTIFACTS")
	}

	dir := t.TempDir()
	t.Setenv("E2E_ARTIFACTS", dir)
	f := forwardLogFile(1)
	if f == nil {
		t.Fatal("forwardLogFile returned nil with E2E_ARTIFACTS set")
	}
	if _, err := io.WriteString(f, "Handling connection for 8080\n"); err != nil {
		t.Fatalf("write port-forward log: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close port-forward log: %v", err)
	}
	data, err := os.ReadFile(dir + "/port-forward-1.log")
	if err != nil {
		t.Fatalf("read port-forward log: %v", err)
	}
	if string(data) != "Handling connection for 8080\n" {
		t.Fatalf("port-forward log = %q", data)
	}
}

// failureRecorder records the first failure a check reports.
type failureRecorder struct{ msg string }

func (r *failureRecorder) Fatalf(format string, args ...any) {
	r.msg = fmt.Sprintf(format, args...)
}

// TestTunnelRetryPropertyCatchesUnsafeRetry is the negative control for the
// rule FuzzTunnelRetryDecision asserts: a decision that repeats a dropped call
// without reading the count is caught. It is the pre-fix behavior.
func TestTunnelRetryPropertyCatchesUnsafeRetry(t *testing.T) {
	unsafe := func(err error, _, _ int64) tunnelDecision {
		if forwardBroken(err) {
			return tunnelRetry
		}
		return tunnelFailOther
	}
	rec := &failureRecorder{}
	checkTunnelDecision(rec, unsafe, io.EOF, 3, 4)
	if !strings.Contains(rec.msg, "retried after the count moved") {
		t.Fatalf("the unsafe rule was not caught: %q", rec.msg)
	}
	rec = &failureRecorder{}
	checkTunnelDecision(rec, tunnelRetryAfter, io.EOF, 3, 4)
	if rec.msg != "" {
		t.Fatalf("the fixed rule failed the property: %s", rec.msg)
	}
}
