package httpapi

import (
	"context"
	"net/http"
	"sort"

	"github.com/hayfever/mayfly/internal/queue"
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

// listSchedules returns the configured schedules in name order.
func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	query, err := parsePage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	out := []ScheduleInfo{}
	if s.schedules != nil && s.schedules.List != nil {
		out = append(out, s.schedules.List...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	start := 0
	if query.hasCursor {
		start = sort.Search(len(out), func(i int) bool { return out[i].Name > query.cursor.ID })
	}
	items, next := pageOf(out, start, query.limit, scheduleKey)
	writePage(w, items, next)
}

// scheduleKey orders schedules by name, so a cursor is the last name on the
// page.
func scheduleKey(info ScheduleInfo) cursorKey {
	return cursorKey{ID: info.Name}
}

func (s *Server) runSchedule(w http.ResponseWriter, r *http.Request) {
	if s.schedules == nil || s.schedules.Run == nil {
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "schedules are not configured")
		return
	}
	name := r.PathValue("name")
	jobID, err := s.schedules.Run(r.Context(), name)
	if err != nil {
		s.writeScheduleError(w, name, err)
		return
	}
	// A manual run uses a random delivery key, so it is never a duplicate.
	// Read the accepted job back for the delivery key when the queue is
	// reachable; the job is already durable, so a lookup failure must not
	// turn an accepted run into an error.
	job := acceptedJob{Accepted: true, JobID: jobID, Status: string(queue.StateQueued)}
	if s.queue != nil {
		if record, ok, err := s.queue.Get(r.Context(), jobID); err == nil && ok {
			job.Delivery = record.Job.IdempotencyKey
		} else if err != nil {
			s.logger.Warn("read accepted schedule job", "job", jobID, "error", err)
		}
	}
	writeJSON(w, http.StatusAccepted, job)
}
