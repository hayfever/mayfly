package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/hayfever/mayfly/internal/queue"
)

// queueRecord is the admin view of one job. The prompt is included only for a
// single job lookup; listing would multiply it by the page size.
type queueRecord struct {
	ID         string      `json:"id"`
	Trigger    string      `json:"trigger"`
	Delivery   string      `json:"delivery,omitempty"`
	State      queue.State `json:"state"`
	Attempts   int         `json:"attempts"`
	AcceptedAt time.Time   `json:"accepted_at"`
	FailedAt   *time.Time  `json:"failed_at,omitempty"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
	Error      string      `json:"error,omitempty"`
	SessionID  string      `json:"session_id,omitempty"`
	Prompt     string      `json:"prompt,omitempty"`
}

func queueView(record queue.Record, withPrompt bool) queueRecord {
	view := queueRecord{
		ID:         record.Job.ID,
		Trigger:    record.Job.Trigger,
		Delivery:   record.Job.Delivery,
		State:      record.State,
		Attempts:   record.Job.Attempts,
		AcceptedAt: record.Job.AcceptedAt,
		Error:      record.Error,
		SessionID:  record.SessionID,
	}
	if !record.FailedAt.IsZero() {
		failed := record.FailedAt
		view.FailedAt = &failed
	}
	if !record.FinishedAt.IsZero() {
		finished := record.FinishedAt
		view.FinishedAt = &finished
	}
	if withPrompt {
		view.Prompt = record.Job.Options.Prompt
	}
	return view
}

// queueJob reports one accepted job and the session it created, for
// queue.tombstone_ttl after completion.
func (s *Server) queueJob(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		writeError(w, http.StatusServiceUnavailable, "queue is not configured")
		return
	}
	record, ok, err := s.queue.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.logger.Warn("queue job lookup", "error", err)
		writeError(w, http.StatusServiceUnavailable, "queue is unavailable")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": queueView(record, true)})
}

// queueDead lists buried jobs, newest first.
func (s *Server) queueDead(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		writeError(w, http.StatusServiceUnavailable, "queue is not configured")
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = parsed
	}
	if limit > 500 {
		limit = 500
	}
	records, err := s.queue.Dead(r.Context(), limit)
	if err != nil {
		s.logger.Warn("queue dead list", "error", err)
		writeError(w, http.StatusServiceUnavailable, "queue is unavailable")
		return
	}
	out := make([]queueRecord, 0, len(records))
	for _, record := range records {
		out = append(out, queueView(record, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"dead": out})
}

// queueRetryDead puts a buried job back on the queue with a fresh attempt
// count.
func (s *Server) queueRetryDead(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		writeError(w, http.StatusServiceUnavailable, "queue is not configured")
		return
	}
	id := r.PathValue("id")
	if err := s.queue.Retry(r.Context(), id); err != nil {
		s.writeQueueAdminError(w, "retry", id, err)
		return
	}
	s.logger.Info("dead job requeued", "job", id)
	writeJSON(w, http.StatusAccepted, map[string]any{"retried": true, "id": id})
}

// queueDeleteDead drops a buried job without running it.
func (s *Server) queueDeleteDead(w http.ResponseWriter, r *http.Request) {
	if s.queue == nil {
		writeError(w, http.StatusServiceUnavailable, "queue is not configured")
		return
	}
	id := r.PathValue("id")
	if err := s.queue.Delete(r.Context(), id); err != nil {
		s.writeQueueAdminError(w, "delete", id, err)
		return
	}
	s.logger.Info("dead job deleted", "job", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeQueueAdminError(w http.ResponseWriter, action, id string, err error) {
	if errors.Is(err, queue.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	s.logger.Warn("queue admin action failed", "action", action, "job", id, "error", err)
	writeError(w, http.StatusServiceUnavailable, "queue is unavailable")
}
