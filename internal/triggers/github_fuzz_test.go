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
	"github.com/hayfever/mayfly/internal/textutil"
)

// FuzzGitHubParse checks the GitHub trigger: a signed delivery parses for any
// event and payload, the delivery header becomes the delivery key, the static
// token header is never accepted, and altered input is unauthorized. The
// prompt derived from an arbitrary payload stays within the documented
// 12 KiB summary bound.
func FuzzGitHubParse(f *testing.F) {
	f.Add("gh-secret", "push", "d-1", []byte(`{"ref":"refs/heads/main","compare":"https://github.com/acme/app/compare/a...b","repository":{"full_name":"acme/app","html_url":"https://github.com/acme/app"},"sender":{"login":"alice"},"commits":[{"message":"fix the widget"},{"message":"add a test"}]}`))
	f.Add("gh-secret", "pull_request", "d-2", []byte(`{"action":"opened","number":7,"pull_request":{"title":"Fix the widget","body":"details here","html_url":"https://github.com/acme/app/pull/7"},"repository":{"full_name":"acme/app"},"sender":{"login":"alice"}}`))
	f.Add("gh-secret", "issues", "d-3", []byte(`{"action":"opened","issue":{"number":9,"title":"Broken deploy","body":"steps to reproduce","html_url":"https://github.com/acme/app/issues/9"},"repository":{"full_name":"acme/app"},"sender":{"login":"bob"}}`))
	f.Add("gh-secret", "issue_comment", "d-4", []byte(`{"action":"created","issue":{"number":9,"title":"Broken deploy"},"comment":{"body":"same here","html_url":"https://github.com/acme/app/issues/9#issuecomment-1"},"repository":{"full_name":"acme/app"},"sender":{"login":"carol"}}`))
	f.Add("gh-secret", "workflow_run", "d-5", []byte(`{"workflow_run":{"name":"CI","conclusion":"failure","html_url":"https://github.com/acme/app/actions/runs/1"},"repository":{"full_name":"acme/app"},"sender":{"login":"dave"}}`))
	for _, repo := range []string{
		strings.Repeat("a", 188),       // title is exactly MaxTitleBytes
		strings.Repeat("a", 189),       // one byte over
		strings.Repeat("a", 186) + "日", // the cut lands inside a rune
	} {
		f.Add("gh-secret", "pull_request", "d-edge", []byte(fmt.Sprintf(
			`{"action":"opened","number":7,"pull_request":{"title":"x"},"repository":{"full_name":%q}}`, repo)))
	}
	f.Fuzz(func(t *testing.T, secret, event, delivery string, body []byte) {
		cfg := config.TriggerConfig{Name: "github", Type: "github", Secret: secret}
		h := fuzzHeader(
			"X-GitHub-Event", event,
			"X-GitHub-Delivery", delivery,
			"X-Hub-Signature-256", "sha256="+hmacHex(secret, body),
		)

		// Without a secret only allow_unsigned accepts the delivery.
		unsigned := config.TriggerConfig{Name: "github", Type: "github"}
		if _, err := Parse(unsigned, h, body); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("unsigned github delivery returned %v, want ErrUnauthorized", err)
		}
		unsigned.AllowUnsigned = true
		if _, err := Parse(unsigned, h, body); errors.Is(err, ErrUnauthorized) {
			t.Fatalf("allow_unsigned rejected an unsigned github delivery")
		}

		if secret == "" {
			return
		}
		res, err := Parse(cfg, h, body)
		if errors.Is(err, ErrUnauthorized) {
			t.Fatalf("valid github signature rejected: %v", err)
		}
		fuzzCheckError(t, err)
		if err != nil {
			return
		}
		if res == nil {
			t.Fatal("nil result with nil error")
		}
		if want := textutil.Valid(event); res.Options.Trigger.Kind != "github" || res.Options.Trigger.Event != want {
			t.Fatalf("trigger = %+v", res.Options.Trigger)
		}
		if want := textutil.Valid(delivery); res.DeliveryKey != want {
			t.Fatalf("delivery key = %q, want %q", res.DeliveryKey, want)
		}
		if res.Options.Trigger.Delivery != res.DeliveryKey {
			t.Fatalf("trigger delivery = %q, want %q", res.Options.Trigger.Delivery, res.DeliveryKey)
		}
		if limit := (12 << 10) + len("…"); len(res.Options.Prompt) > limit {
			t.Fatalf("prompt is %d bytes, over the %d-byte summary bound", len(res.Options.Prompt), limit)
		}
		if len(res.Options.Title) > session.MaxTitleBytes {
			t.Fatalf("title is %d bytes, over the %d-byte cap", len(res.Options.Title), session.MaxTitleBytes)
		}
		if !utf8.ValidString(res.Options.Title) {
			t.Fatalf("title is not valid UTF-8: %q", res.Options.Title)
		}

		// The static token is not a GitHub credential.
		tokenOnly := fuzzHeader(
			"X-GitHub-Event", event,
			"X-GitHub-Delivery", delivery,
			"X-Mayfly-Token", secret,
		)
		if _, err := Parse(cfg, tokenOnly, body); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("github token header returned %v, want ErrUnauthorized", err)
		}

		// A wrong secret, a mutated signature, and a mutated body all fail.
		sig := hmacHex(secret, body)
		for name, bad := range map[string]http.Header{
			"wrong secret": fuzzHeader("X-Hub-Signature-256", "sha256="+hmacHex(secret+"x", body)),
			"flipped hex":  fuzzHeader("X-Hub-Signature-256", "sha256="+fuzzFlipHex(sig)),
			"truncated":    fuzzHeader("X-Hub-Signature-256", sig[:len(sig)-1]),
			"prefix only":  fuzzHeader("X-Hub-Signature-256", "sha256="),
		} {
			bad.Set("X-GitHub-Event", event)
			bad.Set("X-GitHub-Delivery", delivery)
			if _, err := Parse(cfg, bad, body); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("%s returned %v, want ErrUnauthorized", name, err)
			}
		}
		if mutated := fuzzFlip(body); !bytes.Equal(mutated, body) {
			if _, err := Parse(cfg, h, mutated); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("mutated body returned %v, want ErrUnauthorized", err)
			}
		}
	})
}
