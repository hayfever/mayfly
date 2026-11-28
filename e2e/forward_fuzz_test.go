package e2e

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// tunnelError maps a fuzz selector to one error the client can return.
func tunnelError(kind uint8) error {
	switch kind % 8 {
	case 1:
		return io.EOF
	case 2:
		return io.ErrUnexpectedEOF
	case 3:
		return &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	case 4:
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	case 5:
		return net.ErrClosed
	case 6:
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	case 7:
		return errors.New("boom")
	}
	return nil
}

// tunnelDecisionChecker reports a violated retry rule. *testing.T and the
// negative control's recorder both satisfy it.
type tunnelDecisionChecker interface {
	Fatalf(format string, args ...any)
}

// checkTunnelDecision asserts the retry rule for one decision: a call is
// repeated only after a tunnel failure that admitted nothing, and a moved
// count is reported as a lost response.
func checkTunnelDecision(tb tunnelDecisionChecker, decide func(error, int64, int64) tunnelDecision, err error, before, after int64) {
	switch got := decide(err, before, after); got {
	case tunnelRetry:
		if err == nil || !forwardBroken(err) {
			tb.Fatalf("retried a non-tunnel error %v", err)
		}
		if after > before {
			tb.Fatalf("retried after the count moved: requests %d -> %d, err %v", before, after, err)
		}
	case tunnelAdmittedLost:
		if err == nil || !forwardBroken(err) || after <= before {
			tb.Fatalf("reported a lost response for err %v with requests %d -> %d", err, before, after)
		}
	}
}

// FuzzTunnelRetryDecision checks the retry rule over every error class and
// every relation of the two request counts: a call is repeated only after a
// tunnel failure that admitted nothing, and a moved count always reports a
// lost response instead of a retry.
func FuzzTunnelRetryDecision(f *testing.F) {
	f.Add(uint8(0), int64(3), int64(4))
	f.Add(uint8(1), int64(0), int64(0))
	f.Add(uint8(1), int64(1), int64(2))
	f.Add(uint8(3), int64(2), int64(2))
	f.Add(uint8(6), int64(2), int64(2))
	f.Add(uint8(7), int64(5), int64(6))
	f.Fuzz(func(t *testing.T, kind uint8, before, after int64) {
		checkTunnelDecision(t, tunnelRetryAfter, tunnelError(kind), before, after)
	})
}
