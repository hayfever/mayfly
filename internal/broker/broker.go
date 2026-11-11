// Package broker proxies model and MCP traffic from session containers. This
// keeps upstream API keys and MCP credentials outside the session.
package broker

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/hayden/mayfly/internal/config"
)

// Broker serves the internal credential-proxy endpoints.
type Broker struct {
	cfg    config.LLMConfig
	logger *slog.Logger
	client *http.Client
}

// New builds a broker.
func New(cfg config.LLMConfig, logger *slog.Logger) *Broker {
	return &Broker{cfg: cfg, logger: logger, client: &http.Client{}}
}

// ServeLLM handles POST /internal/v1/llm/chat/completions. The session token is
// validated by the caller.
func (b *Broker) ServeLLM(w http.ResponseWriter, r *http.Request) {
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
	if probe.Model == "" {
		probe.Model = b.cfg.Model
	}
	if !b.cfg.ModelAllowed(probe.Model) {
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
	defer resp.Body.Close()
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
	var body io.Reader
	if r.Body != nil {
		body = r.Body
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
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header, "Content-Type", "Cache-Control", "Mcp-Session-Id", "MCP-Protocol-Version")
	w.WriteHeader(resp.StatusCode)
	streamCopy(w, resp.Body)
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
