package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/metrics"
)

// TestUnauthenticatedRouteAudit checks each route's authentication on purpose.
// Every /api/v1 route with a scope rejects a missing or wrong token; the
// attach socket requires its one-time ticket. The remaining unauthenticated
// routes are the health probes, the static UI, and the webhook receivers,
// which verify the sender themselves.
func TestUnauthenticatedRouteAudit(t *testing.T) {
	server, _ := sessionServer(t, 0)
	server.cfg.Triggers = []config.TriggerConfig{{Name: "github", Type: "github", Secret: "s3cret"}}
	server.triggers["github"] = server.cfg.Triggers[0]
	server.metrics = metrics.New(metrics.Sources{}, metrics.BuildInfo{})
	handler := server.Handler()

	do := func(method, target, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	for _, route := range server.apiRoutes() {
		method, pattern, ok := strings.Cut(route.pattern, " ")
		if !ok {
			t.Fatalf("route %q has no method", route.pattern)
		}
		target := strings.NewReplacer("{id}", "abc", "{name}", "github").Replace(pattern)
		rec := do(method, target, "", "{}")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a token = %d body=%s", method, target, rec.Code, rec.Body.String())
		}
		if body := decodeEnvelope(t, rec); body.Code != codeUnauthenticated {
			t.Fatalf("%s %s code = %q", method, target, body.Code)
		}
		rec = do(method, target, "wrong-token", "{}")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with a wrong token = %d", method, target, rec.Code)
		}
	}

	// The attach socket has no scope because a one-time ticket authenticates
	// it. Without a ticket it is still a 401, not an open stream.
	rec := do(http.MethodGet, "/api/v1/sessions/abc/ws", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("attach socket without a ticket = %d body=%s", rec.Code, rec.Body.String())
	}

	// Health probes stay open by design.
	if rec := do(http.MethodGet, "/healthz", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
	if rec := do(http.MethodGet, "/readyz", "", ""); rec.Code == http.StatusUnauthorized {
		t.Fatalf("readyz requires a token: %d", rec.Code)
	}
	if rec := do(http.MethodGet, "/", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("UI index = %d", rec.Code)
	}

	// A webhook verifies the sender itself: unsigned GitHub traffic is a 401.
	if rec := do(http.MethodPost, "/hooks/github", "", `{"action":"opened"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPost, "/hooks/missing", "", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown webhook = %d", rec.Code)
	}

	// The internal runtime protocol authenticates with the session token.
	internal := []struct {
		method string
		target string
	}{
		{http.MethodPost, "/internal/v1/sessions/abc/events"},
		{http.MethodPost, "/internal/v1/sessions/abc/journal"},
		{http.MethodGet, "/internal/v1/sessions/abc/tools"},
		{http.MethodPost, "/internal/v1/sessions/abc/tools/calls"},
		{http.MethodGet, "/internal/v1/sessions/abc/inbox"},
		{http.MethodPost, "/internal/v1/sessions/abc/artifacts"},
		{http.MethodPost, "/internal/v1/sessions/abc/finalize"},
		{http.MethodPost, "/internal/v1/llm/chat/completions"},
		{http.MethodPost, "/internal/v1/mcp/github"},
	}
	for _, route := range internal {
		if rec := do(route.method, route.target, "", "{}"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a session token = %d", route.method, route.target, rec.Code)
		}
	}

	// Metrics on the main port requires the admin scope by default.
	if rec := do(http.MethodGet, "/metrics", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("metrics without a token = %d", rec.Code)
	}
	if rec := do(http.MethodGet, "/metrics", "test-admin", ""); rec.Code != http.StatusOK {
		t.Fatalf("metrics with an admin token = %d", rec.Code)
	}
}
