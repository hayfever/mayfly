package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/metrics"
)

func metricsServer(t *testing.T, cfg *config.Config, logger *slog.Logger) (*Server, *metrics.Metrics) {
	t.Helper()
	registry := metrics.New(metrics.Sources{}, metrics.BuildInfo{Version: "test"})
	return New(cfg, nil, nil, logger, WithMetrics(registry)), registry
}

func scrape(t *testing.T, handler http.Handler, token string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestRequestIDIsEchoedOrGenerated(t *testing.T) {
	s, _ := metricsServer(t, config.Default(), testLogger())

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	generated := rec.Header().Get("X-Request-ID")
	if len(generated) != 32 {
		t.Fatalf("generated request id = %q", generated)
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "trace-42")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-ID"); got != "trace-42" {
		t.Fatalf("echoed request id = %q", got)
	}

	// An id that could inject a line break is replaced.
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "bad id")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-ID"); got == "bad id" || len(got) != 32 {
		t.Fatalf("invalid request id was kept: %q", got)
	}
}

func TestErrorEnvelopeCarriesRequestID(t *testing.T) {
	s, _ := metricsServer(t, config.Default(), testLogger())
	req := httptest.NewRequest(http.MethodPost, "/hooks/missing", strings.NewReader(`{}`))
	req.Header.Set("X-Request-ID", "trace-42")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var envelope errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v body=%s", err, rec.Body.String())
	}
	if envelope.Error.Code != codeNotFound {
		t.Fatalf("code = %q", envelope.Error.Code)
	}
	if got := envelope.Error.Details["request_id"]; got != "trace-42" {
		t.Fatalf("details.request_id = %v", got)
	}
}

func TestRequestsAreCountedByRouteTemplate(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Metrics.RequireAuth = new(false)
	s, _ := metricsServer(t, cfg, testLogger())

	s.Handler().ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/hooks/abc123", strings.NewReader(`{}`)))
	s.Handler().ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/no-such-endpoint", nil))

	body := scrape(t, s.Handler(), "")
	if !strings.Contains(body, `mayfly_http_requests_total{code="404",method="POST",route="/hooks/{name}"} 1`) {
		t.Fatalf("route template counter is missing:\n%s", body)
	}
	if !strings.Contains(body, `mayfly_http_requests_total{code="404",method="GET",route="unmatched"} 1`) {
		t.Fatalf("unmatched route counter is missing:\n%s", body)
	}
	if !strings.Contains(body, `mayfly_http_request_duration_seconds_count{method="POST",route="/hooks/{name}"} 1`) {
		t.Fatalf("route template histogram is missing:\n%s", body)
	}
	if strings.Contains(body, "abc123") {
		t.Fatal("the raw path reached a metric label")
	}
}

func TestMetricsEndpointRequiresTheAdminTokenByDefault(t *testing.T) {
	cfg := config.Default()
	cfg.AdminToken = "admin-token"
	s, _ := metricsServer(t, cfg, testLogger())

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", rec.Code)
	}
	body := scrape(t, s.Handler(), "admin-token")
	if !strings.Contains(body, "mayfly_build_info") {
		t.Fatalf("metrics body has no build info:\n%s", body)
	}
}

func TestSeparateMetricsAddressMovesTheEndpoint(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Metrics.Address = ":9090"
	s, _ := metricsServer(t, cfg, testLogger())

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("main listener status = %d, want 404", rec.Code)
	}
	body := scrape(t, s.MetricsHandler(), "")
	if !strings.Contains(body, "mayfly_build_info") {
		t.Fatalf("separate handler body has no build info:\n%s", body)
	}
}

func TestRequestLogCarriesTheRouteAndStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	s, _ := metricsServer(t, config.Default(), logger)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "trace-7")
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)

	line := buf.String()
	for _, field := range []string{
		"msg=\"http request\"",
		"request_id=trace-7",
		"method=GET",
		"route=/healthz",
		"status=200",
		"bytes=",
		"duration_ms=",
	} {
		if !strings.Contains(line, field) {
			t.Fatalf("log line is missing %q:\n%s", field, line)
		}
	}
}
