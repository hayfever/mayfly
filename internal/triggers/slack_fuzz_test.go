package triggers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
)

// FuzzSlackParse checks the Slack trigger: a v0 signature is accepted inside
// the five-minute window and rejected outside it, missing or malformed headers
// and altered bodies are unauthorized, and both url_verification JSON and
// form-encoded slash commands parse without panic.
func FuzzSlackParse(f *testing.F) {
	f.Add("slack-secret", []byte("command=%2Fmayfly&text=deploy+failed&user_name=alice&channel_name=ops&trigger_id=tr-1&response_url=https%3A%2F%2Fhooks.slack.com%2Fcommands%2F1"), int64(0), uint8(1))
	f.Add("slack-secret", []byte(`{"type":"url_verification","challenge":"abc123"}`), int64(0), uint8(2))
	f.Add("slack-secret", []byte("text=status&user_name=bob&channel_name=dev"), int64(-600), uint8(0))
	f.Add("", []byte("text=unsigned"), int64(0), uint8(5))
	f.Fuzz(func(t *testing.T, secret string, body []byte, delta int64, mode uint8) {
		// Keep the offset in a range that still exercises both sides of the
		// window without overflowing the timestamp.
		if delta > 3600 {
			delta = 3600
		}
		if delta < -3600 {
			delta = -3600
		}
		// The timestamp is computed here, not stored in the corpus, so the
		// window check stays meaningful at run time.
		ts := strconv.FormatInt(time.Now().Unix()+delta, 10)
		sig := fuzzSlackSignature(secret, ts, body)
		h := fuzzHeader(
			"X-Slack-Request-Timestamp", ts,
			"X-Slack-Signature", sig,
		)
		if mode&1 != 0 {
			h.Set("Content-Type", "application/json")
		} else {
			h.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if mode&2 != 0 {
			h.Set("Idempotency-Key", "idem-slack")
		}
		cfg := config.TriggerConfig{
			Name:          "slack",
			Type:          "slack",
			Secret:        secret,
			AllowUnsigned: mode&4 != 0,
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
		switch d := fuzzAbs(delta); {
		case d <= 240:
			if errors.Is(err, ErrUnauthorized) {
				t.Fatalf("in-window signature rejected (delta %d): %v", delta, err)
			}
			fuzzCheckError(t, err)
		case d >= 360:
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("out-of-window timestamp accepted (delta %d): %v", delta, err)
			}
		default:
			fuzzCheckError(t, err)
		}
		if err != nil {
			return
		}
		if res == nil {
			t.Fatal("nil result with nil error")
		}
		if len(res.Options.Title) > len("Slack: ")+60+len("…") {
			t.Fatalf("title is %d bytes, over the 60-byte subject bound", len(res.Options.Title))
		}

		jsonBranch := strings.Contains(h.Get("Content-Type"), "application/json") ||
			(len(body) > 0 && body[0] == '{')
		if jsonBranch {
			if res.Challenge == nil {
				return
			}
			var payload map[string]any
			if json.Unmarshal(body, &payload) == nil {
				challenge, _ := payload["challenge"].(string)
				if res.Challenge["challenge"] != challenge {
					t.Fatalf("challenge = %v, want %q", res.Challenge["challenge"], challenge)
				}
			}
			return
		}
		if values, err := url.ParseQuery(string(body)); err == nil {
			if id := strings.TrimSpace(values.Get("trigger_id")); id != "" && res.DeliveryKey != id {
				t.Fatalf("delivery key = %q, want trigger_id %q", res.DeliveryKey, id)
			}
		}

		// Missing, malformed, stale, and altered credentials all fail.
		cases := map[string]http.Header{
			"missing timestamp": fuzzHeader("X-Slack-Signature", sig),
			"missing signature": fuzzHeader("X-Slack-Request-Timestamp", ts),
			"non-numeric ts": fuzzHeader(
				"X-Slack-Request-Timestamp", "not-a-number",
				"X-Slack-Signature", sig),
			"signature for another ts": fuzzHeader(
				"X-Slack-Request-Timestamp", strconv.FormatInt(time.Now().Unix()+delta+1, 10),
				"X-Slack-Signature", sig),
			"flipped signature": fuzzHeader(
				"X-Slack-Request-Timestamp", ts,
				"X-Slack-Signature", "v0="+fuzzFlipHex(strings.TrimPrefix(sig, "v0="))),
			"truncated signature": fuzzHeader(
				"X-Slack-Request-Timestamp", ts,
				"X-Slack-Signature", sig[:len(sig)-1]),
		}
		for name, bad := range cases {
			if _, err := Parse(cfg, bad, body); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("%s returned %v, want ErrUnauthorized", name, err)
			}
		}
		if mutated := fuzzFlip(body); !bytes.Equal(mutated, body) {
			if _, err := Parse(cfg, h, mutated); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("mutated body returned %v, want ErrUnauthorized", err)
			}
		}
		wrongSecret := config.TriggerConfig{Name: "slack", Type: "slack", Secret: secret + "x"}
		if _, err := Parse(wrongSecret, h, body); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("signature for another secret returned %v, want ErrUnauthorized", err)
		}
	})
}
