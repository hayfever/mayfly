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
	"strings"
	"time"

	"net/http"

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
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("emit events: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

var errInboxReset = errors.New("inbox sequence reset")

// Inbox streams human messages and control actions until ctx is canceled.
// The callback receives events in order. Gaps are reported as a system event
// with type control and data action "gap".
func (c *Client) Inbox(ctx context.Context, after int64, fn func(model.Event)) error {
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
	defer resp.Body.Close()
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
	defer f.Close()
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
	defer resp.Body.Close()
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
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("finalize: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
