package broker

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hayfever/mayfly/internal/config"
	"github.com/hayfever/mayfly/internal/stub"
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
	b.ServeLLM(rec, req, "stub")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Completed.") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}

	// "extra" is allowlisted but not assigned to this session.
	other := `{"model":"extra","messages":[]}`
	rec = httptest.NewRecorder()
	b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(other)), "stub")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unassigned model status = %d", rec.Code)
	}
}

func TestLLMFillsMissingModelFromSession(t *testing.T) {
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()
	cfg := config.LLMConfig{BaseURL: upstream.URL + "/v1", Model: "stub", MaxBodyBytes: 1 << 20}
	b := New(cfg, testLogger())
	body := `{"messages":[{"role":"user","content":"hello"}]}`
	rec := httptest.NewRecorder()
	b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body)), "stub")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != "stub" {
		t.Fatalf("upstream model = %q, want the session model", gotModel)
	}
}

func TestLLMStreamingPassthrough(t *testing.T) {
	upstream := httptest.NewServer(stub.LLMHandler())
	defer upstream.Close()
	cfg := config.LLMConfig{BaseURL: upstream.URL + "/v1", Model: "stub", MaxBodyBytes: 1 << 20}
	b := New(cfg, testLogger())
	body := `{"model":"stub","stream":true,"messages":[{"role":"user","content":"stream"}]}`
	rec := httptest.NewRecorder()
	b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body)), "stub")
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

func TestMCPProxyRejectsOversizedBody(t *testing.T) {
	const limit = 1024
	type upstreamCall struct {
		body []byte
	}
	calls := make(chan upstreamCall, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, limit+1))
		calls <- upstreamCall{body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer upstream.Close()

	def := config.MCPServerConfig{Name: "remote", Transport: "http", URL: upstream.URL + "/mcp"}
	b := New(config.LLMConfig{MaxBodyBytes: limit}, testLogger())
	oversized := strings.Repeat("a", 2*limit)

	t.Run("declared length over cap", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/mcp/remote", strings.NewReader(oversized))
		if req.ContentLength != int64(len(oversized)) {
			t.Fatalf("test setup: content length = %d", req.ContentLength)
		}
		rec := httptest.NewRecorder()
		b.ServeMCP(rec, req, def)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413 body=%s", rec.Code, rec.Body.String())
		}
		select {
		case call := <-calls:
			t.Fatalf("upstream was called with %d bytes, want no call", len(call.body))
		default:
		}
	})

	t.Run("chunked body over cap", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/mcp/remote", nil)
		req.Body = io.NopCloser(strings.NewReader(oversized))
		req.ContentLength = -1
		rec := httptest.NewRecorder()
		b.ServeMCP(rec, req, def)
		if rec.Code < 400 {
			t.Fatalf("status = %d, want an error status", rec.Code)
		}
		select {
		case call := <-calls:
			if len(call.body) > limit {
				t.Fatalf("upstream received %d bytes, want at most %d", len(call.body), limit)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("upstream call did not finish")
		}
	})

	t.Run("under cap is forwarded", func(t *testing.T) {
		payload := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/mcp/remote", strings.NewReader(payload))
		rec := httptest.NewRecorder()
		b.ServeMCP(rec, req, def)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		select {
		case call := <-calls:
			if string(call.body) != payload {
				t.Fatalf("upstream body = %q, want %q", call.body, payload)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("upstream call did not finish")
		}
	})
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
