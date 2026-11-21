package triggers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/config"
)

// FuzzDatadogParse checks the Datadog trigger: signature and token
// authentication both work, altered input is unauthorized, arbitrary JSON
// never panics, and the prompt stays proportional to the payload.
func FuzzDatadogParse(f *testing.F) {
	f.Add("dd-secret", []byte(`{"title":"High error rate","text":"5xx above threshold","alert_type":"error","url":"https://app.example/monitor/1","tags":["env:prod","service:api"]}`), uint8(0))
	f.Add("dd-secret", []byte(`{"title":"Disk almost full","text":"91% used","alert_type":"warning"}`), uint8(1))
	f.Add("dd-secret", []byte(`{"alert_type":"info","text":"recovered"}`), uint8(3))
	f.Add("dd-secret", []byte(`{}`), uint8(8))
	f.Fuzz(func(t *testing.T, secret string, body []byte, mode uint8) {
		cfg := config.TriggerConfig{
			Name:          "datadog",
			Type:          "datadog",
			Secret:        secret,
			AllowUnsigned: mode&8 != 0,
		}
		h := fuzzHeader("Content-Type", "application/json")
		switch mode & 3 {
		case 0:
			h.Set("X-Mayfly-Signature-256", hmacHex(secret, body))
		case 1:
			h.Set("X-Hub-Signature-256", "sha256="+hmacHex(secret, body))
		case 2:
			h.Set("X-Mayfly-Signature-256", "sha256="+hmacHex(secret, body))
		case 3:
			h.Set("X-Mayfly-Token", secret)
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

		// Parse only returns nil error after a successful JSON decode.
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("Parse accepted a body that does not decode: %v", err)
		}
		title := str(payload, "title")
		text := str(payload, "text")
		if res.Options.Trigger.Kind != "datadog" || res.Options.Trigger.Event != str(payload, "alert_type") {
			t.Fatalf("trigger = %+v", res.Options.Trigger)
		}
		if res.Options.Title != title {
			t.Fatalf("title = %q, want %q", res.Options.Title, title)
		}
		// The prompt is the composed alert, trimmed at both ends, so the alert
		// text appears unless it is only whitespace.
		if trimmed := strings.TrimSpace(text); trimmed != "" && !strings.Contains(res.Options.Prompt, trimmed) {
			t.Fatalf("prompt %q does not contain the alert text %q", res.Options.Prompt, trimmed)
		}
		if limit := 9*len(body) + 64; len(res.Options.Prompt) > limit {
			t.Fatalf("prompt is %d bytes, over the %d-byte payload bound", len(res.Options.Prompt), limit)
		}

		// No credential, a wrong token, a wrong secret, and altered bytes fail.
		strict := config.TriggerConfig{Name: "datadog", Type: "datadog", Secret: secret, AllowUnsigned: true}
		sig := hmacHex(secret, body)
		for name, bad := range map[string]http.Header{
			"no credential": {},
			"wrong token":   fuzzHeader("X-Mayfly-Token", secret+"x"),
			"wrong secret":  fuzzHeader("X-Hub-Signature-256", "sha256="+hmacHex(secret+"x", body)),
			"flipped hex":   fuzzHeader("X-Mayfly-Signature-256", fuzzFlipHex(sig)),
			"truncated":     fuzzHeader("X-Mayfly-Signature-256", sig[:len(sig)-1]),
		} {
			if _, err := Parse(strict, bad, body); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("%s returned %v, want ErrUnauthorized", name, err)
			}
		}
		if mutated := fuzzFlip(body); !bytes.Equal(mutated, body) {
			signed := fuzzHeader("X-Mayfly-Signature-256", sig)
			if _, err := Parse(strict, signed, mutated); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("mutated body returned %v, want ErrUnauthorized", err)
			}
		}
	})
}
