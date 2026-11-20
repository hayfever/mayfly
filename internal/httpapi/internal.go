package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hayfever/mayfly/internal/gateway"
	"github.com/hayfever/mayfly/internal/journal"
	"github.com/hayfever/mayfly/internal/metrics"
	"github.com/hayfever/mayfly/internal/model"
	"github.com/hayfever/mayfly/internal/session"
)

func (s *Server) internalEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := s.internalSession(r)
	if !ok {
		writeInternalError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req struct {
		Events []model.Event `json:"events"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeInternalError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	stored, err := s.mgr.EmitRuntimeEvents(id, bearerToken(r), req.Events)
	if err != nil {
		writeInternalError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": stored})
}

func (s *Server) internalJournal(w http.ResponseWriter, r *http.Request) {
	id, ok := s.internalSession(r)
	if !ok {
		writeInternalError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch r.Method {
	case http.MethodPost:
		limit := s.cfg.Journal.MaxEntryBytes
		if limit <= 0 {
			limit = 8 << 20
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			writeInternalError(w, http.StatusBadRequest, "read body")
			return
		}
		if int64(len(raw)) > limit {
			writeInternalError(w, http.StatusRequestEntityTooLarge, "journal entry is over journal.max_entry_bytes")
			return
		}
		var e journal.Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			writeInternalError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		stored, err := s.mgr.AppendJournal(r.Context(), id, bearerToken(r), e)
		switch {
		case errors.Is(err, session.ErrUnauthorized):
			writeInternalError(w, http.StatusUnauthorized, "unauthorized")
		case errors.Is(err, session.ErrNotFound):
			writeInternalError(w, http.StatusNotFound, "session not found")
		case errors.Is(err, journal.ErrInvalid):
			writeInternalError(w, http.StatusBadRequest, err.Error())
		case err != nil:
			s.logger.Error("append journal entry", "session", id, "error", err)
			writeInternalError(w, http.StatusInternalServerError, "journal write failed")
		default:
			writeJSON(w, http.StatusCreated, stored)
		}
	case http.MethodGet:
		query := r.URL.Query()
		if raw := query.Get("turn"); raw != "" {
			turn, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || turn <= 0 {
				writeInternalError(w, http.StatusBadRequest, "turn must be a positive integer")
				return
			}
			entries, err := s.mgr.JournalTurn(r.Context(), id, bearerToken(r), turn)
			if err != nil {
				writeJournalError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"turn": turn, "entries": entries})
			return
		}
		turn, err := s.mgr.JournalLastTurn(r.Context(), id, bearerToken(r))
		if err != nil {
			writeJournalError(w, err)
			return
		}
		entries := []journal.Entry{}
		if turn > 0 {
			entries, err = s.mgr.JournalTurn(r.Context(), id, bearerToken(r), turn)
			if err != nil {
				writeJournalError(w, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"turn": turn, "entries": entries})
	default:
		writeInternalError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func writeJournalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrUnauthorized):
		writeInternalError(w, http.StatusUnauthorized, "unauthorized")
	case errors.Is(err, session.ErrNotFound):
		writeInternalError(w, http.StatusNotFound, "session not found")
	case errors.Is(err, journal.ErrInvalid):
		writeInternalError(w, http.StatusBadRequest, err.Error())
	default:
		writeInternalError(w, http.StatusInternalServerError, "journal read failed")
	}
}

// harnessBuiltIn is the only harness the tool gateway serves. An alternate
// harness keeps reaching MCP servers through the broker's byte proxy and has
// no plugin tools, so the gateway routes answer 404 for it.
const harnessBuiltIn = "mayfly"

// toolSession authenticates one runtime request and applies the harness rule.
// It writes the response and reports false when the handler must stop.
func (s *Server) toolSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := s.internalSession(r)
	if !ok {
		writeInternalError(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	sess, ok := s.mgr.Get(id)
	if !ok {
		writeInternalError(w, http.StatusNotFound, "session not found")
		return "", false
	}
	if sess.Harness != "" && sess.Harness != harnessBuiltIn {
		writeInternalError(w, http.StatusNotFound, "the tool gateway serves only the built-in harness")
		return "", false
	}
	if s.gateway == nil {
		writeInternalError(w, http.StatusServiceUnavailable, "tool gateway is not configured")
		return "", false
	}
	return id, true
}

func (s *Server) internalTools(w http.ResponseWriter, r *http.Request) {
	id, ok := s.toolSession(w, r)
	if !ok {
		return
	}
	discovery, err := s.gateway.Discover(r.Context(), id)
	if err != nil {
		s.writeGatewayError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, discovery)
}

func (s *Server) internalToolCall(w http.ResponseWriter, r *http.Request) {
	id, ok := s.toolSession(w, r)
	if !ok {
		return
	}
	var req gateway.BeginRequest
	if err := decodeJSON(w, r, s.cfg.Server.MaxBodyBytes, &req); err != nil {
		writeInternalError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	resp, err := s.gateway.Begin(r.Context(), id, req)
	if err != nil {
		s.writeGatewayError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) internalToolComplete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.toolSession(w, r)
	if !ok {
		return
	}
	var req gateway.CompleteRequest
	if err := decodeJSON(w, r, s.cfg.Server.MaxBodyBytes, &req); err != nil {
		writeInternalError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.gateway.Complete(r.Context(), id, req); err != nil {
		s.writeGatewayError(w, id, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeGatewayError turns a gateway failure into its HTTP status. A rate
// limit carries Retry-After in whole seconds, rounded up as the runtime
// expects.
func (s *Server) writeGatewayError(w http.ResponseWriter, session string, err error) {
	var gwErr *gateway.Error
	if !errors.As(err, &gwErr) {
		s.logger.Error("tool gateway", "session", session, "error", err)
		writeInternalError(w, http.StatusInternalServerError, "tool gateway failed")
		return
	}
	if gwErr.RetryAfter > 0 {
		seconds := int64((gwErr.RetryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
	writeInternalError(w, gwErr.Status, gwErr.Message)
}

func (s *Server) internalInbox(w http.ResponseWriter, r *http.Request) {
	id, ok := s.internalSession(r)
	if !ok {
		writeInternalError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	s.mgr.RuntimeConnected(id)
	if last, ok := s.mgr.LastSeq(id); !ok {
		writeInternalError(w, http.StatusNotFound, "session not found")
		return
	} else if after > last {
		// The control plane restarted and sequence numbering restarted. Tell
		// the runtime to reconnect from zero instead of silently missing events.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: reset\ndata: {}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	ch, cancel, ok := s.mgr.SubscribeInbox(id, after)
	if !ok {
		writeInternalError(w, http.StatusNotFound, "session not found")
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	_, _ = fmt.Fprint(w, "retry: 1000\n: connected\n\n")
	if flusher != nil {
		flusher.Flush()
	}

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		case <-ping.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (s *Server) internalArtifact(w http.ResponseWriter, r *http.Request) {
	id, ok := s.internalSession(r)
	if !ok {
		writeInternalError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.ContentLength <= 0 {
		writeInternalError(w, http.StatusLengthRequired, "Content-Length is required")
		return
	}
	partial := strings.EqualFold(r.Header.Get("X-Mayfly-Partial"), "true")
	info, err := s.mgr.UploadArtifact(r.Context(), id, bearerToken(r), r.Body, r.ContentLength, r.Header.Get("X-Mayfly-SHA256"), partial)
	if err != nil {
		s.logger.Warn("artifact upload failed", "session", id, "error", err)
		writeInternalError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) internalFinalize(w http.ResponseWriter, r *http.Request) {
	id, ok := s.internalSession(r)
	if !ok {
		writeInternalError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeInternalError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	status := model.SessionStatus(req.Status)
	if status != model.StatusCompleted && status != model.StatusFailed {
		writeInternalError(w, http.StatusBadRequest, "status must be completed or failed")
		return
	}
	if err := s.mgr.Complete(id, bearerToken(r), status, req.Reason, req.Error); err != nil {
		writeInternalError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": string(status)})
}

func (s *Server) internalLLM(w http.ResponseWriter, r *http.Request) {
	id, ok := s.mgr.SessionIDForToken(bearerToken(r))
	if !ok {
		writeInternalError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	sess, ok := s.mgr.Get(id)
	if !ok {
		writeInternalError(w, http.StatusNotFound, "session not found")
		return
	}
	start := time.Now()
	rec := &requestRecorder{ResponseWriter: w}
	s.broker.ServeLLM(rec, r, sess.Model)
	// The broker writes the upstream status, so the result comes from the
	// response it produced. A stream counts until its last byte.
	result := metrics.ResultOK
	if rec.status >= http.StatusBadRequest {
		result = metrics.ResultError
	}
	s.metrics.LLMRequest(sess.Model, result, time.Since(start))
}

func (s *Server) internalMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost, http.MethodGet, http.MethodDelete:
	default:
		writeInternalError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, ok := s.mgr.SessionIDForToken(bearerToken(r))
	if !ok {
		writeInternalError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	server := r.PathValue("server")
	sess, ok := s.mgr.Get(id)
	if !ok {
		writeInternalError(w, http.StatusNotFound, "session not found")
		return
	}
	allowed := false
	for _, name := range sess.MCPServers {
		if name == server {
			allowed = true
			break
		}
	}
	if !allowed {
		writeInternalError(w, http.StatusForbidden, "MCP server is not assigned to this session")
		return
	}
	def, ok := s.mcpServers[server]
	if !ok {
		writeInternalError(w, http.StatusNotFound, "unknown MCP server")
		return
	}
	s.broker.ServeMCP(w, r, def)
}

func (s *Server) internalReadArtifact(w http.ResponseWriter, r *http.Request) {
	id, ok := s.internalSession(r)
	if !ok {
		writeInternalError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	query := r.URL.Query()
	key := strings.TrimSpace(query.Get("key"))
	if key == "" {
		writeInternalError(w, http.StatusBadRequest, "key is required")
		return
	}
	offset, err := parseWindowInt(query.Get("offset"), 0)
	if err != nil {
		writeInternalError(w, http.StatusBadRequest, "offset must be a non-negative integer")
		return
	}
	limit, err := parseWindowInt(query.Get("limit"), 24<<10)
	if err != nil || limit <= 0 {
		writeInternalError(w, http.StatusBadRequest, "limit must be a positive integer")
		return
	}
	window, err := s.mgr.ReadArtifact(r.Context(), id, bearerToken(r), key, offset, limit)
	switch {
	case errors.Is(err, session.ErrUnauthorized):
		writeInternalError(w, http.StatusUnauthorized, "unauthorized")
		return
	case errors.Is(err, session.ErrNotFound):
		writeInternalError(w, http.StatusNotFound, "artifact not found")
		return
	case err != nil:
		s.logger.Warn("read artifact failed", "session", id, "key", key, "error", err)
		writeInternalError(w, http.StatusInternalServerError, "artifact read failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key":          window.Key,
		"offset":       window.Offset,
		"bytes":        window.Bytes,
		"total":        window.Total,
		"next_offset":  window.NextOffset,
		"content_type": window.ContentType,
		"content":      string(window.Content),
	})
}

func parseWindowInt(raw string, fallback int64) (int64, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return value, nil
}
