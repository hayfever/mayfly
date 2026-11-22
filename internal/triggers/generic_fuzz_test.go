package triggers

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/session"
)

// FuzzGenericParse checks the generic trigger: a signed or token-authenticated
// delivery parses, an unsigned or altered delivery is unauthorized, and the
// Idempotency-Key header becomes the delivery key.
func FuzzGenericParse(f *testing.F) {
	f.Add("gen-secret", []byte(`{"prompt":"triage this","title":"t","ttl":"15m","skills":["reporting"],"mcp_servers":["echo"]}`), uint8(9))
	f.Add("gen-secret", []byte("plain text prompt"), uint8(2))
	f.Add("gen-secret", []byte(`{"prompt":"token auth"}`), uint8(11))
	f.Add("", []byte(`{"prompt":"unsigned"}`), uint8(1))
	for _, title := range []string{
		strings.Repeat("t", session.MaxTitleBytes),           // exactly the cap
		strings.Repeat("t", session.MaxTitleBytes+1),         // one byte over
		strings.Repeat("t", session.MaxTitleBytes-4) + "日zz", // the cut lands inside a rune
	} {
		f.Add("gen-secret", []byte(fmt.Sprintf(`{"prompt":"triage","title":%q}`, title)), uint8(2))
	}
	f.Fuzz(func(t *testing.T, secret string, body []byte, mode uint8) {
		cfg := config.TriggerConfig{
			Name:          "generic",
			Type:          "generic",
			Secret:        secret,
			AllowUnsigned: mode&1 != 0,
			Agent:         "triage",
		}
		h := fuzzHeader("Content-Type", "application/json")
		switch {
		case mode&2 != 0:
			h.Set("X-Mayfly-Token", secret)
		case mode&4 != 0:
			h.Set("X-Hub-Signature-256", "sha256="+hmacHex(secret, body))
		default:
			h.Set("X-Mayfly-Signature-256", hmacHex(secret, body))
		}
		if mode&8 != 0 {
			h.Set("Idempotency-Key", "  idem-1 \n")
		}

		res, err := Parse(cfg, h, body)
		if secret == "" {
			if cfg.AllowUnsigned {
				if errors.Is(err, ErrUnauthorized) {
					t.Fatalf("allow_unsigned rejected an empty-secret request: %v", err)
				}
			} else if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("empty secret without allow_unsigned returned %v", err)
			}
			return
		}
		if errors.Is(err, ErrUnauthorized) {
			t.Fatalf("valid credential rejected (mode %d): %v", mode, err)
		}
		fuzzCheckError(t, err)
		if err != nil {
			return
		}
		if res == nil {
			t.Fatal("nil result with nil error")
		}
		if res.Options.Trigger.Kind != "generic" || res.Options.Trigger.Name != cfg.Name {
			t.Fatalf("trigger = %+v", res.Options.Trigger)
		}
		if mode&8 != 0 && res.DeliveryKey != "idem-1" {
			t.Fatalf("delivery key = %q, want the trimmed Idempotency-Key", res.DeliveryKey)
		}
		if res.Options.Trigger.Delivery != res.DeliveryKey {
			t.Fatalf("trigger delivery %q != delivery key %q", res.Options.Trigger.Delivery, res.DeliveryKey)
		}
		if len(res.Options.Title) > session.MaxTitleBytes {
			t.Fatalf("title is %d bytes, over the %d-byte cap", len(res.Options.Title), session.MaxTitleBytes)
		}
		if !utf8.ValidString(res.Options.Title) {
			t.Fatalf("title is not valid UTF-8: %q", res.Options.Title)
		}

		// No credential, a wrong token, and a mutated body must all be
		// rejected even when allow_unsigned is set for a non-empty secret.
		strict := config.TriggerConfig{Name: "generic", Type: "generic", Secret: secret, AllowUnsigned: true}
		if _, err := Parse(strict, http.Header{}, body); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("unsigned request returned %v, want ErrUnauthorized", err)
		}
		if _, err := Parse(strict, fuzzHeader("X-Mayfly-Token", secret+"x"), body); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("wrong token returned %v, want ErrUnauthorized", err)
		}
		sig := hmacHex(secret, body)
		signed := fuzzHeader("X-Mayfly-Signature-256", sig)
		if mutated := fuzzFlip(body); !bytes.Equal(mutated, body) {
			if _, err := Parse(strict, signed, mutated); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("mutated body returned %v, want ErrUnauthorized", err)
			}
		}
		if _, err := Parse(strict, fuzzHeader("X-Mayfly-Signature-256", sig[:len(sig)-1]), body); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("truncated signature returned %v, want ErrUnauthorized", err)
		}

		// A malformed ttl is a payload error, not a verification error.
		unsigned := config.TriggerConfig{Name: "generic", Type: "generic", AllowUnsigned: true}
		if _, err := Parse(unsigned, fuzzHeader("Content-Type", "application/json"), []byte(`{"prompt":"x","ttl":"soon"}`)); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("invalid ttl returned %v, want ErrBadRequest", err)
		}
	})
}
