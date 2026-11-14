package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/hayfever/mayfly/internal/workflow"
)

// ScheduleInfo is the public description of one configured schedule.
type ScheduleInfo = workflow.ScheduleInfo

// ScheduleService lets the API list schedules and enqueue a manual run.
type ScheduleService struct {
	List []ScheduleInfo
	Run  func(ctx context.Context, name string) (string, error)
}

// Option configures optional server features.
type Option func(*Server)

// WithSchedules attaches the scheduler to the API.
func WithSchedules(service ScheduleService) Option {
	return func(s *Server) {
		s.schedules = &service
	}
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	out := []ScheduleInfo{}
	if s.schedules != nil && s.schedules.List != nil {
		out = s.schedules.List
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": out})
}

func (s *Server) runSchedule(w http.ResponseWriter, r *http.Request) {
	if s.schedules == nil || s.schedules.Run == nil {
		writeError(w, http.StatusServiceUnavailable, "schedules are not configured")
		return
	}
	name := r.PathValue("name")
	jobID, err := s.schedules.Run(r.Context(), name)
	switch {
	case errors.Is(err, workflow.ErrUnknownSchedule):
		writeError(w, http.StatusNotFound, "schedule not found")
	case errors.Is(err, workflow.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		s.logger.Error("run schedule", "schedule", name, "error", err)
		writeError(w, http.StatusInternalServerError, "schedule run failed")
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID})
	}
}
