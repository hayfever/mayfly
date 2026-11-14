package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// auditRecord is one line of the plugin audit log. It deliberately has no
// field for arguments or results: the gateway cannot log what it cannot pass
// here, and both carry workspace data.
type auditRecord struct {
	Time        time.Time `json:"time"`
	Event       string    `json:"event"`
	Session     string    `json:"session"`
	Agent       string    `json:"agent,omitempty"`
	Tool        string    `json:"tool"`
	Owner       string    `json:"owner,omitempty"`
	Placement   string    `json:"placement"`
	Key         string    `json:"key"`
	ArgsSHA256  string    `json:"args_sha256"`
	Attempt     int       `json:"attempt"`
	DurationMS  int64     `json:"duration_ms,omitempty"`
	IsError     bool      `json:"is_error,omitempty"`
	Replayed    bool      `json:"replayed,omitempty"`
	Interrupted bool      `json:"interrupted,omitempty"`
}

// Audit writes one line per begin and per done to the mayflyd logger and,
// when a path is set, appends the same record as JSON to that file.
type Audit struct {
	mu     sync.Mutex
	logger *slog.Logger
	file   *os.File
	err    error // first write error, reported once
}

// NewAudit opens the optional JSONL file. An empty path keeps the audit in
// the process log only. The file is opened for append with owner-only mode;
// plugin arguments and results never reach it.
func NewAudit(path string, logger *slog.Logger) (*Audit, error) {
	a := &Audit{logger: logger}
	if path == "" {
		return a, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create plugin audit directory: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open plugin audit log: %w", err)
	}
	a.file = file
	return a, nil
}

// Close releases the JSONL file.
func (a *Audit) Close() error {
	if a == nil || a.file == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	file := a.file
	a.file = nil
	return file.Close()
}

// write emits one record to both sinks. The file write error is reported
// once, then swallowed so a full disk cannot fail a tool call.
func (a *Audit) write(ctx context.Context, rec auditRecord) {
	if a == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if a.logger != nil {
		attrs := []slog.Attr{
			slog.String("event", rec.Event),
			slog.String("session", rec.Session),
			slog.String("tool", rec.Tool),
			slog.String("owner", rec.Owner),
			slog.String("placement", rec.Placement),
			slog.String("key", rec.Key),
			slog.String("args_sha256", rec.ArgsSHA256),
			slog.Int("attempt", rec.Attempt),
		}
		if rec.Agent != "" {
			attrs = append(attrs, slog.String("agent", rec.Agent))
		}
		if rec.Event == "call.done" {
			attrs = append(attrs,
				slog.Duration("duration", time.Duration(rec.DurationMS)*time.Millisecond),
				slog.Bool("is_error", rec.IsError),
				slog.Bool("replayed", rec.Replayed),
				slog.Bool("interrupted", rec.Interrupted),
			)
		}
		a.logger.LogAttrs(ctx, slog.LevelInfo, "plugin call", attrs...)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil || a.err != nil {
		return
	}
	line, err := json.Marshal(rec)
	if err == nil {
		_, err = a.file.Write(append(line, '\n'))
	}
	if err != nil {
		a.err = err
		if a.logger != nil {
			a.logger.Error("write plugin audit log", "error", err)
		}
	}
}
