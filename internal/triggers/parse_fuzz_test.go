package triggers

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hayfever/mayfly/internal/config"
)

var fuzzTriggerTypes = []string{"github", "slack", "datadog", "generic"}

// FuzzParseUntrusted checks the Parse dispatch over arbitrary trigger types,
// headers, and bodies: it never panics, it returns only ErrUnauthorized or
// ErrBadRequest, and a request accepted with a configured secret must carry a
// credential that verifies on recomputation. The GitHub token header never
// counts as that credential.
func FuzzParseUntrusted(f *testing.F) {
	f.Add(uint8(0), "gh-secret", "push", "del-1", []byte(`{"ref":"refs/heads/main","repository":{"full_name":"acme/app","html_url":"https://github.com/acme/app"},"sender":{"login":"alice"},"commits":[{"message":"fix"}]}`), uint8(3))
	f.Add(uint8(1), "slack-secret", "", "", []byte("command=%2Fmayfly&text=hi&user_name=alice&channel_name=ops&trigger_id=tr-1"), uint8(3))
	f.Add(uint8(2), "dd-secret", "", "", []byte(`{"title":"High error rate","text":"5xx above threshold","alert_type":"error"}`), uint8(3))
	f.Add(uint8(3), "gen-secret", "", "", []byte(`{"prompt":"do the thing"}`), uint8(3))
	f.Add(uint8(3), "", "", "", []byte(`{"prompt":"unsigned"}`), uint8(1))
	f.Add(uint8(0), "gh-secret", "pull_request", "", []byte(`{"action":"opened","number":1}`), uint8(17))
	f.Fuzz(func(t *testing.T, typ uint8, secret, event, delivery string, body []byte, mode uint8) {
		kind := fuzzTriggerTypes[int(typ)%len(fuzzTriggerTypes)]
		cfg := config.TriggerConfig{
			Name:          "fuzz-" + kind,
			Type:          kind,
			Secret:        secret,
			AllowUnsigned: mode&1 != 0,
		}
		h := http.Header{}
		switch auth := (mode >> 1) & 3; auth {
		case 1:
			h.Set("X-Mayfly-Signature-256", hmacHex(secret, body))
		case 2:
			h.Set("X-Hub-Signature-256", "sha256="+hmacHex(secret, body))
		case 3:
			h.Set("X-Mayfly-Token", secret)
		}
		if kind == "slack" && (mode>>1)&3 != 0 {
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			h.Set("X-Slack-Request-Timestamp", ts)
			h.Set("X-Slack-Signature", fuzzSlackSignature(secret, ts, body))
		}
		if kind == "github" {
			h.Set("X-GitHub-Event", event)
			h.Set("X-GitHub-Delivery", delivery)
		}
		if mode&8 != 0 {
			h.Set("Idempotency-Key", "  idem \n")
		}

		res, err := Parse(cfg, h, body)
		if err != nil {
			if res != nil {
				t.Fatalf("Parse returned a result with error %v", err)
			}
			fuzzCheckError(t, err)
			if secret == "" && !cfg.AllowUnsigned && !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("empty secret without allow_unsigned returned %v, want ErrUnauthorized", err)
			}
			return
		}
		if res == nil {
			t.Fatal("nil result with nil error")
		}
		if secret == "" && !cfg.AllowUnsigned {
			t.Fatal("request accepted with an empty secret and allow_unsigned disabled")
		}
		if secret != "" && !cfg.AllowUnsigned {
			// Accepted without allow_unsigned: the credential must verify.
			switch kind {
			case "github":
				if !verifySignature(secret, h, body) {
					t.Fatal("github request accepted without a valid signature")
				}
			case "generic", "datadog":
				if !verifySignature(secret, h, body) && !verifyToken(secret, h) {
					t.Fatal("request accepted without a valid signature or token")
				}
			case "slack":
				ts := h.Get("X-Slack-Request-Timestamp")
				if fuzzSlackSignature(secret, ts, body) != h.Get("X-Slack-Signature") {
					t.Fatal("slack request accepted without a valid v0 signature")
				}
			}
		}
		if res.Challenge == nil && res.Options.Trigger.Kind != kind {
			t.Fatalf("trigger kind = %q, want %q", res.Options.Trigger.Kind, kind)
		}

		// Delivery key precedence: the sender's delivery, then the header. A
		// delivery key must be valid UTF-8, so an invalid GitHub delivery never
		// reaches this point as an accepted request.
		switch kind {
		case "github":
			if delivery != "" {
				if !utf8.ValidString(delivery) {
					t.Fatalf("accepted a delivery key that is not valid UTF-8: %q", delivery)
				}
				if res.DeliveryKey != delivery {
					t.Fatalf("delivery key = %q, want %q", res.DeliveryKey, delivery)
				}
			} else if mode&8 != 0 && res.DeliveryKey != "idem" {
				t.Fatalf("delivery key = %q, want the trimmed Idempotency-Key", res.DeliveryKey)
			}
		case "generic", "datadog":
			if mode&8 != 0 && res.DeliveryKey != "idem" {
				t.Fatalf("delivery key = %q, want the trimmed Idempotency-Key", res.DeliveryKey)
			}
		}
		if res.DeliveryKey != "" && res.Options.Trigger.Delivery != res.DeliveryKey {
			t.Fatalf("trigger delivery %q != delivery key %q", res.Options.Trigger.Delivery, res.DeliveryKey)
		}
	})
}
