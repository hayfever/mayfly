package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
)

// webhookBody returns a JSON body of exactly size bytes.
func webhookBody(size int) string {
	const prefix, suffix = `{"prompt":"`, `"}`
	return prefix + strings.Repeat("a", size-len(prefix)-len(suffix)) + suffix
}

func newWebhookServer(t *testing.T, cfg *config.Config) http.Handler {
	t.Helper()
	cfg.Runtime.Image = "example/agent:1"
	return New(cfg, testManager(t, cfg), testQueue(t), testLogger()).Handler()
}

func postWebhook(handler http.Handler, name, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/hooks/"+name, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func checkPayloadTooLarge(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Code != codePayloadTooLarge {
		t.Fatalf("code = %q, want %q", env.Error.Code, codePayloadTooLarge)
	}
}

func TestWebhookBodyLimitDefault(t *testing.T) {
	cfg := config.Default()
	cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
	handler := newWebhookServer(t, cfg)

	checkPayloadTooLarge(t, postWebhook(handler, "generic", webhookBody(int(config.DefaultWebhookMaxBodyBytes)+1)))
	if rec := postWebhook(handler, "generic", webhookBody(64)); rec.Code != http.StatusAccepted {
		t.Fatalf("body under the default limit: status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWebhookBodyLimitServerDefault(t *testing.T) {
	cfg := config.Default()
	cfg.Server.WebhookMaxBodyBytes = 64
	cfg.Triggers = []config.TriggerConfig{{Name: "generic", Type: "generic", AllowUnsigned: true}}
	handler := newWebhookServer(t, cfg)

	checkPayloadTooLarge(t, postWebhook(handler, "generic", webhookBody(65)))
	if rec := postWebhook(handler, "generic", webhookBody(64)); rec.Code != http.StatusAccepted {
		t.Fatalf("body at the server limit: status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWebhookBodyLimitTriggerOverride(t *testing.T) {
	cfg := config.Default()
	cfg.Triggers = []config.TriggerConfig{
		{Name: "small", Type: "generic", AllowUnsigned: true, MaxBodyBytes: 64},
		{Name: "large", Type: "generic", AllowUnsigned: true, MaxBodyBytes: 4096},
	}
	handler := newWebhookServer(t, cfg)

	checkPayloadTooLarge(t, postWebhook(handler, "small", webhookBody(65)))
	if rec := postWebhook(handler, "small", webhookBody(64)); rec.Code != http.StatusAccepted {
		t.Fatalf("body at the trigger limit: status = %d body=%s", rec.Code, rec.Body.String())
	}
	for _, size := range []int{1024, 4096} {
		if rec := postWebhook(handler, "large", webhookBody(size)); rec.Code != http.StatusAccepted {
			t.Fatalf("large trigger body %d: status = %d body=%s", size, rec.Code, rec.Body.String())
		}
	}
}

// TestWebhookBodyLimitKeepsRateTokens checks that an oversize body is refused
// before signature verification and before the rate limit, so repeated bad
// deliveries cannot exhaust a trigger's budget.
func TestWebhookBodyLimitKeepsRateTokens(t *testing.T) {
	cfg := config.Default()
	cfg.Server.WebhookMaxBodyBytes = 64
	cfg.Triggers = []config.TriggerConfig{{
		Name:          "generic",
		Type:          "generic",
		AllowUnsigned: true,
		RateLimit:     &config.RateLimitConfig{Requests: 1, Interval: config.Duration(time.Hour), Burst: 1},
	}}
	handler := newWebhookServer(t, cfg)

	for range 3 {
		checkPayloadTooLarge(t, postWebhook(handler, "generic", webhookBody(65)))
	}
	if rec := postWebhook(handler, "generic", webhookBody(64)); rec.Code != http.StatusAccepted {
		t.Fatalf("first accepted body: status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postWebhook(handler, "generic", webhookBody(64)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second accepted body: status = %d body=%s", rec.Code, rec.Body.String())
	}
}
