package plugin

import (
	"bytes"
	"testing"
)

// TestSidecarKeyIsPerPlugin keeps one plugin's derived key from verifying
// another plugin's tokens.
func TestSidecarKeyIsPerPlugin(t *testing.T) {
	session := bytes.Repeat([]byte{0x42}, 32)
	alpha := SidecarKey(session, "alpha")
	beta := SidecarKey(session, "beta")
	if len(alpha) != 32 {
		t.Fatalf("key length = %d, want 32", len(alpha))
	}
	if bytes.Equal(alpha, beta) {
		t.Fatal("two plugins derived the same key")
	}
	if !bytes.Equal(alpha, SidecarKey(session, "alpha")) {
		t.Fatal("derivation is not stable")
	}
	otherSession := SidecarKey(bytes.Repeat([]byte{0x43}, 32), "alpha")
	if bytes.Equal(alpha, otherSession) {
		t.Fatal("two sessions derived the same key")
	}
}
