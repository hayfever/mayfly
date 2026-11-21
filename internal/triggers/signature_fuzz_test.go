package triggers

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/config"
)

// FuzzVerifyHMAC checks that a signature computed for one secret and body is
// accepted in both signature headers, with and without the sha256= prefix,
// and that every change to the body, secret, or signature is rejected.
func FuzzVerifyHMAC(f *testing.F) {
	f.Add("s3cret", []byte(`{"prompt":"triage the failing deploy"}`))
	f.Add("", []byte("unsigned body"))
	f.Add("key", []byte{})
	f.Fuzz(func(t *testing.T, secret string, body []byte) {
		cfg := config.TriggerConfig{Name: "fuzz", Type: "generic", Secret: secret}

		// With no secret the verifier never reads a header: allow_unsigned
		// alone decides, even when the request carries a valid signature.
		if secret == "" {
			for _, allow := range []bool{false, true} {
				cfg.AllowUnsigned = allow
				for _, h := range []http.Header{
					{},
					fuzzHeader("X-Hub-Signature-256", "sha256="+hmacHex("", body)),
					fuzzHeader("X-Mayfly-Signature-256", hmacHex("", body)),
				} {
					if got := verifyHMACOrToken(cfg, h, body); got != allow {
						t.Fatalf("empty secret: verifyHMACOrToken = %v, want %v", got, allow)
					}
				}
			}
			return
		}
		cfg.AllowUnsigned = false

		sig := hmacHex(secret, body)
		for _, name := range []string{"X-Mayfly-Signature-256", "X-Hub-Signature-256"} {
			if !verifyHMACOrToken(cfg, fuzzHeader(name, sig), body) {
				t.Fatalf("%s: valid signature rejected", name)
			}
			if !verifyHMACOrToken(cfg, fuzzHeader(name, "sha256="+sig), body) {
				t.Fatalf("%s: valid prefixed signature rejected", name)
			}

			for _, bad := range []string{
				sig[:len(sig)-1],       // truncated
				sig + "0",              // one extra nibble
				fuzzFlipHex(sig),       // valid hex, different bytes
				"sha256=" + sig + "\n", // trailing byte inside the value
				"",                     // empty value
				"sha256=",              // prefix only
			} {
				if verifyHMACOrToken(cfg, fuzzHeader(name, bad), body) {
					t.Fatalf("%s: rejected signature %q was accepted", name, bad)
				}
			}
		}

		// A signature for another secret, or for another body, never passes.
		valid := fuzzHeader("X-Hub-Signature-256", "sha256="+sig)
		if verifyHMACOrToken(cfg, fuzzHeader("X-Hub-Signature-256", "sha256="+hmacHex(secret+"x", body)), body) {
			t.Fatal("signature for another secret accepted")
		}
		if mutated := fuzzFlip(body); !bytes.Equal(mutated, body) {
			if verifyHMACOrToken(cfg, valid, mutated) {
				t.Fatal("signature accepted for a mutated body")
			}
			if verifyHMACOrToken(cfg, valid, append(append([]byte(nil), body...), 0)) {
				t.Fatal("signature accepted for an extended body")
			}
		}

		// allow_unsigned does not relax verification for a configured secret.
		cfg.AllowUnsigned = true
		if verifyHMACOrToken(cfg, fuzzHeader("X-Hub-Signature-256", "sha256="+hmacHex("wrong", body)), body) {
			t.Fatal("allow_unsigned accepted a wrong signature for a non-empty secret")
		}
		cfg.AllowUnsigned = false

		// The token path is an exact, constant-time compare.
		if !verifyHMACOrToken(cfg, fuzzHeader("X-Mayfly-Token", secret), body) {
			t.Fatal("exact token rejected")
		}
		for _, bad := range []string{secret + "\n", strings.ToUpper(secret)} {
			if bad == secret {
				continue
			}
			if verifyHMACOrToken(cfg, fuzzHeader("X-Mayfly-Token", bad), body) {
				t.Fatalf("token %q accepted", bad)
			}
		}
	})
}
