package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/hayfever/mayfly/internal/triggers"
)

func (s *Server) receiveWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, ok := s.triggers[name]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown trigger")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "webhook body is too large")
		return
	}
	result, err := triggers.Parse(cfg, r.Header, body)
	if err != nil {
		switch {
		case errors.Is(err, triggers.ErrUnauthorized):
			writeError(w, http.StatusUnauthorized, "webhook verification failed")
		case errors.Is(err, triggers.ErrBadRequest):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			s.logger.Warn("webhook parse failed", "trigger", name, "error", err)
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	if result.Challenge != nil {
		writeJSON(w, http.StatusOK, result.Challenge)
		return
	}
	// Spend a rate-limit token only for a verified request that will create a
	// session. Unsigned traffic must not be able to exhaust a trigger's budget.
	if limiter := s.limiters[name]; !limiter.Allow() {
		seconds := int(limiter.RetryAfter() / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		s.logger.Warn("webhook rate limited", "trigger", name, "retry_after_seconds", seconds)
		writeError(w, http.StatusTooManyRequests, "trigger rate limit reached")
		return
	}
	created, err := s.mgr.Create(r.Context(), result.Options)
	if err != nil {
		if created == nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "session": created})
		return
	}
	link := s.sessionURL(created.ID)
	if cfg.Type == "slack" {
		writeJSON(w, http.StatusOK, map[string]any{
			"response_type": "in_channel",
			"text":          "Mayfly session started: " + link,
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"session_id": created.ID,
		"status":     created.Status,
		"url":        link,
	})
}
