package triggers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/session"
)

func hmacHex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestGitHubSignatureAndPrompt(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"action": "opened",
		"number": 7,
		"repository": map[string]any{
			"full_name": "acme/app",
			"html_url":  "https://github.com/acme/app",
		},
		"pull_request": map[string]any{
			"title":    "Fix the widget",
			"html_url": "https://github.com/acme/app/pull/7",
			"body":     "Widget breaks on startup.",
		},
		"sender": map[string]any{"login": "alice"},
	})
	secret := "gh-secret"
	header := http.Header{}
	header.Set("X-GitHub-Event", "pull_request")
	header.Set("X-GitHub-Delivery", "delivery-1")
	header.Set("X-Hub-Signature-256", "sha256="+hmacHex(secret, body))
	cfg := config.TriggerConfig{Name: "github", Type: "github", Secret: secret}
	res, err := Parse(cfg, header, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Options.Trigger.Kind != "github" || res.Options.Trigger.Event != "pull_request" || res.Options.Trigger.Actor != "alice" {
		t.Fatalf("trigger = %+v", res.Options.Trigger)
	}
	if got := res.Options.Prompt; got == "" || !contains(got, "Fix the widget") || !contains(got, "acme/app") {
		t.Fatalf("prompt = %q", got)
	}

	header.Set("X-Hub-Signature-256", "sha256="+hmacHex("wrong", body))
	if _, err := Parse(cfg, header, body); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}

func TestGenericPromptAndOverrides(t *testing.T) {
	body := []byte(`{"prompt":"triage this","title":"t","ttl":"15m","skills":["reporting"],"mcp_servers":["echo"]}`)
	cfg := config.TriggerConfig{Name: "generic", Type: "generic", AllowUnsigned: true, Agent: "triage"}
	res, err := Parse(cfg, http.Header{"Content-Type": {"application/json"}}, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Options.Prompt != "triage this" || res.Options.Title != "t" || res.Options.TTL != 15*time.Minute {
		t.Fatalf("options = %+v", res.Options)
	}
	if len(res.Options.Skills) != 1 || res.Options.Skills[0] != "reporting" {
		t.Fatalf("skills = %v", res.Options.Skills)
	}
	if len(res.Options.MCPServers) != 1 || res.Options.MCPServers[0] != "echo" {
		t.Fatalf("mcp = %v", res.Options.MCPServers)
	}
	if res.Options.Agent != "triage" {
		t.Fatalf("agent = %q", res.Options.Agent)
	}
}

func TestGenericTokenAuthAndTemplate(t *testing.T) {
	body := []byte(`{"prompt":"hello","actor":"alice"}`)
	cfg := config.TriggerConfig{
		Name:     "generic",
		Type:     "generic",
		Secret:   "tok",
		Template: "Task from {{.Payload.actor}}: {{.Body}}",
	}
	header := http.Header{}
	header.Set("X-Mayfly-Token", "tok")
	header.Set("Content-Type", "application/json")
	res, err := Parse(cfg, header, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Options.Prompt != "Task from alice: hello" {
		t.Fatalf("prompt = %q", res.Options.Prompt)
	}
	header.Set("X-Mayfly-Token", "bad")
	if _, err := Parse(cfg, header, body); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}

func TestSlackChallengeAndSlashCommand(t *testing.T) {
	secret := "slack-secret"
	body := []byte(`{"type":"url_verification","challenge":"abc123"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%s:%s", ts, body)
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("X-Slack-Request-Timestamp", ts)
	header.Set("X-Slack-Signature", sig)
	cfg := config.TriggerConfig{Name: "slack", Type: "slack", Secret: secret}
	res, err := Parse(cfg, header, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Challenge["challenge"] != "abc123" {
		t.Fatalf("challenge = %v", res.Challenge)
	}

	form := []byte("text=deploy+failed&user_name=alice&channel_name=ops")
	mac = hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%s:%s", ts, form)
	header.Set("Content-Type", "application/x-www-form-urlencoded")
	header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	res, err = Parse(cfg, header, form)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(res.Options.Prompt, "deploy failed") || res.Options.Trigger.Actor != "alice" {
		t.Fatalf("options = %+v", res.Options)
	}

	header.Set("X-Slack-Signature", "v0=deadbeef")
	if _, err := Parse(cfg, header, form); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}

func TestGitHubRejectsStaticToken(t *testing.T) {
	body := []byte(`{"action":"opened","number":1,"repository":{"full_name":"acme/app"},"sender":{"login":"alice"}}`)
	cfg := config.TriggerConfig{Name: "github", Type: "github", Secret: "gh-secret"}
	header := http.Header{}
	header.Set("X-GitHub-Event", "pull_request")
	header.Set("X-Mayfly-Token", "gh-secret")
	if _, err := Parse(cfg, header, body); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replayed token was accepted: %v", err)
	}

	generic := config.TriggerConfig{Name: "generic", Type: "generic", Secret: "gh-secret"}
	header = http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("X-Mayfly-Token", "gh-secret")
	if _, err := Parse(generic, header, body); err != nil {
		t.Fatalf("generic trigger must still accept the shared token: %v", err)
	}
}

// TestGitHubAcceptsEitherSignatureHeader pins that a GitHub trigger takes the
// body signature from X-Hub-Signature-256 or X-Mayfly-Signature-256. Both carry
// the same HMAC over the same body and secret, so neither is weaker; the token
// header stays rejected (TestGitHubRejectsStaticToken).
func TestGitHubAcceptsEitherSignatureHeader(t *testing.T) {
	body := []byte(`{"action":"opened","number":1,"repository":{"full_name":"acme/app"},"sender":{"login":"alice"}}`)
	cfg := config.TriggerConfig{Name: "github", Type: "github", Secret: "gh-secret"}
	for _, name := range []string{"X-Hub-Signature-256", "X-Mayfly-Signature-256"} {
		header := http.Header{}
		header.Set("X-GitHub-Event", "pull_request")
		header.Set(name, "sha256="+hmacHex("gh-secret", body))
		if _, err := Parse(cfg, header, body); err != nil {
			t.Fatalf("%s: valid signature rejected: %v", name, err)
		}
		header.Set(name, "sha256="+hmacHex("wrong", body))
		if _, err := Parse(cfg, header, body); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%s: wrong signature accepted: %v", name, err)
		}
	}
}

func TestDatadogToken(t *testing.T) {
	body := []byte(`{"title":"High error rate","text":"5xx above threshold","alert_type":"error","url":"https://app.example/monitor/1"}`)
	cfg := config.TriggerConfig{Name: "datadog", Type: "datadog", Secret: "dd"}
	header := http.Header{}
	header.Set("X-Mayfly-Token", "dd")
	res, err := Parse(cfg, header, body)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(res.Options.Prompt, "High error rate") || !contains(res.Options.Prompt, "5xx above threshold") {
		t.Fatalf("prompt = %q", res.Options.Prompt)
	}
	if res.Options.Trigger.Event != "error" {
		t.Fatalf("trigger = %+v", res.Options.Trigger)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestGitHubTitleIsBounded(t *testing.T) {
	secret := "gh-secret"
	for name, repo := range map[string]string{
		"ascii":     strings.Repeat("r", 5000),
		"multibyte": strings.Repeat("日", 2000),
	} {
		t.Run(name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"action":     "opened",
				"number":     7,
				"repository": map[string]any{"full_name": repo},
				"pull_request": map[string]any{
					"title": "Fix the widget",
					"body":  "details",
				},
				"sender": map[string]any{"login": "alice"},
			})
			header := http.Header{}
			header.Set("X-GitHub-Event", "pull_request")
			header.Set("X-GitHub-Delivery", "delivery-1")
			header.Set("X-Hub-Signature-256", "sha256="+hmacHex(secret, body))
			res, err := Parse(config.TriggerConfig{Name: "github", Type: "github", Secret: secret}, header, body)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(res.Options.Title); got > session.MaxTitleBytes {
				t.Fatalf("title is %d bytes, over %d", got, session.MaxTitleBytes)
			}
			if !utf8.ValidString(res.Options.Title) {
				t.Fatalf("title is not valid UTF-8: %q", res.Options.Title)
			}
			if !strings.HasPrefix(res.Options.Prompt, "Pull request") {
				t.Fatalf("prompt = %q", res.Options.Prompt)
			}
		})
	}
}

func TestGenericAndDatadogTitlesAreBounded(t *testing.T) {
	long := strings.Repeat("t", 5000)
	generic, err := Parse(config.TriggerConfig{Name: "generic", Type: "generic", AllowUnsigned: true},
		http.Header{"Content-Type": []string{"application/json"}},
		[]byte(fmt.Sprintf(`{"prompt":"triage","title":%q}`, long)))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(generic.Options.Title); got > session.MaxTitleBytes {
		t.Fatalf("generic title is %d bytes, over %d", got, session.MaxTitleBytes)
	}

	datadog, err := Parse(config.TriggerConfig{Name: "datadog", Type: "datadog", AllowUnsigned: true},
		http.Header{"Content-Type": []string{"application/json"}},
		[]byte(fmt.Sprintf(`{"title":%q,"text":"alert","alert_type":"error"}`, long)))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(datadog.Options.Title); got > session.MaxTitleBytes {
		t.Fatalf("datadog title is %d bytes, over %d", got, session.MaxTitleBytes)
	}
}

func TestGitHubTitleReplacesInvalidUTF8(t *testing.T) {
	const secret = "gh-secret"
	body := []byte(`{"repository":{"full_name":"acme/app"},"sender":{"login":"alice"}}`)
	header := http.Header{}
	header.Set("X-GitHub-Event", "push\xff")
	header.Set("X-GitHub-Delivery", "delivery-1")
	header.Set("X-Hub-Signature-256", "sha256="+hmacHex(secret, body))
	res, err := Parse(config.TriggerConfig{Name: "github", Type: "github", Secret: secret}, header, body)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"title":       res.Options.Title,
		"event":       res.Options.Trigger.Event,
		"delivery":    res.DeliveryKey,
		"prompt body": res.Options.Prompt,
	} {
		if !utf8.ValidString(value) {
			t.Fatalf("%s is not valid UTF-8: %q", name, value)
		}
	}
	if !strings.Contains(res.Options.Title, "\uFFFD") || !strings.Contains(res.Options.Title, "push") {
		t.Fatalf("title = %q, want the event name with a replacement rune", res.Options.Title)
	}
}

func TestDeliveryKeyMustBeValidUTF8(t *testing.T) {
	gh := func(delivery string) (*Result, error) {
		body := []byte(`{}`)
		header := http.Header{}
		header.Set("X-GitHub-Event", "push")
		header.Set("X-GitHub-Delivery", delivery)
		header.Set("X-Hub-Signature-256", "sha256="+hmacHex("gh-secret", body))
		return Parse(config.TriggerConfig{Name: "github", Type: "github", Secret: "gh-secret"}, header, body)
	}
	// Two distinct invalid deliveries must not collapse to one dedupe key.
	for _, delivery := range []string{"\xb0", "\xb1"} {
		if _, err := gh(delivery); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("delivery %q returned %v, want ErrBadRequest", delivery, err)
		}
	}
	const uuid = "9f0d6a2e-4c3b-4a1e-9f8d-2b7c6e5a1d30"
	res, err := gh(uuid)
	if err != nil {
		t.Fatal(err)
	}
	if res.DeliveryKey != uuid || res.Options.Trigger.Delivery != uuid {
		t.Fatalf("delivery key = %q trigger delivery = %q, want %q", res.DeliveryKey, res.Options.Trigger.Delivery, uuid)
	}

	// The Idempotency-Key fallback follows the same rule for every kind.
	for _, kind := range []string{"generic", "datadog"} {
		cfg := config.TriggerConfig{Name: kind, Type: kind, AllowUnsigned: true}
		header := http.Header{}
		header.Set("Idempotency-Key", "\xb0")
		if _, err := Parse(cfg, header, []byte(`{}`)); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("%s with an invalid Idempotency-Key returned %v, want ErrBadRequest", kind, err)
		}
		header.Set("Idempotency-Key", "  idem-1 \n")
		res, err := Parse(cfg, header, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if res.DeliveryKey != "idem-1" || res.Options.Trigger.Delivery != "idem-1" {
			t.Fatalf("%s delivery key = %q trigger delivery = %q, want idem-1", kind, res.DeliveryKey, res.Options.Trigger.Delivery)
		}
	}

	// A Slack trigger_id is a delivery key too.
	slack := config.TriggerConfig{Name: "slack", Type: "slack", AllowUnsigned: true}
	if _, err := Parse(slack, http.Header{}, []byte("command=%2Fmayfly&text=hi&trigger_id=%B0")); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("slack trigger_id with invalid UTF-8 returned %v, want ErrBadRequest", err)
	}
}

func TestTruncateKeepsRunes(t *testing.T) {
	for _, tc := range []struct {
		in    string
		max   int
		valid bool
	}{
		{"hello", 10, true},
		{"hello", 3, true},
		{"a" + strings.Repeat("日", 10), 5, true},
		{"a" + strings.Repeat("日", 10), 1, true},
		{"a\xffb", 10, true},
		{"a\xffb", 2, true},
	} {
		got := truncate(tc.in, tc.max)
		if len(got) > tc.max {
			t.Fatalf("truncate(%q, %d) = %q is over the cap", tc.in, tc.max, got)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("truncate(%q, %d) = %q is not valid UTF-8", tc.in, tc.max, got)
		}
	}
}
