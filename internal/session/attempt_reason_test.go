package session

import (
	"strings"
	"testing"
)

// TestAdoptAttemptReason checks the count and the plural in the reason a
// settlement gives a session adoption could not rebuild.
func TestAdoptAttemptReason(t *testing.T) {
	cases := []struct {
		attempts int
		want     string
	}{
		{1, "session could not be adopted after 1 attempt: read the session config"},
		{2, "session could not be adopted after 2 attempts: read the session config"},
		{10, "session could not be adopted after 10 attempts: read the session config"},
	}
	for _, tc := range cases {
		got := adoptAttemptReason(adoptFailure{Attempts: tc.attempts, Reason: "read the session config"})
		if !strings.HasPrefix(got, tc.want) {
			t.Fatalf("%d attempts = %q, want %q", tc.attempts, got, tc.want)
		}
	}
}
