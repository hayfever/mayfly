package broker

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hayden/mayfly/internal/config"
	"github.com/hayden/mayfly/internal/stub"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLLMProxyAndModelPolicy(t *testing.T) {
	upstream := httptest.NewServer(stub.LLMHandler())
	defer upstream.Close()
	cfg := config.LLMConfig{
		BaseURL:      upstream.URL + "/v1",
		Model:        "stub",
		AllowModels:  []string{"stub"},
		MaxBodyBytes: 1 << 20,
	}
	b := New(cfg, testLogger())

	body := `{"model":"stub","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	b.ServeLLM(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Completed.") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}

	bad := `{"model":"expensive","messages":[]}`
	rec = httptest.NewRecorder()
	b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(bad)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad model status = %d", rec.Code)
	}
}

func TestLLMStreamingPassthrough(t *testing.T) {
	upstream := httptest.NewServer(stub.LLMHandler())
	defer upstream.Close()
	cfg := config.LLMConfig{BaseURL: upstream.URL + "/v1", Model: "stub", MaxBodyBytes: 1 << 20}
	b := New(cfg, testLogger())
	body := `{"model":"stub","stream":true,"messages":[{"role":"user","content":"stream"}]}`
	rec := httptest.NewRecorder()
	b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("stream not passed through: %s", rec.Body.String())
	}
}

func TestMCPProxyInjectsCredentials(t *testing.T) {
	var upstreamAuth, upstreamSession string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		upstreamSession = r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer upstream.Close()

	def := config.MCPServerConfig{
		Name:            "remote",
		Transport:       "http",
		URL:             upstream.URL + "/mcp",
		ResolvedHeaders: map[string]string{"Authorization": "Bearer upstream-secret"},
	}
	b := New(config.LLMConfig{MaxBodyBytes: 1 << 20}, testLogger())
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/mcp/remote", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("Authorization", "Bearer session-token")
	req.Header.Set("Mcp-Session-Id", "abc")
	rec := httptest.NewRecorder()
	b.ServeMCP(rec, req, def)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if upstreamAuth != "Bearer upstream-secret" {
		t.Fatalf("upstream auth = %q", upstreamAuth)
	}
	if strings.Contains(rec.Body.String(), "session-token") {
		t.Fatal("session token leaked upstream response")
	}
	if upstreamSession != "abc" {
		t.Fatalf("session header = %q", upstreamSession)
	}
}
