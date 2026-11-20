// Package platformclient is the session runtime's client for the control
// plane: event ingestion, the inbox stream, artifact upload, and finalize.
package platformclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"net/http"

	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/model"
)

// Client talks to the control plane with a scoped session token.
type Client struct {
	baseURL   string
	sessionID string
	token     string
	http      *http.Client
	logger    *slog.Logger
}

// New creates a session client.
func New(baseURL, sessionID, token string) *Client {
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		sessionID: sessionID,
		token:     token,
		http:      &http.Client{},
		logger:    slog.Default(),
	}
}

// SetLogger replaces the logger.
func (c *Client) SetLogger(l *slog.Logger) { c.logger = l }

func (c *Client) path(suffix string) string {
	return c.baseURL + "/internal/v1/sessions/" + url.PathEscape(c.sessionID) + suffix
}

// Emit sends events to the control plane.
func (c *Client) Emit(ctx context.Context, events ...model.Event) error {
	if len(events) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"events": events})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.path("/events"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("emit events: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("emit events: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

var errInboxReset = errors.New("inbox sequence reset")

// Inbox streams human messages and control actions until ctx is canceled. The
// callback receives events in order. onReset runs when the control plane was
// replaced and the stream restarts from zero, which invalidates any sequence
// the runtime remembered.
func (c *Client) Inbox(ctx context.Context, after int64, fn func(model.Event), onReset func()) error {
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := c.inboxOnce(ctx, after, func(ev model.Event) {
			if ev.Seq > after {
				after = ev.Seq
			}
			fn(ev)
		})
		if errors.Is(err, errInboxReset) {
			after = 0
			backoff = time.Second
			if onReset != nil {
				onReset()
			}
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			c.logger.Warn("inbox stream interrupted", "error", err, "retry_in", backoff.String())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) inboxOnce(ctx context.Context, after int64, fn func(model.Event)) error {
	u := fmt.Sprintf("%s?after=%d", c.path("/inbox"), after)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("inbox: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data bytes.Buffer
	eventName := ""
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if eventName == "reset" {
				return errInboxReset
			}
			if data.Len() == 0 {
				eventName = ""
				continue
			}
			var ev model.Event
			if err := json.Unmarshal(data.Bytes(), &ev); err == nil {
				fn(ev)
			}
			data.Reset()
			eventName = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			v := strings.TrimPrefix(line, "data:")
			v = strings.TrimPrefix(v, " ")
			data.WriteString(v)
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("inbox read: %w", err)
	}
	return fmt.Errorf("inbox stream closed")
}

// UploadArtifact sends the bundle tarball. It retries transient failures.
func (c *Client) UploadArtifact(ctx context.Context, path, sha256Hex string, size int64, partial bool) (model.ArtifactInfo, error) {
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return model.ArtifactInfo{}, ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * time.Second):
			}
		}
		info, err := c.uploadOnce(ctx, path, sha256Hex, size, partial)
		if err == nil {
			return info, nil
		}
		lastErr = err
		c.logger.Warn("artifact upload failed", "attempt", attempt+1, "error", err)
	}
	return model.ArtifactInfo{}, lastErr
}

func (c *Client) uploadOnce(ctx context.Context, path, sha256Hex string, size int64, partial bool) (model.ArtifactInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return model.ArtifactInfo{}, err
	}
	defer func() { _ = f.Close() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.path("/artifacts"), f)
	if err != nil {
		return model.ArtifactInfo{}, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Mayfly-SHA256", sha256Hex)
	if partial {
		req.Header.Set("X-Mayfly-Partial", "true")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return model.ArtifactInfo{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return model.ArtifactInfo{}, fmt.Errorf("upload artifact: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var info model.ArtifactInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return model.ArtifactInfo{}, fmt.Errorf("decode artifact response: %w", err)
	}
	return info, nil
}

// Finalize reports terminal status to the control plane.
func (c *Client) Finalize(ctx context.Context, status model.SessionStatus, reason, errMsg string) error {
	body, err := json.Marshal(map[string]string{
		"status": string(status),
		"reason": reason,
		"error":  errMsg,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.path("/finalize"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("finalize: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Tool gateway errors, one per outcome the runtime acts on. A ToolError
// unwraps to the sentinel for its status, so a caller can use errors.Is.
var (
	// ErrToolNotGranted reports a tool the session may not call (403).
	ErrToolNotGranted = errors.New("tool is not granted to this session")
	// ErrToolConflict reports a call key reused for another tool or with
	// other arguments (409). The runtime treats it as fatal.
	ErrToolConflict = errors.New("tool call key conflicts with an earlier call")
	// ErrToolRateLimited reports a plugin rate limit (429). ToolError
	// carries the server's RetryAfter.
	ErrToolRateLimited = errors.New("tool call is rate limited")
	// ErrToolUnavailable reports no free in-flight slot (503). The call can
	// proceed after the runtime waits.
	ErrToolUnavailable = errors.New("tool gateway is unavailable")
)

// ToolError is a rejected tool gateway request. Status is the HTTP status and
// Message is the server's text; Unwrap maps the status to the sentinel the
// runtime switches on.
type ToolError struct {
	Op         string
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.Op, e.Status, e.Message)
}

// Unwrap reports the typed outcome for the status.
func (e *ToolError) Unwrap() error {
	switch e.Status {
	case http.StatusForbidden:
		return ErrToolNotGranted
	case http.StatusConflict:
		return ErrToolConflict
	case http.StatusTooManyRequests:
		return ErrToolRateLimited
	case http.StatusServiceUnavailable:
		return ErrToolUnavailable
	}
	return nil
}

// Tools lists the session's tool grant.
func (c *Client) Tools(ctx context.Context) (gateway.Discovery, error) {
	var discovery gateway.Discovery
	if err := c.toolRequest(ctx, "list tools", http.MethodGet, "/tools", nil, &discovery); err != nil {
		return gateway.Discovery{}, err
	}
	if discovery.Tools == nil {
		discovery.Tools = []gateway.DiscoveryTool{}
	}
	return discovery, nil
}

// BeginCall asks the gateway to authorize one tool call. The answer is either
// a recorded result or a dispatch for the runtime to perform.
func (c *Client) BeginCall(ctx context.Context, req gateway.BeginRequest) (gateway.BeginResponse, error) {
	var resp gateway.BeginResponse
	if err := c.toolRequest(ctx, "begin tool call", http.MethodPost, "/tools/calls", req, &resp); err != nil {
		return gateway.BeginResponse{}, err
	}
	return resp, nil
}

// CompleteCall reports the result of a locally dispatched call. Repeating it
// with the same content succeeds.
func (c *Client) CompleteCall(ctx context.Context, req gateway.CompleteRequest) error {
	return c.toolRequest(ctx, "complete tool call", http.MethodPost, "/tools/calls/complete", req, nil)
}

// toolRequest sends one gateway request and decodes the response into dst. A
// non-2xx answer becomes a ToolError. dst may be nil for an empty response.
func (c *Client) toolRequest(ctx context.Context, op, method, suffix string, body, dst any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.path(suffix), reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &ToolError{
			Op:         op,
			Status:     resp.StatusCode,
			Message:    errorMessage(msg),
			RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if dst == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// errorMessage reads the gateway's {"error": "..."} envelope so the runtime
// can show its message as the tool result. Anything else is passed through
// trimmed, as the journal routes do.
func errorMessage(body []byte) string {
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != "" {
		return envelope.Error
	}
	return strings.TrimSpace(string(body))
}

// retryAfter reads a Retry-After header in delta-seconds.
func retryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// httpStatusError carries a non-2xx response from the control plane.
type httpStatusError struct {
	op     string
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.op, e.status, e.body)
}

// retryable reports whether a journal request can be repeated. Client errors
// other than timeouts and rate limits will not succeed on a retry.
func retryable(err error) bool {
	var status *httpStatusError
	if errors.As(err, &status) {
		if status.status == http.StatusRequestTimeout || status.status == http.StatusTooManyRequests {
			return true
		}
		return status.status >= 500
	}
	return !errors.Is(err, context.Canceled)
}

// journalRetryBudget bounds one journal request across all its attempts. A
// control-plane restart is covered; a partition that outlasts the budget ends
// the turn so a replacement resumes from the journal.
var journalRetryBudget = 5 * time.Minute

// journalRetry runs one idempotent journal request until it succeeds, is
// rejected permanently, or the budget ends. A control-plane restart must not
// make a step execute without its durable record.
func journalRetry(ctx context.Context, op string, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, journalRetryBudget)
	defer cancel()
	backoff := 500 * time.Millisecond
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := fn(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		if !retryable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", op, ctx.Err())
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// AppendJournal records one turn step before its effect is committed locally.
func (c *Client) AppendJournal(ctx context.Context, e journal.Entry) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return journalRetry(ctx, "append journal", func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.path("/journal"), bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("append journal: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 300 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return &httpStatusError{op: "append journal", status: resp.StatusCode, body: strings.TrimSpace(string(msg))}
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	})
}

// TurnJournal reads the steps of one turn.
func (c *Client) TurnJournal(ctx context.Context, session string, turn int64) ([]journal.Entry, error) {
	var out []journal.Entry
	err := journalRetry(ctx, "read journal", func(ctx context.Context) error {
		u := fmt.Sprintf("%s?turn=%d", c.path("/journal"), turn)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("read journal: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 300 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return &httpStatusError{op: "read journal", status: resp.StatusCode, body: strings.TrimSpace(string(msg))}
		}
		var payload struct {
			Entries []journal.Entry `json:"entries"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return fmt.Errorf("decode journal response: %w", err)
		}
		out = payload.Entries
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []journal.Entry{}
	}
	return out, nil
}

// LastJournalTurn returns the highest journaled turn for the session.
func (c *Client) LastJournalTurn(ctx context.Context) (int64, error) {
	var turn int64
	err := journalRetry(ctx, "read last journal turn", func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.path("/journal"), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("read last journal turn: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 300 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return &httpStatusError{op: "read last journal turn", status: resp.StatusCode, body: strings.TrimSpace(string(msg))}
		}
		var payload struct {
			Turn int64 `json:"turn"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return fmt.Errorf("decode journal response: %w", err)
		}
		turn = payload.Turn
		return nil
	})
	return turn, err
}

// Append, Turn, and LastTurn make the client a journal.Store for one session.
func (c *Client) Append(ctx context.Context, e journal.Entry) error {
	return c.AppendJournal(ctx, e)
}

func (c *Client) Turn(ctx context.Context, _ string, turn int64) ([]journal.Entry, error) {
	return c.TurnJournal(ctx, c.sessionID, turn)
}

func (c *Client) LastTurn(ctx context.Context, _ string) (int64, error) {
	return c.LastJournalTurn(ctx)
}

// ArtifactWindow is one bounded read of a stored session input.
type ArtifactWindow struct {
	Key         string `json:"key"`
	Offset      int64  `json:"offset"`
	Bytes       int64  `json:"bytes"`
	Total       int64  `json:"total"`
	NextOffset  *int64 `json:"next_offset"`
	ContentType string `json:"content_type"`
	Content     string `json:"content"`
}

// ReadArtifact reads a window of an artifact the control plane granted this
// session. The token in the client authorizes only that session's grants.
func (c *Client) ReadArtifact(ctx context.Context, key string, offset, limit int64) (ArtifactWindow, error) {
	query := url.Values{}
	query.Set("key", key)
	query.Set("offset", strconv.FormatInt(offset, 10))
	query.Set("limit", strconv.FormatInt(limit, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.path("/artifacts")+"?"+query.Encode(), nil)
	if err != nil {
		return ArtifactWindow{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return ArtifactWindow{}, fmt.Errorf("read artifact: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ArtifactWindow{}, fmt.Errorf("read artifact: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var window ArtifactWindow
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&window); err != nil {
		return ArtifactWindow{}, fmt.Errorf("read artifact: %w", err)
	}
	return window, nil
}
