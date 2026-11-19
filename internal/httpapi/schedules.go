package httpapi

import (
	"context"
	"net/http"
	"sort"

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
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID})
}
