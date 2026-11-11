package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/triggers"
)

// receiveWebhook verifies a delivery, stores it as a job, and answers. The
// session is created later by a worker, so a slow cluster cannot make the
// sender time out or retry.
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
	if s.queue == nil {
		writeError(w, http.StatusServiceUnavailable, "queue is not configured")
		return
	}
	// Reject requests the worker could never run while the sender is still
	// listening. Create repeats these checks when the job runs.
	if err := s.mgr.Validate(result.Options); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job := queue.NewJob(cfg.Name, result.DeliveryKey, result.Options, time.Now())
	switch err := s.queue.Accept(r.Context(), job); {
	case errors.Is(err, queue.ErrDuplicate):
		s.logger.Info("duplicate webhook delivery ignored", "trigger", name, "job", job.ID)
		writeAccepted(w, job, true)
		return
	case errors.Is(err, queue.ErrFull):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "queue is full")
		return
	case err != nil:
		s.logger.Error("accept webhook job", "trigger", name, "error", err)
		writeError(w, http.StatusServiceUnavailable, "queue is unavailable")
		return
	}
	s.logger.Info("webhook accepted", "trigger", name, "job", job.ID, "delivery", job.Delivery)
	if cfg.Type == "slack" {
		// Slack wants an answer within three seconds; the link is not known yet.
		writeJSON(w, http.StatusOK, map[string]any{
			"response_type": "in_channel",
			"text":          "Mayfly session queued.",
		})
		return
	}
	writeAccepted(w, job, false)
}

func writeAccepted(w http.ResponseWriter, job queue.Job, duplicate bool) {
	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted":  true,
		"duplicate": duplicate,
		"job_id":    job.ID,
		"delivery":  job.Delivery,
		"status":    "queued",
	})
}
