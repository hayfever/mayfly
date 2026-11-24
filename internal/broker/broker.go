// Package broker proxies model and MCP traffic from session containers. This
// keeps upstream API keys and MCP credentials outside the session.
package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hayfever/mayfly/internal/config"
)

const defaultUpstreamTimeout = 180 * time.Second

// Broker serves the internal credential-proxy endpoints.
type Broker struct {
	cfg    config.LLMConfig
	logger *slog.Logger
	client *http.Client
}

// New builds a broker. The client bounds connection setup and the wait for
// upstream response headers, so a stuck provider cannot pin control-plane
// goroutines. Streamed bodies stay open until the caller disconnects.
func New(cfg config.LLMConfig, logger *slog.Logger) *Broker {
	timeout := cfg.Timeout.Std()
	if timeout <= 0 {
		timeout = defaultUpstreamTimeout
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: timeout,
	}
	return &Broker{cfg: cfg, logger: logger, client: &http.Client{Transport: transport}}
}

// HTTPClient returns the client that reaches upstream providers. It carries
// the broker's dial, TLS, and response-header timeouts, so a caller that
// speaks a protocol the broker proxies (such as the tool gateway's MCP
// clients) applies the same transport policy.
func (b *Broker) HTTPClient() *http.Client { return b.client }

// ServeLLM handles POST /internal/v1/llm/chat/completions. The session token is
// validated by the caller, which also passes the model assigned to the session.
// A session cannot select a different model, even an allowlisted one.
//
// admit runs after body and model validation and before the upstream request.
// It lets a caller enforce a budget: a non-nil error makes the broker write the
// hook's refusal (see Refusal) and skip the upstream call. A nil admit skips
// the check.
//
// The returned CallResult reports the upstream status, any usage the provider
// sent, and the byte counts. The proxy still passes the response through
// unparsed, so a caller sees the bytes the provider sent.
func (b *Broker) ServeLLM(w http.ResponseWriter, r *http.Request, sessionModel string, admit func(reqBytes int64, maxOut int64) error) CallResult {
	var res CallResult
	if r.Method != http.MethodPost {
		res.Status = http.StatusMethodNotAllowed
		res.RespBytes = writeError(w, res.Status, "method not allowed")
		return res
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, b.cfg.MaxBodyBytes+1))
	if err != nil {
		res.Status = http.StatusBadRequest
		res.RespBytes = writeError(w, res.Status, "read request body")
		return res
	}
	if int64(len(body)) > b.cfg.MaxBodyBytes {
		res.Status = http.StatusRequestEntityTooLarge
		res.RespBytes = writeError(w, res.Status, "request body too large")
		return res
	}
	var probe struct {
		Model               string          `json:"model"`
		Stream              bool            `json:"stream"`
		MaxTokens           json.RawMessage `json:"max_tokens"`
		MaxCompletionTokens json.RawMessage `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		res.Status = http.StatusBadRequest
		res.RespBytes = writeError(w, res.Status, "invalid JSON body")
		return res
	}
	switch probe.Model {
	case "":
		// Fill in the session model so the upstream cannot pick another one.
		body, err = setModel(body, sessionModel)
		if err != nil {
			res.Status = http.StatusBadRequest
			res.RespBytes = writeError(w, res.Status, "invalid JSON body")
			return res
		}
	case sessionModel:
	default:
		res.Status = http.StatusForbidden
		res.RespBytes = writeError(w, res.Status, "model is not assigned to this session")
		return res
	}
	if !b.cfg.ModelAllowed(sessionModel) {
		res.Status = http.StatusForbidden
		res.RespBytes = writeError(w, res.Status, "model is not allowed")
		return res
	}
	if probe.Stream {
		// Ask the provider to send usage in the final stream chunk. Only a
		// streamed request may carry stream_options.
		body, err = injectStreamOptions(body, true)
		if err != nil {
			res.Status = http.StatusBadRequest
			res.RespBytes = writeError(w, res.Status, "invalid JSON body")
			return res
		}
	}

	base := strings.TrimRight(b.cfg.BaseURL, "/")
	upstream, err := url.Parse(base + "/chat/completions")
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") {
		b.logger.Error("invalid llm base_url", "base_url", b.cfg.BaseURL)
		res.Status = http.StatusInternalServerError
		res.RespBytes = writeError(w, res.Status, "model backend is not configured")
		return res
	}

	res.ReqBytes = int64(len(body))
	if admit != nil {
		if err := admit(res.ReqBytes, outputLimit(probe.MaxCompletionTokens, probe.MaxTokens)); err != nil {
			res.Status, res.RespBytes = writeRefusal(w, err)
			return res
		}
	}
	// The hook admitted the call, so from here the reservation stands even if
	// the upstream never answers.
	res.Forwarded = true

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream.String(), bytes.NewReader(body))
	if err != nil {
		res.Status = http.StatusInternalServerError
		res.RespBytes = writeError(w, res.Status, "build upstream request")
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", r.Header.Get("Accept"))
	if b.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.cfg.APIKey)
	}
	for k, v := range b.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.logger.Warn("llm upstream request failed", "error", err)
		res.Status = http.StatusBadGateway
		res.RespBytes = writeError(w, res.Status, "model backend unavailable")
		return res
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeaders(w.Header(), resp.Header, "Content-Type", "Cache-Control")
	res.Status = resp.StatusCode
	w.WriteHeader(resp.StatusCode)

	if probe.Stream && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		res.Streamed = true
		scan := &usageScanner{}
		streamCopy(w, resp.Body, scan)
		res.RespBytes = scan.total
		res.Usage, res.Reported = scan.Result()
		return res
	}

	capture := &bodyCapture{limit: maxUsageBufferBytes}
	streamCopy(w, resp.Body, capture)
	res.RespBytes = capture.total
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && !capture.overflow {
		res.Usage, res.Reported = decodeUsage(capture.buf)
	}
	return res
}

// ServeMCP proxies one MCP server. upstream is the configured server definition;
// its headers were resolved from the control plane environment at startup.
func (b *Broker) ServeMCP(w http.ResponseWriter, r *http.Request, def config.MCPServerConfig) {
	if def.Transport != "http" {
		writeError(w, http.StatusNotFound, "MCP server is not an HTTP server")
		return
	}
	upstream, err := url.Parse(def.URL)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") {
		b.logger.Error("invalid mcp server url", "server", def.Name)
		writeError(w, http.StatusBadGateway, "MCP server is not configured correctly")
		return
	}
	limit := b.cfg.MaxBodyBytes
	if limit > 0 && r.ContentLength > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "MCP request body is too large")
		return
	}
	var body io.Reader
	if r.Body != nil {
		body = r.Body
		if limit > 0 {
			body = http.MaxBytesReader(w, r.Body, limit)
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream.String(), body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "build MCP request")
		return
	}
	req.ContentLength = r.ContentLength
	for _, name := range []string{"Content-Type", "Accept", "Mcp-Session-Id", "MCP-Protocol-Version", "Last-Event-ID"} {
		if v := r.Header.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
	for k, v := range def.ResolvedHeaders {
		req.Header.Set(k, v)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.logger.Warn("mcp upstream request failed", "server", def.Name, "error", err)
		writeError(w, http.StatusBadGateway, "MCP server unavailable")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeaders(w.Header(), resp.Header, "Content-Type", "Cache-Control", "Mcp-Session-Id", "MCP-Protocol-Version")
	w.WriteHeader(resp.StatusCode)
	streamCopy(w, resp.Body, nil)
}

// setModel returns the request body with the model field set to name.
func setModel(body []byte, name string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, errNotJSONObject
	}
	encoded, err := json.Marshal(name)
	if err != nil {
		return nil, err
	}
	doc["model"] = encoded
	return json.Marshal(doc)
}

func copyHeaders(dst, src http.Header, names ...string) {
	for _, name := range names {
		if values := src.Values(name); len(values) > 0 {
			dst[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
}

// streamCopy copies r to w and flushes after every read. A non-nil tee sees
// each chunk that reached the client, after the flush, so it never delays a
// stream. The tee's writes are ignored: it observes, it does not forward.
func streamCopy(w http.ResponseWriter, r io.Reader, tee io.Writer) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if tee != nil {
				_, _ = tee.Write(buf[:n])
			}
		}
		if err != nil {
			return
		}
	}
}

// writeJSON writes value as a JSON response and returns the bytes written.
func writeJSON(w http.ResponseWriter, status int, value any) int64 {
	payload, err := json.Marshal(value)
	if err != nil {
		payload = []byte(`{"error":"internal error"}`)
		status = http.StatusInternalServerError
	}
	payload = append(payload, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	n, _ := w.Write(payload)
	return int64(n)
}

// writeError writes one error object and returns the bytes written.
func writeError(w http.ResponseWriter, status int, message string) int64 {
	return writeJSON(w, status, map[string]string{"error": message})
}

// writeRefusal writes an admit-hook refusal and returns its status and the
// bytes written. An error that is not a *Refusal becomes a 500 with no code.
func writeRefusal(w http.ResponseWriter, err error) (int, int64) {
	status := http.StatusInternalServerError
	code := ""
	var refusal *Refusal
	if errors.As(err, &refusal) {
		if refusal.Status != 0 {
			status = refusal.Status
		}
		code = refusal.Code
	}
	body := map[string]string{"error": err.Error()}
	if code != "" {
		body["code"] = code
	}
	return status, writeJSON(w, status, body)
}
