package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/metrics"
)

// scopeServer returns a server whose token set the test controls.
func scopeServer(t *testing.T, tokens ...config.APITokenConfig) *Server {
	t.Helper()
	s := New(config.Default(), nil, nil, testLogger())
	s.tokens = tokens
	return s
}

func serve(t *testing.T, handler http.Handler, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestRouteScopesEnforced drives every registered /api/v1 route through its
// wrapper: a missing token is refused, a token with the route's scope passes,
// and a token without the scope is forbidden.
func TestRouteScopesEnforced(t *testing.T) {
	const adminSecret = "admin-secret"
	s := scopeServer(t,
		config.APITokenConfig{Name: "root", Scopes: []string{config.ScopeAdmin}, Secret: adminSecret},
		config.APITokenConfig{Name: "wrong", Scopes: []string{config.ScopeQueueWrite}, Secret: "wrong-secret"},
	)
	for _, route := range s.apiRoutes() {
		method, path, ok := strings.Cut(route.pattern, " ")
		if !ok {
			t.Fatalf("route %q has no method", route.pattern)
		}
		if route.scope == "" {
			continue
		}
		if !config.ValidScope(route.scope) {
			t.Errorf("%s declares unknown scope %q", route.pattern, route.scope)
		}
		// Each route starts with a full failure budget; the loop makes more
		// than one burst of refused requests.
		s.authFailures = newAuthFailureLimiter()
		allowed := config.APITokenConfig{Name: "scoped", Scopes: []string{route.scope}, Secret: "scoped-secret"}
		s.tokens = []config.APITokenConfig{allowed}
		stub := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }
		handler := s.authorizedRoute(apiRoute{pattern: route.pattern, scope: route.scope, handler: stub})

		if rec := serve(t, handler, method, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", route.pattern, rec.Code)
		}
		if rec := serve(t, handler, method, path, "not-a-token"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with a wrong token = %d, want 401", route.pattern, rec.Code)
		}
		if rec := serve(t, handler, method, path, allowed.Secret); rec.Code != http.StatusNoContent {
			t.Errorf("%s with %s = %d body=%s", route.pattern, route.scope, rec.Code, rec.Body.String())
		}
		if route.scope != config.ScopeQueueWrite {
			s.tokens = []config.APITokenConfig{{Name: "wrong", Scopes: []string{config.ScopeQueueWrite}, Secret: "wrong-secret"}}
			rec := serve(t, handler, method, path, "wrong-secret")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s with a foreign scope = %d, want 403", route.pattern, rec.Code)
			}
			if body := decodeEnvelope(t, rec); body.Code != codeForbidden {
				t.Fatalf("%s forbidden code = %q", route.pattern, body.Code)
			}
		}
		s.tokens = []config.APITokenConfig{allowed}
	}
}

// TestAttachWebSocketRouteHasNoScope keeps the ticket-authenticated route out
// of token authentication.
func TestAttachWebSocketRouteHasNoScope(t *testing.T) {
	s := scopeServer(t)
	for _, route := range s.apiRoutes() {
		if route.pattern != "GET /api/v1/sessions/{id}/ws" {
			continue
		}
		if route.scope != "" {
			t.Fatalf("scope = %q, want empty", route.scope)
		}
		reached := false
		handler := s.authorizedRoute(apiRoute{pattern: route.pattern, handler: func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.WriteHeader(http.StatusNoContent)
		}})
		if rec := serve(t, handler, http.MethodGet, "/api/v1/sessions/x/ws", ""); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d", rec.Code)
		}
		if !reached {
			t.Fatal("handler was not reached without a bearer token")
		}
		return
	}
	t.Fatal("attach WebSocket route is not registered")
}

func TestExpiredTokenIsRejected(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	s := scopeServer(t, config.APITokenConfig{Name: "old", Scopes: []string{config.ScopeQueueRead}, Secret: "old-secret", ExpiresAt: &past})
	stub := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }
	handler := s.authorizedRoute(apiRoute{pattern: "GET /api/v1/queue", scope: config.ScopeQueueRead, handler: stub})
	if rec := serve(t, handler, http.MethodGet, "/api/v1/queue", "old-secret"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token status = %d, want 401", rec.Code)
	}

	future := time.Now().Add(time.Minute)
	s.tokens = []config.APITokenConfig{{Name: "new", Scopes: []string{config.ScopeQueueRead}, Secret: "new-secret", ExpiresAt: &future}}
	if rec := serve(t, handler, http.MethodGet, "/api/v1/queue", "new-secret"); rec.Code != http.StatusNoContent {
		t.Fatalf("valid token status = %d, want 204", rec.Code)
	}
}

func TestMissingTokenCode(t *testing.T) {
	s := scopeServer(t)
	handler := s.authorizedRoute(apiRoute{pattern: "GET /api/v1/queue", scope: config.ScopeQueueRead, handler: func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler ran without a token")
	}})
	rec := serve(t, handler, http.MethodGet, "/api/v1/queue", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := decodeEnvelope(t, rec); body.Code != codeUnauthenticated {
		t.Fatalf("code = %q, want %q", body.Code, codeUnauthenticated)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("body leaked token material: %s", rec.Body.String())
	}
}

func TestAuditLineForMutatingRequest(t *testing.T) {
	var buf bytes.Buffer
	// audit capture is separate from the process logger after #46.
	logger := testLogger()
	cfg := config.Default()
	cfg.Server.Tokens = []config.APITokenConfig{{Name: "ci", Scopes: []string{config.ScopeSchedulesWrite}, Secret: "ci-secret-value"}}
	s := New(cfg, nil, nil, logger, WithAuditLogger(NewAuditLogger(&buf)))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/schedules/{name}/run", s.authorizedRoute(apiRoute{
		pattern: "POST /api/v1/schedules/{name}/run",
		scope:   config.ScopeSchedulesWrite,
		handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) },
	}))
	handler := s.observeRequests(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/schedules/nightly/run", nil)
	req.Header.Set("Authorization", "Bearer ci-secret-value")
	req.Header.Set("X-Request-Id", "req-42")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	line := auditLines(t, &buf)
	if len(line) != 1 {
		t.Fatalf("audit lines = %d, want 1: %s", len(line), buf.String())
	}
	event := line[0]
	want := map[string]any{
		"msg":        "audit",
		"level":      "INFO",
		"token_name": "ci",
		"method":     http.MethodPost,
		"route":      "/api/v1/schedules/{name}/run",
		"target":     "nightly",
		"status":     float64(http.StatusAccepted),
		"request_id": "req-42",
	}
	for key, value := range want {
		if event[key] != value {
			t.Errorf("%s = %v, want %v", key, event[key], value)
		}
	}
	if strings.Contains(buf.String(), "ci-secret-value") {
		t.Fatalf("audit line contains the token secret: %s", buf.String())
	}
}

func TestAuditLineForForbiddenRequest(t *testing.T) {
	var buf bytes.Buffer
	// audit capture is separate from the process logger after #46.
	logger := testLogger()
	cfg := config.Default()
	cfg.Server.Tokens = []config.APITokenConfig{{Name: "reader", Scopes: []string{config.ScopeSessionsRead}, Secret: "reader-secret"}}
	s := New(cfg, nil, nil, logger, WithAuditLogger(NewAuditLogger(&buf)))

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.authorizedRoute(apiRoute{
		pattern: "DELETE /api/v1/sessions/{id}",
		scope:   config.ScopeSessionsWrite,
		handler: func(http.ResponseWriter, *http.Request) { t.Fatal("handler ran without the scope") },
	}))
	rec := serve(t, s.observeRequests(mux), http.MethodDelete, "/api/v1/sessions/sess-7", "reader-secret")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
	lines := auditLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1", len(lines))
	}
	if lines[0]["token_name"] != "reader" || lines[0]["target"] != "sess-7" || lines[0]["status"] != float64(http.StatusForbidden) {
		t.Fatalf("audit line = %+v", lines[0])
	}
}

func TestAuditSkipsReadRequests(t *testing.T) {
	var buf bytes.Buffer
	// audit capture is separate from the process logger after #46.
	logger := testLogger()
	cfg := config.Default()
	cfg.Server.Tokens = []config.APITokenConfig{{Name: "reader", Scopes: []string{config.ScopeQueueRead}, Secret: "reader-secret"}}
	s := New(cfg, nil, nil, logger, WithAuditLogger(NewAuditLogger(&buf)))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/queue", s.authorizedRoute(apiRoute{
		pattern: "GET /api/v1/queue",
		scope:   config.ScopeQueueRead,
		handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
	}))
	if rec := serve(t, s.observeRequests(mux), http.MethodGet, "/api/v1/queue", "reader-secret"); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if lines := auditLines(t, &buf); len(lines) != 0 {
		t.Fatalf("read request was audited: %+v", lines)
	}
}

func TestAuditTargetFromHandler(t *testing.T) {
	var buf bytes.Buffer
	// audit capture is separate from the process logger after #46.
	logger := testLogger()
	cfg := config.Default()
	cfg.Server.Tokens = []config.APITokenConfig{{Name: "writer", Scopes: []string{config.ScopeSessionsWrite}, Secret: "writer-secret"}}
	s := New(cfg, nil, nil, logger, WithAuditLogger(NewAuditLogger(&buf)))
	handler := s.authorizedRoute(apiRoute{
		pattern: "POST /api/v1/sessions",
		scope:   config.ScopeSessionsWrite,
		handler: func(w http.ResponseWriter, r *http.Request) {
			setAuditTarget(r, "sess-new")
			w.WriteHeader(http.StatusCreated)
		},
	})
	if rec := serve(t, s.observeRequests(handler), http.MethodPost, "/api/v1/sessions", "writer-secret"); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	lines := auditLines(t, &buf)
	if len(lines) != 1 || lines[0]["target"] != "sess-new" {
		t.Fatalf("audit lines = %+v", lines)
	}
}

func TestRequestIDHandling(t *testing.T) {
	s := scopeServer(t)
	handler := s.observeRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", requestID(r))
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", "caller-7")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); got != "caller-7" {
		t.Fatalf("request id = %q, want caller-7", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", "bad id/with space")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	got := rec.Header().Get("X-Request-Id")
	if got == "bad id/with space" || len(got) != 16 {
		t.Fatalf("invalid request id was not replaced: %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); len(got) != 16 {
		t.Fatalf("generated request id = %q", got)
	}
}

func TestHandlerScopeEnforcement(t *testing.T) {
	var buf bytes.Buffer
	// audit capture is separate from the process logger after #46.
	logger := testLogger()
	cfg := config.Default()
	cfg.Server.Tokens = []config.APITokenConfig{{Name: "reader", Scopes: []string{config.ScopeSessionsRead, config.ScopeSchedulesRead}, Secret: "reader-secret"}}
	handler := New(cfg, nil, nil, logger, WithAuditLogger(NewAuditLogger(&buf))).Handler()

	if rec := serve(t, handler, http.MethodGet, "/api/v1/schedules", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", rec.Code)
	}
	buf.Reset()
	rec := serve(t, handler, http.MethodGet, "/api/v1/schedules", "reader-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("read with sessions:read status = %d body=%s", rec.Code, rec.Body.String())
	}
	if id := rec.Header().Get("X-Request-Id"); id == "" {
		t.Fatal("response has no request id")
	}
	rec = serve(t, handler, http.MethodDelete, "/api/v1/sessions/sess-1", "reader-secret")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("write with sessions:read status = %d body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeEnvelope(t, rec); body.Code != codeForbidden {
		t.Fatalf("forbidden code = %q", body.Code)
	}
	lines := auditLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1: %s", len(lines), buf.String())
	}
	if lines[0]["token_name"] != "reader" || lines[0]["route"] != "/api/v1/sessions/{id}" || lines[0]["target"] != "sess-1" {
		t.Fatalf("audit line = %+v", lines[0])
	}
	if lines[0]["request_id"] == "" {
		t.Fatalf("audit line has no request id: %+v", lines[0])
	}
}

// TestAuditLineIndependentOfProcessLogLevel is the #46 acceptance test. The
// process logger is at warn or error, as MAYFLY_LOG_LEVEL=warn or error sets
// it, and a mutating request still writes exactly one audit line.
func TestAuditLineIndependentOfProcessLogLevel(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelWarn, slog.LevelError} {
		t.Run(level.String(), func(t *testing.T) {
			var processBuf, auditBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&processBuf, &slog.HandlerOptions{Level: level}))
			const secret = "ci-secret-value-for-audit"
			cfg := config.Default()
			cfg.Server.Tokens = []config.APITokenConfig{{Name: "ci", Scopes: []string{config.ScopeSchedulesWrite}, Secret: secret}}
			s := New(cfg, nil, nil, logger, WithAuditLogger(NewAuditLogger(&auditBuf)))
			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/v1/schedules/{name}/run", s.authorizedRoute(apiRoute{
				pattern: "POST /api/v1/schedules/{name}/run",
				scope:   config.ScopeSchedulesWrite,
				handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) },
			}))
			rec := serve(t, s.observeRequests(mux), http.MethodPost, "/api/v1/schedules/nightly/run", secret)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			lines := auditLines(t, &auditBuf)
			if len(lines) != 1 {
				t.Fatalf("audit lines = %d, want 1 at process level %s: %s", len(lines), level, auditBuf.String())
			}
			if strings.Contains(auditBuf.String(), secret) {
				t.Fatalf("audit line contains the token secret: %s", auditBuf.String())
			}
			if strings.Contains(processBuf.String(), `"msg":"audit"`) {
				t.Fatalf("audit line was written through the process logger: %s", processBuf.String())
			}
		})
	}
}

// TestFailedBearerAuthAuditsAndCounts is the #47 acceptance test for a 401 on
// a scoped route: one audit line and one counter increment, never the secret.
func TestFailedBearerAuthAuditsAndCounts(t *testing.T) {
	var buf bytes.Buffer
	logger := testLogger()
	const secret = "reader-secret-value"
	cfg := config.Default()
	cfg.Server.Tokens = []config.APITokenConfig{{Name: "reader", Scopes: []string{config.ScopeQueueRead}, Secret: secret}}
	s := New(cfg, nil, nil, logger, WithAuditLogger(NewAuditLogger(&buf)))
	s.metrics = metrics.New(metrics.Sources{}, metrics.BuildInfo{})
	handler := s.authorizedRoute(apiRoute{
		pattern: "GET /api/v1/queue",
		scope:   config.ScopeQueueRead,
		handler: func(http.ResponseWriter, *http.Request) { t.Fatal("handler ran without a valid token") },
	})

	rec := serve(t, handler, http.MethodGet, "/api/v1/queue", "wrong-secret")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	lines := auditLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1: %s", len(lines), buf.String())
	}
	line := lines[0]
	if line["token_name"] != "unknown" || line["via"] != authViaBearer || line["status"] != float64(http.StatusUnauthorized) {
		t.Fatalf("audit line = %+v", line)
	}
	if strings.Contains(buf.String(), "wrong-secret") {
		t.Fatalf("audit line contains the presented secret: %s", buf.String())
	}
	if got := counterValue(t, s.metrics, `mayfly_auth_failures_total{via="bearer"}`); got != 1 {
		t.Fatalf("mayfly_auth_failures_total{bearer} = %v, want 1", got)
	}
}

func TestMissingAndExpiredBearerAuthAudited(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	cases := []struct {
		name      string
		presented string
		expires   *time.Time
	}{
		{name: "missing"},
		{name: "wrong", presented: "wrong-secret"},
		{name: "expired", presented: "reader-secret-value", expires: &past},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			cfg := config.Default()
			cfg.Server.Tokens = []config.APITokenConfig{{
				Name:      "reader",
				Scopes:    []string{config.ScopeQueueRead},
				Secret:    "reader-secret-value",
				ExpiresAt: tc.expires,
			}}
			s := New(cfg, nil, nil, testLogger(), WithAuditLogger(NewAuditLogger(&buf)))
			s.metrics = metrics.New(metrics.Sources{}, metrics.BuildInfo{})
			rec := serve(t, authTestHandler(s), http.MethodGet, "/api/v1/queue", tc.presented)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			lines := auditLines(t, &buf)
			if len(lines) != 1 {
				t.Fatalf("audit lines = %d, want 1: %s", len(lines), buf.String())
			}
			if lines[0]["token_name"] != "unknown" || lines[0]["via"] != authViaBearer {
				t.Fatalf("audit line = %+v", lines[0])
			}
			if tc.presented != "" && strings.Contains(buf.String(), tc.presented) {
				t.Fatalf("audit line contains the presented secret: %s", buf.String())
			}
			if got := counterValue(t, s.metrics, `mayfly_auth_failures_total{via="bearer"}`); got != 1 {
				t.Fatalf("failures = %v, want 1", got)
			}
		})
	}
}

func TestInvalidWebSessionOnScopedRouteAudited(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Default()
	cfg.Server.Tokens = []config.APITokenConfig{{Name: "reader", Scopes: []string{config.ScopeQueueRead}, Secret: "reader-secret-value"}}
	s := New(cfg, nil, nil, testLogger(), WithAuditLogger(NewAuditLogger(&buf)))
	s.metrics = metrics.New(metrics.Sources{}, metrics.BuildInfo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/queue", nil)
	req.AddCookie(&http.Cookie{Name: webSessionCookie, Value: "not-a-session"})
	rec := httptest.NewRecorder()
	authTestHandler(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	lines := auditLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1: %s", len(lines), buf.String())
	}
	if lines[0]["token_name"] != "unknown" || lines[0]["via"] != authViaWeb {
		t.Fatalf("audit line = %+v", lines[0])
	}
	if got := counterValue(t, s.metrics, `mayfly_auth_failures_total{via="web"}`); got != 1 {
		t.Fatalf("failures = %v, want 1", got)
	}
}

func TestFailedAuthLimiterBlocksAfterBurst(t *testing.T) {
	s := scopeServer(t, config.APITokenConfig{Name: "reader", Scopes: []string{config.ScopeQueueRead}, Secret: "reader-secret-value"})
	handler := authTestHandler(s)
	for i := range 20 {
		rec := requestFrom(handler, "192.0.2.10:4000", "wrong-secret")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, rec.Code)
		}
	}
	rec := requestFrom(handler, "192.0.2.10:4000", "wrong-secret")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("failure 21 = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 has no Retry-After header")
	}
	if body := decodeEnvelope(t, rec); body.Code != codeRateLimited {
		t.Fatalf("429 code = %q", body.Code)
	}
}

func TestSuccessfulAuthNeverLimited(t *testing.T) {
	s := scopeServer(t, config.APITokenConfig{Name: "reader", Scopes: []string{config.ScopeQueueRead}, Secret: "reader-secret-value"})
	handler := authTestHandler(s)
	for range 25 {
		requestFrom(handler, "192.0.2.11:4000", "wrong-secret")
	}
	if rec := requestFrom(handler, "192.0.2.11:4000", "reader-secret-value"); rec.Code != http.StatusNoContent {
		t.Fatalf("valid token after failures = %d, want 204", rec.Code)
	}
}

func TestFailedAuthLimiterSeparatesIPs(t *testing.T) {
	s := scopeServer(t, config.APITokenConfig{Name: "reader", Scopes: []string{config.ScopeQueueRead}, Secret: "reader-secret-value"})
	handler := authTestHandler(s)
	for range 21 {
		requestFrom(handler, "192.0.2.12:4000", "wrong-secret")
	}
	if rec := requestFrom(handler, "192.0.2.12:4000", "wrong-secret"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted IP = %d, want 429", rec.Code)
	}
	if rec := requestFrom(handler, "192.0.2.13:4000", "wrong-secret"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("other IP = %d, want 401", rec.Code)
	}
	if rec := requestFrom(handler, "192.0.2.13:4000", "reader-secret-value"); rec.Code != http.StatusNoContent {
		t.Fatalf("other IP valid token = %d, want 204", rec.Code)
	}
}

func TestForwardedForIgnoredByDefault(t *testing.T) {
	s := scopeServer(t, config.APITokenConfig{Name: "reader", Scopes: []string{config.ScopeQueueRead}, Secret: "reader-secret-value"})
	handler := authTestHandler(s)
	// Each request carries a different forwarded address. With trust off the
	// limiter keys on RemoteAddr, so the 21st request is over budget.
	for i := range 20 {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/queue", nil)
		req.RemoteAddr = "192.0.2.14:4000"
		req.Header.Set("Authorization", "Bearer wrong-secret")
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i+1))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/queue", nil)
	req.RemoteAddr = "192.0.2.14:4000"
	req.Header.Set("Authorization", "Bearer wrong-secret")
	req.Header.Set("X-Forwarded-For", "203.0.113.200")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("X-Forwarded-For changed the limiter key while trust is off: %d", rec.Code)
	}
}

func TestForwardedForHonoredWhenTrusted(t *testing.T) {
	cfg := config.Default()
	cfg.Server.TrustForwardedFor = true
	cfg.Server.Tokens = []config.APITokenConfig{{Name: "reader", Scopes: []string{config.ScopeQueueRead}, Secret: "reader-secret-value"}}
	s := New(cfg, nil, nil, testLogger())
	handler := authTestHandler(s)

	request := func(remote, forwarded, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/queue", nil)
		req.RemoteAddr = remote
		req.Header.Set("X-Forwarded-For", forwarded)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	for range 20 {
		if rec := request("192.0.2.20:4000", "203.0.113.5", "wrong-secret"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure = %d, want 401", rec.Code)
		}
	}
	if rec := request("192.0.2.20:4000", "203.0.113.5", "wrong-secret"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("same forwarded key = %d, want 429", rec.Code)
	}
	if rec := request("192.0.2.20:4000", "203.0.113.6", "wrong-secret"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("different forwarded key = %d, want 401", rec.Code)
	}
	if rec := request("192.0.2.21:4000", "203.0.113.5", "wrong-secret"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("same forwarded key from another peer = %d, want 429", rec.Code)
	}
}

func TestAuthFailureLimiterMapBounded(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	limiter := newAuthFailureLimiter()
	limiter.now = func() time.Time { return now }
	limiter.max = 4
	for i := range 100 {
		limiter.fail(fmt.Sprintf("192.0.2.%d", i))
	}
	if got := len(limiter.buckets); got > limiter.max {
		t.Fatalf("buckets = %d, want <= %d", got, limiter.max)
	}
	now = now.Add(limiter.idle + time.Second)
	limiter.fail("198.51.100.1")
	if got := len(limiter.buckets); got != 1 {
		t.Fatalf("idle buckets were not evicted: %d left", got)
	}
}

func TestFailedWebSignInLimitedAndCounted(t *testing.T) {
	s, handler := webTestServer(t, "http://localhost:8080")
	s.metrics = metrics.New(metrics.Sources{}, metrics.BuildInfo{})
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, webSessionRoute, strings.NewReader(`{"token":"wrong-secret"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	for range 20 {
		if rec := post(); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure = %d, want 401", rec.Code)
		}
	}
	rec := post()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("failure 21 = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 has no Retry-After header")
	}
	if got := counterValue(t, s.metrics, `mayfly_auth_failures_total{via="web"}`); got != 21 {
		t.Fatalf("mayfly_auth_failures_total{web} = %v, want 21", got)
	}
}

func authTestHandler(s *Server) http.Handler {
	return s.authorizedRoute(apiRoute{
		pattern: "GET /api/v1/queue",
		scope:   config.ScopeQueueRead,
		handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
	})
}

func requestFrom(handler http.Handler, remote, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/queue", nil)
	req.RemoteAddr = remote
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// counterValue reads one series from the registry in Prometheus text form.
func counterValue(t *testing.T, m *metrics.Metrics, series string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, series+" ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("metric line = %q", line)
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			t.Fatalf("metric value %q: %v", fields[1], err)
		}
		return value
	}
	t.Fatalf("series %s not found in registry", series)
	return 0
}

// auditLines decodes the JSON log lines captured during one test.
func auditLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			t.Fatalf("decode log line %q: %v", raw, err)
		}
		if event["msg"] != "audit" {
			// The request log shares this buffer at info level.
			continue
		}
		out = append(out, event)
	}
	return out
}
