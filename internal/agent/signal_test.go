package agent

import (
	"errors"
	"testing"
)

// TestSignalStop covers the two SIGTERM outcomes: a resumable session leaves
// its turn to a replacement, and one that cannot resume finalizes instead.
func TestSignalStop(t *testing.T) {
	cause := errors.New("context canceled")

	reason, err := signalStop(false, cause)
	if reason != "terminated" || err != nil {
		t.Fatalf("non-resumable stop = reason %q, error %v", reason, err)
	}

	reason, err = signalStop(true, cause)
	if reason != "" {
		t.Fatalf("resumable stop reason = %q, want empty", reason)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("resumable stop error = %v, want %v", err, cause)
	}
}
