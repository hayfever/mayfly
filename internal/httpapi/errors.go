package httpapi

import (
	"errors"
	"net/http"

	"github.com/hayfever/mayfly/internal/queue"
	"github.com/hayfever/mayfly/internal/session"
	"github.com/hayfever/mayfly/internal/workflow"
)

// Error codes carried by the API error envelope. The set is fixed by
// api/openapi.yaml: add a code here and in the spec together.
const (
	codeInvalidRequest   = "invalid_request"
	codeUnauthorized     = "unauthorized"
	codeForbidden        = "forbidden"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
	codeConflict         = "conflict"
	codePayloadTooLarge  = "payload_too_large"
	codeRateLimited      = "rate_limited"
	codeUnavailable      = "unavailable"
	codeInternal         = "internal"
)

// errorEnvelope is the body of every failed /api/v1 and /hooks request.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// errorBody carries a stable machine code and a message for people. Details
// holds optional, code-specific context such as the id of a half-created
// session.
type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeErrorDetails(w, status, code, message, nil)
}

func writeErrorDetails(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	// The request id lets a person correlate the response with its log line.
	if id := requestIDFromWriter(w); id != "" {
		merged := make(map[string]any, len(details)+1)
		for key, value := range details {
			merged[key] = value
		}
		merged["request_id"] = id
		details = merged
	}
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message, Details: details}})
}

// requestIDFromWriter walks the response writer wrappers for the id that the
// request middleware assigned. No id means the response is outside a request,
// as in a direct handler test.
func requestIDFromWriter(w http.ResponseWriter) string {
	for {
		if carrier, ok := w.(interface{ requestID() string }); ok {
			if id := carrier.requestID(); id != "" {
				return id
			}
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return ""
		}
		w = unwrapper.Unwrap()
	}
}

// writeInternalError writes the runtime protocol error body. The /internal/v1
// shapes are versioned with the session runtime, not with this API, so they
// keep the plain {"error": "..."} body.
func writeInternalError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeSessionError maps a manager error to the envelope. An error without a
// known sentinel becomes 500: a validation problem is reported by
// Manager.Validate before the call, so anything left is a server fault.
func (s *Server) writeSessionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, err.Error())
	case errors.Is(err, session.ErrClosed):
		writeError(w, http.StatusConflict, codeConflict, err.Error())
	case errors.Is(err, session.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, codeUnauthorized, err.Error())
	case errors.Is(err, session.ErrCapacity):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, codeRateLimited, err.Error())
	case errors.Is(err, session.ErrEmptyMessage), errors.Is(err, session.ErrMessageTooLong):
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
	default:
		s.logger.Error("session request failed", "error", err)
		writeError(w, http.StatusInternalServerError, codeInternal, "session request failed")
	}
}

// writeQueueAdminError maps a queue store error from an admin action.
func (s *Server) writeQueueAdminError(w http.ResponseWriter, action, id string, err error) {
	if errors.Is(err, queue.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeNotFound, "job not found")
		return
	}
	s.logger.Warn("queue admin action failed", "action", action, "job", id, "error", err)
	writeError(w, http.StatusServiceUnavailable, codeUnavailable, "queue is unavailable")
}

// writeScheduleError maps a manual schedule run error.
func (s *Server) writeScheduleError(w http.ResponseWriter, name string, err error) {
	switch {
	case errors.Is(err, workflow.ErrUnknownSchedule):
		writeError(w, http.StatusNotFound, codeNotFound, "schedule not found")
	case errors.Is(err, workflow.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, err.Error())
	default:
		s.logger.Error("run schedule", "schedule", name, "error", err)
		writeError(w, http.StatusInternalServerError, codeInternal, "schedule run failed")
	}
}
