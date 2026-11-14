package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type httpTransport struct {
	url        string
	headers    map[string]string
	client     *http.Client
	perRequest func(*http.Request)
	mu         sync.Mutex
	sessionID  string
}

// defaultHTTPClient is the client for a Streamable HTTP server: one request
// can wait as long as a tool may run.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

func newHTTPTransport(url string, headers map[string]string, client *http.Client, perRequest func(*http.Request)) *httpTransport {
	if client == nil {
		client = defaultHTTPClient()
	}
	return &httpTransport{
		url:        url,
		headers:    headers,
		client:     client,
		perRequest: perRequest,
	}
}

func (t *httpTransport) Do(ctx context.Context, id int64, payload []byte) ([]byte, error) {
	resp, err := t.post(ctx, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "text/event-stream") {
		return decodeSSE(resp.Body, id)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && len(probe.ID) > 0 && !matchesID(probe.ID, id) {
		return nil, fmt.Errorf("mcp http response id mismatch")
	}
	return raw, nil
}

func (t *httpTransport) Notify(ctx context.Context, payload []byte) error {
	resp, err := t.post(ctx, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

func (t *httpTransport) post(ctx context.Context, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	t.mu.Lock()
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if t.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", t.sessionID)
	}
	t.mu.Unlock()
	if t.perRequest != nil {
		t.perRequest(req)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp http request: %w", err)
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("mcp http status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func (t *httpTransport) Close() error {
	t.mu.Lock()
	sid := t.sessionID
	t.mu.Unlock()
	if sid == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Mcp-Session-Id", sid)
	if t.perRequest != nil {
		t.perRequest(req)
	}
	resp, err := t.client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return nil
}
