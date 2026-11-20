// Package broker proxies model and MCP traffic from session containers. This
// keeps upstream API keys and MCP credentials outside the session.
package broker

import (
	"bytes"
	"encoding/json"
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
func (b *Broker) ServeLLM(w http.ResponseWriter, r *http.Request, sessionModel string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, b.cfg.MaxBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body")
		return
	}
	if int64(len(body)) > b.cfg.MaxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	switch probe.Model {
	case "":
		// Fill in the session model so the upstream cannot pick another one.
		body, err = setModel(body, sessionModel)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	case sessionModel:
	default:
		writeError(w, http.StatusForbidden, "model is not assigned to this session")
		return
	}
	if !b.cfg.ModelAllowed(sessionModel) {
		writeError(w, http.StatusForbidden, "model is not allowed")
		return
	}

	base := strings.TrimRight(b.cfg.BaseURL, "/")
	upstream, err := url.Parse(base + "/chat/completions")
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") {
		b.logger.Error("invalid llm base_url", "base_url", b.cfg.BaseURL)
		writeError(w, http.StatusInternalServerError, "model backend is not configured")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream.String(), bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "build upstream request")
		return
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
		writeError(w, http.StatusBadGateway, "model backend unavailable")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeaders(w.Header(), resp.Header, "Content-Type", "Cache-Control")
	w.WriteHeader(resp.StatusCode)
	streamCopy(w, resp.Body)
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
	streamCopy(w, resp.Body)
}

// setModel returns the request body with the model field set to name.
func setModel(body []byte, name string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
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

func streamCopy(w http.ResponseWriter, r io.Reader) {
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
		}
		if err != nil {
			return
		}
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
