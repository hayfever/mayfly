package broker

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/config"
)

// capturedLogger returns a logger that keeps every line in buf, so a test can
// assert on what reached the operator.
func capturedLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

func TestRedactURL(t *testing.T) {
	longPath := strings.Repeat("a", 4096)
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"userinfo, query and fragment", "https://user:pass@example.com/v1?api_key=secret#frag", "https://example.com/v1"},
		{"clean url", "http://example.com/v1", "http://example.com/v1"},
		{"query only", "https://example.com/v1/chat/completions?key=x", "https://example.com/v1/chat/completions"},
		{"empty", "", ""},
		{"ipv6 host with port", "http://[::1]:8080/v1", "http://[::1]:8080/v1"},
		{"userinfo without password", "https://user@host/path", "https://host/path"},
		{"query on bare host", "http://host?x=1", "http://host"},
		{"encoded userinfo", "https://user%3Apass@example.com/", "https://example.com/"},
		{"long path", "https://example.com/" + longPath + "?key=secret", "https://example.com/" + longPath},
		{"empty authority", "//user:pass@//", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactURL(tc.raw); got != tc.want {
				t.Fatalf("redactURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			if again := redactURL(tc.want); again != tc.want {
				t.Fatalf("redactURL(%q) = %q, want the value back", tc.want, again)
			}
		})
	}
}

func TestRedactURLUnparsableInput(t *testing.T) {
	const raw = "://user:pass@example.com/v1?key=secret"
	got := redactURL(raw)
	if got != unparsableURL {
		t.Fatalf("redactURL(%q) = %q, want the placeholder", raw, got)
	}
	if strings.ContainsAny(got, "@?#") {
		t.Fatalf("placeholder %q looks like credentials", got)
	}
	if strings.Contains(got, "user:pass") || strings.Contains(got, "key=secret") {
		t.Fatalf("placeholder %q echoes the input", got)
	}
	if again := redactURL(got); again != got {
		t.Fatalf("redactURL(%q) = %q, want the placeholder back", got, again)
	}
}

func TestRedactError(t *testing.T) {
	cause := &url.Error{Op: "Post", URL: "https://user:pass@example.com/v1?key=secret", Err: errors.New("connection reset")}

	t.Run("direct", func(t *testing.T) {
		got := redactError(cause)
		const want = `Post "https://example.com/v1": connection reset`
		if got.Error() != want {
			t.Fatalf("redactError text = %q, want %q", got.Error(), want)
		}
		var uerr *url.Error
		if !errors.As(got, &uerr) {
			t.Fatalf("redactError dropped the *url.Error type")
		}
	})

	t.Run("wrapped", func(t *testing.T) {
		wrapped := fmt.Errorf("chat call: %w", cause)
		got := redactError(wrapped)
		if strings.Contains(got.Error(), "user:pass") || strings.Contains(got.Error(), "key=secret") {
			t.Fatalf("redactError leaks the url: %s", got.Error())
		}
		const want = `chat call: Post "https://example.com/v1": connection reset`
		if got.Error() != want {
			t.Fatalf("redactError text = %q, want %q", got.Error(), want)
		}
		if !errors.Is(got, cause) {
			t.Fatalf("redactError broke the unwrap chain")
		}
	})

	t.Run("other errors pass through", func(t *testing.T) {
		plain := errors.New("plain failure")
		got := redactError(plain)
		if !errors.Is(got, plain) || got.Error() != plain.Error() {
			t.Fatalf("redactError changed a plain error: %v", got)
		}
		if got := redactError(nil); got != nil {
			t.Fatalf("redactError(nil) = %v", got)
		}
	})
}

func TestInvalidBaseURLLogRedactsCredentials(t *testing.T) {
	var logs bytes.Buffer
	b := New(config.LLMConfig{
		BaseURL:      "httpx://user:pass@example.com/v1?key=secret#frag",
		Model:        "stub",
		AllowModels:  []string{"stub"},
		MaxBodyBytes: 1 << 20,
	}, capturedLogger(&logs))

	rec := httptest.NewRecorder()
	res := b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions",
		strings.NewReader(`{"model":"stub","messages":[]}`)), "stub", nil)
	if res.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", res.Status, http.StatusInternalServerError)
	}

	logged := logs.String()
	for _, secret := range []string{"user:pass", "key=secret", "frag"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaks %q: %s", secret, logged)
		}
	}
	if !strings.Contains(logged, "httpx://example.com/v1") {
		t.Fatalf("log loses the useful url: %s", logged)
	}

	body := rec.Body.String()
	if strings.Contains(body, "user:pass") || strings.Contains(body, "key=secret") {
		t.Fatalf("response leaks credentials: %s", body)
	}
}

func TestInvalidBaseURLLogKeepsUsefulURL(t *testing.T) {
	var logs bytes.Buffer
	b := New(config.LLMConfig{
		BaseURL:      "ftp://example.com/v1",
		Model:        "stub",
		AllowModels:  []string{"stub"},
		MaxBodyBytes: 1 << 20,
	}, capturedLogger(&logs))

	rec := httptest.NewRecorder()
	res := b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions",
		strings.NewReader(`{"model":"stub","messages":[]}`)), "stub", nil)
	if res.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", res.Status, http.StatusInternalServerError)
	}
	if logged := logs.String(); !strings.Contains(logged, "ftp://example.com/v1") {
		t.Fatalf("log loses host and path: %s", logged)
	}
}

// closedUpstreamURL starts a server, keeps its credentialed URL, and closes the
// listener so the next request fails in transport.
func closedUpstreamURL(t *testing.T, userinfo, suffix string) string {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	raw := strings.Replace(upstream.URL, "http://", "http://"+userinfo+"@", 1) + suffix
	upstream.Close()
	return raw
}

func TestLLMTransportErrorRedactsURL(t *testing.T) {
	baseURL := closedUpstreamURL(t, "user:pass", "/v1?key=secret")
	var logs bytes.Buffer
	b := New(config.LLMConfig{
		BaseURL:      baseURL,
		Model:        "stub",
		AllowModels:  []string{"stub"},
		MaxBodyBytes: 1 << 20,
	}, capturedLogger(&logs))

	rec := httptest.NewRecorder()
	res := b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions",
		strings.NewReader(`{"model":"stub","messages":[]}`)), "stub", nil)
	if res.Status != http.StatusBadGateway || !res.TransportErr {
		t.Fatalf("status = %d transportErr = %v, want 502 and true", res.Status, res.TransportErr)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "model backend unavailable") {
		t.Fatalf("client message = %s", body)
	}
	if strings.Contains(body, "user:pass") || strings.Contains(body, "key=secret") {
		t.Fatalf("client message leaks credentials: %s", body)
	}

	logged := logs.String()
	for _, secret := range []string{"user:pass", "key=secret"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaks %q: %s", secret, logged)
		}
	}
	if !strings.Contains(logged, "llm upstream request failed") {
		t.Fatalf("missing the failure log: %s", logged)
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}
	if !strings.Contains(logged, u.Host) {
		t.Fatalf("log loses the upstream host %q: %s", u.Host, logged)
	}
}

func TestMCPTransportErrorRedactsURL(t *testing.T) {
	serverURL := closedUpstreamURL(t, "user:pass", "/mcp?key=secret")
	var logs bytes.Buffer
	b := New(config.LLMConfig{MaxBodyBytes: 1 << 20}, capturedLogger(&logs))

	rec := httptest.NewRecorder()
	b.ServeMCP(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/mcp/remote", strings.NewReader(`{}`)),
		config.MCPServerConfig{Name: "remote", Transport: "http", URL: serverURL})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "MCP server unavailable") {
		t.Fatalf("client message = %s", body)
	}
	if strings.Contains(body, "user:pass") || strings.Contains(body, "key=secret") {
		t.Fatalf("client message leaks credentials: %s", body)
	}

	logged := logs.String()
	for _, secret := range []string{"user:pass", "key=secret"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaks %q: %s", secret, logged)
		}
	}
	for _, want := range []string{"mcp upstream request failed", "remote"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log loses %q: %s", want, logged)
		}
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}
	if !strings.Contains(logged, u.Host) {
		t.Fatalf("log loses the upstream host %q: %s", u.Host, logged)
	}
}
