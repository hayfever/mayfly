package httpapi

import (
	"context"
	"io"
	"log/slog"
)

// auditHandler enables every record regardless of its level. The process
// logger is filtered by MAYFLY_LOG_LEVEL; audit records must not be, or an
// operator who quiets request logs loses the audit trail.
type auditHandler struct {
	slog.Handler
}

func (h auditHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h auditHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return auditHandler{h.Handler.WithAttrs(attrs)}
}

func (h auditHandler) WithGroup(name string) slog.Handler {
	return auditHandler{h.Handler.WithGroup(name)}
}

// NewAuditLogger returns a JSON logger that writes every audit record to w,
// whatever the record level. Production passes os.Stderr, so audit lines sit
// on the same stream as the process log but are never filtered.
func NewAuditLogger(w io.Writer) *slog.Logger {
	return slog.New(auditHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})})
}
