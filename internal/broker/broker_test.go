package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	b.ServeLLM(rec, req, "stub", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Completed.") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}

	// "extra" is allowlisted but not assigned to this session.
	other := `{"model":"extra","messages":[]}`
	rec = httptest.NewRecorder()
	b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(other)), "stub", nil)
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
	b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body)), "stub", nil)
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
	b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body)), "stub", nil)
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

// TestBrokerProxyUnchanged keeps the alternate-harness path intact: the byte
// proxy still injects resolved headers, and HTTPClient exposes the one client
// that carries the broker's transport policy.
func TestBrokerProxyUnchanged(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":7,"result":{"tools":[]}}`))
	}))
	defer upstream.Close()

	b := New(config.LLMConfig{MaxBodyBytes: 1 << 20}, testLogger())
	if b.HTTPClient() == nil {
		t.Fatal("HTTPClient returned nil")
	}
	if b.HTTPClient() != b.client {
		t.Fatal("HTTPClient did not return the broker's client")
	}
	if b.HTTPClient().Transport == nil {
		t.Fatal("the broker's client has no transport")
	}

	def := config.MCPServerConfig{
		Name:            "remote",
		Transport:       "http",
		URL:             upstream.URL + "/mcp",
		ResolvedHeaders: map[string]string{"Authorization": "Bearer upstream-secret"},
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/mcp/remote", strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`))
	rec := httptest.NewRecorder()
	b.ServeMCP(rec, req, def)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer upstream-secret" {
		t.Fatalf("upstream auth = %q", gotAuth)
	}
}

func TestLLMNonStreamUsage(t *testing.T) {
	const payload = `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":12,"completion_tokens":34}}`
	var forwarded []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	}))
	defer upstream.Close()
	b := New(config.LLMConfig{BaseURL: upstream.URL + "/v1", Model: "stub", MaxBodyBytes: 1 << 20}, testLogger())

	body := `{"model":"stub","messages":[{"role":"user","content":"hello"}]}`
	rec := httptest.NewRecorder()
	res := b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body)), "stub", nil)
	if rec.Code != http.StatusOK || res.Status != http.StatusOK {
		t.Fatalf("status = %d result=%d body=%s", rec.Code, res.Status, rec.Body.String())
	}
	if !res.Forwarded || res.Streamed {
		t.Fatalf("forwarded=%v streamed=%v", res.Forwarded, res.Streamed)
	}
	if !res.Reported || res.Usage != (Usage{Prompt: 12, Completion: 34}) {
		t.Fatalf("usage = %+v reported=%v", res.Usage, res.Reported)
	}
	if res.ReqBytes != int64(len(body)) {
		t.Fatalf("req bytes = %d, want %d", res.ReqBytes, len(body))
	}
	if res.RespBytes != int64(len(payload)) {
		t.Fatalf("resp bytes = %d, want %d", res.RespBytes, len(payload))
	}
	if rec.Body.String() != payload {
		t.Fatalf("passthrough body changed:\n got %q\nwant %q", rec.Body.String(), payload)
	}
	// A non-streamed request must reach the provider byte-identical, without
	// stream_options.
	if string(forwarded) != body {
		t.Fatalf("forwarded body = %q, want %q", forwarded, body)
	}
}

func TestLLMStreamedUsageInjectsIncludeUsage(t *testing.T) {
	const payload = "data: {\"choices\":[{\"delta\":{\"content\":\"h\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"
	var forwarded map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &forwarded)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for start := 0; start < len(payload); start += 7 {
			end := min(start+7, len(payload))
			_, _ = io.WriteString(w, payload[start:end])
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()
	b := New(config.LLMConfig{BaseURL: upstream.URL + "/v1", Model: "stub", MaxBodyBytes: 1 << 20}, testLogger())

	body := `{"model":"stub","stream":true,"stream_options":{"service_tier":"auto"},"messages":[]}`
	rec := httptest.NewRecorder()
	res := b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body)), "stub", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !res.Streamed || !res.Reported || res.Usage != (Usage{Prompt: 7, Completion: 9}) {
		t.Fatalf("streamed=%v usage=%+v reported=%v", res.Streamed, res.Usage, res.Reported)
	}
	if rec.Body.String() != payload {
		t.Fatalf("passthrough body changed:\n got %q\nwant %q", rec.Body.String(), payload)
	}

	var options map[string]any
	if err := json.Unmarshal(forwarded["stream_options"], &options); err != nil {
		t.Fatalf("upstream stream_options: %v", err)
	}
	if options["include_usage"] != true {
		t.Fatalf("include_usage = %v, want true", options["include_usage"])
	}
	if options["service_tier"] != "auto" {
		t.Fatalf("client stream_options key dropped: %v", options)
	}
	var model string
	if err := json.Unmarshal(forwarded["model"], &model); err != nil || model != "stub" {
		t.Fatalf("upstream model = %q err=%v", model, err)
	}
	if _, ok := forwarded["messages"]; !ok {
		t.Fatal("client key messages was dropped")
	}
}

func TestLLMUsageMissingIsUnreported(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		status      int
		payload     string
		stream      bool
	}{
		{"json without usage", "application/json", http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`, false},
		{"stream without usage", "text/event-stream", http.StatusOK, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n", true},
		{"error without usage", "application/json", http.StatusInternalServerError, `{"error":"upstream broke"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.payload)
			}))
			defer upstream.Close()
			b := New(config.LLMConfig{BaseURL: upstream.URL + "/v1", Model: "stub", MaxBodyBytes: 1 << 20}, testLogger())

			body := `{"model":"stub","messages":[]}`
			if tc.stream {
				body = `{"model":"stub","stream":true,"messages":[]}`
			}
			rec := httptest.NewRecorder()
			res := b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body)), "stub", nil)
			if rec.Code != tc.status || res.Status != tc.status {
				t.Fatalf("status = %d result=%d", rec.Code, res.Status)
			}
			if !res.Forwarded {
				t.Fatal("forwarded = false")
			}
			if res.Reported || res.Usage != (Usage{}) {
				t.Fatalf("usage = %+v reported = %v, want unreported", res.Usage, res.Reported)
			}
			if res.RespBytes != int64(len(tc.payload)) {
				t.Fatalf("resp bytes = %d, want %d", res.RespBytes, len(tc.payload))
			}
		})
	}
}

func TestLLMAdmitRefusal(t *testing.T) {
	var hit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	b := New(config.LLMConfig{BaseURL: upstream.URL + "/v1", Model: "stub", MaxBodyBytes: 1 << 20}, testLogger())

	body := `{"model":"stub","max_completion_tokens":128,"messages":[]}`
	var gotReq, gotMax int64
	rec := httptest.NewRecorder()
	res := b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body)), "stub",
		func(reqBytes, maxOut int64) error {
			gotReq, gotMax = reqBytes, maxOut
			return &Refusal{Status: http.StatusTooManyRequests, Code: "llm_budget_exhausted", Message: "token budget exhausted"}
		})
	if hit.Load() {
		t.Fatal("upstream was called after admit refused")
	}
	if res.Forwarded {
		t.Fatal("forwarded = true, want false")
	}
	if res.Status != http.StatusTooManyRequests || rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d result=%d, want 429", rec.Code, res.Status)
	}
	if gotReq != int64(len(body)) || gotMax != 128 {
		t.Fatalf("admit args = (%d, %d), want (%d, 128)", gotReq, gotMax, len(body))
	}
	var written map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &written); err != nil {
		t.Fatalf("refusal body: %v", err)
	}
	if written["code"] != "llm_budget_exhausted" || written["error"] != "token budget exhausted" {
		t.Fatalf("refusal body = %v", written)
	}
	if res.RespBytes != int64(rec.Body.Len()) {
		t.Fatalf("resp bytes = %d, want %d", res.RespBytes, rec.Body.Len())
	}

	t.Run("busy refusal sets Retry-After", func(t *testing.T) {
		rec := httptest.NewRecorder()
		res := b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body)), "stub",
			func(int64, int64) error {
				return &Refusal{Status: http.StatusTooManyRequests, Code: "llm_busy", Message: "busy", RetryAfter: 1500 * time.Millisecond}
			})
		if res.Forwarded {
			t.Fatal("forwarded = true, want false")
		}
		if got := rec.Header().Get("Retry-After"); got != "2" {
			t.Fatalf("Retry-After = %q, want 2", got)
		}
	})

	t.Run("exhausted refusal sets no Retry-After", func(t *testing.T) {
		rec := httptest.NewRecorder()
		b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body)), "stub",
			func(int64, int64) error {
				return &Refusal{Status: http.StatusTooManyRequests, Code: "llm_budget_exhausted", Message: "spent"}
			})
		if got := rec.Header().Get("Retry-After"); got != "" {
			t.Fatalf("Retry-After = %q, want none", got)
		}
	})

	t.Run("plain error becomes 500", func(t *testing.T) {
		rec := httptest.NewRecorder()
		res := b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body)), "stub",
			func(int64, int64) error { return errors.New("ledger unavailable") })
		if hit.Load() || res.Forwarded {
			t.Fatal("upstream was called after admit failed")
		}
		if res.Status != http.StatusInternalServerError || rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if strings.Contains(rec.Body.String(), `"code"`) {
			t.Fatalf("plain error carried a code: %s", rec.Body.String())
		}
	})
}

func TestLLMAdmitOutputLimit(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{"default", `{"model":"stub","messages":[]}`, defaultCompletionCap},
		{"max_tokens", `{"model":"stub","max_tokens":64,"messages":[]}`, 64},
		{"max_completion_tokens wins", `{"model":"stub","max_tokens":64,"max_completion_tokens":128,"messages":[]}`, 128},
		{"zero falls back", `{"model":"stub","max_tokens":0,"messages":[]}`, defaultCompletionCap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := New(config.LLMConfig{BaseURL: "http://provider.invalid/v1", Model: "stub", MaxBodyBytes: 1 << 20}, testLogger())
			var got int64 = -1
			rec := httptest.NewRecorder()
			b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(tc.body)), "stub",
				func(_ int64, maxOut int64) error {
					got = maxOut
					return &Refusal{Status: http.StatusTooManyRequests, Code: "llm_busy", Message: "busy"}
				})
			if got != tc.want {
				t.Fatalf("maxOut = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLLMTransportFailure(t *testing.T) {
	// Nothing listens on the closed server's address, so the dial fails after
	// the admit hook reserved the call. That is the same result as the
	// response-header timeout: Forwarded=true and no provider status.
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := upstream.URL
	upstream.Close()
	b := New(config.LLMConfig{BaseURL: baseURL + "/v1", Model: "stub", MaxBodyBytes: 1 << 20}, testLogger())

	body := `{"model":"stub","max_completion_tokens":256,"messages":[]}`
	var gotMax int64
	rec := httptest.NewRecorder()
	res := b.ServeLLM(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/llm/chat/completions", strings.NewReader(body)), "stub",
		func(_ int64, maxOut int64) error {
			gotMax = maxOut
			return nil
		})
	if !res.Forwarded {
		t.Fatal("forwarded = false, want true after the admit hook reserved the call")
	}
	if !res.TransportErr {
		t.Fatal("transport error not reported")
	}
	if res.Status != http.StatusBadGateway || rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d result=%d, want 502", rec.Code, res.Status)
	}
	if res.MaxOut != 256 || gotMax != 256 {
		t.Fatalf("reserved cap = %d admit=%d, want 256", res.MaxOut, gotMax)
	}
	if res.ReqBytes != int64(len(body)) {
		t.Fatalf("request bytes = %d, want %d", res.ReqBytes, len(body))
	}
	if res.Reported {
		t.Fatal("a failed transport reported usage")
	}
}

func TestUsageScannerSplitsIdentically(t *testing.T) {
	stream := []byte("event: message\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data:{\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":6}}\r\n\r\n" +
		"data: {\"usage\":null}\n\n" +
		": note \"usage\"\n" +
		"data: [DONE]\n\n")
	want := Usage{Prompt: 5, Completion: 6}

	parse := func(chunks ...[]byte) *usageScanner {
		s := &usageScanner{}
		for _, chunk := range chunks {
			if n, err := s.Write(chunk); n != len(chunk) || err != nil {
				t.Fatalf("Write returned (%d, %v), want (%d, nil)", n, err, len(chunk))
			}
			if len(s.line) > maxUsageLineBytes {
				t.Fatalf("line buffer grew to %d bytes", len(s.line))
			}
		}
		if s.total != int64(len(stream)) {
			t.Fatalf("total = %d, want %d", s.total, len(stream))
		}
		if got, ok := s.Result(); !ok || got != want {
			t.Fatalf("result = %+v ok=%v, want %+v", got, ok, want)
		}
		return s
	}

	if s := parse(stream); len(s.line) != 0 {
		t.Fatalf("complete stream left %d bytes in the line buffer", len(s.line))
	}
	for cut := 0; cut <= len(stream); cut++ {
		parse(stream[:cut], stream[cut:])
	}
	for cut := 0; cut < len(stream); cut += 3 {
		parse(stream[:cut], stream[cut:cut+1], stream[cut+1:])
	}
}

func TestUsageScannerDropsOverlongLine(t *testing.T) {
	overlong := append(bytes.Repeat([]byte("a"), maxUsageLineBytes+1), '\n')
	tail := []byte("data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n")
	s := &usageScanner{}
	_, _ = s.Write(overlong)
	if len(s.line) != 0 || s.skip {
		t.Fatalf("overlong line kept %d bytes, skip=%v", len(s.line), s.skip)
	}
	_, _ = s.Write(tail)
	if got, ok := s.Result(); !ok || got != (Usage{Prompt: 2, Completion: 3}) {
		t.Fatalf("result = %+v ok=%v", got, ok)
	}
	if s.total != int64(len(overlong)+len(tail)) {
		t.Fatalf("total = %d, want %d", s.total, len(overlong)+len(tail))
	}

	// A trailing over-long line with no newline stays skipped and never grows.
	s = &usageScanner{}
	_, _ = s.Write(overlong[:len(overlong)-1])
	if len(s.line) != 0 || !s.skip {
		t.Fatalf("unterminated overlong line kept %d bytes, skip=%v", len(s.line), s.skip)
	}
}

func TestBodyCaptureBounds(t *testing.T) {
	c := &bodyCapture{limit: 8}
	if _, _ = c.Write([]byte("hello")); c.overflow || string(c.buf) != "hello" || c.total != 5 {
		t.Fatalf("first write: buf=%q overflow=%v total=%d", c.buf, c.overflow, c.total)
	}
	if _, _ = c.Write([]byte("world")); !c.overflow || c.buf != nil || c.total != 10 {
		t.Fatalf("overflow write: buf=%q overflow=%v total=%d", c.buf, c.overflow, c.total)
	}
	if _, _ = c.Write([]byte("more")); c.total != 14 {
		t.Fatalf("total after overflow = %d, want 14", c.total)
	}
}

func TestTokenCountClamps(t *testing.T) {
	cases := map[json.Number]int64{
		"":                     0,
		"0":                    0,
		"42":                   42,
		"-7":                   0,
		"1.5":                  1,
		"1e3":                  1000,
		"1e400":                math.MaxInt64,
		"9223372036854775808":  math.MaxInt64,
		"99999999999999999999": math.MaxInt64,
	}
	for input, want := range cases {
		if got := tokenCount(input); got != want {
			t.Fatalf("tokenCount(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestInjectStreamOptions(t *testing.T) {
	streamBody := []byte(`{"model":"stub","stream":true,"stream_options":{"service_tier":"auto"},"messages":[]}`)
	out, err := injectStreamOptions(streamBody, true)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	var options map[string]any
	if err := json.Unmarshal(doc["stream_options"], &options); err != nil {
		t.Fatalf("stream_options: %v", err)
	}
	if options["include_usage"] != true || options["service_tier"] != "auto" {
		t.Fatalf("stream_options = %v", options)
	}
	var model string
	if err := json.Unmarshal(doc["model"], &model); err != nil || model != "stub" {
		t.Fatalf("model = %q err=%v", model, err)
	}

	plain := []byte(`{"model":"stub","messages":[]}`)
	if out, err := injectStreamOptions(plain, false); err != nil || !bytes.Equal(out, plain) {
		t.Fatalf("non-stream body changed: %q err=%v", out, err)
	}

	if _, err := injectStreamOptions([]byte(`{"stream":true,"stream_options":"bad"}`), true); err != nil {
		t.Fatalf("non-object stream_options: %v", err)
	}
	if _, err := injectStreamOptions([]byte(`null`), true); err == nil {
		t.Fatal("null body was accepted")
	}
	if _, err := injectStreamOptions([]byte(`[1]`), true); err == nil {
		t.Fatal("array body was accepted")
	}
}
