package workflow

import (
	"context"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/hayfever/mayfly/internal/project"
)

// scheduler owns the cron entries for the bundle's schedules.
type scheduler struct {
	r      *Runner
	cron   *cron.Cron
	cancel context.CancelFunc
}

func newScheduler(r *Runner) *scheduler {
	return &scheduler{
		r:    r,
		cron: cron.New(cron.WithLocation(time.UTC)),
	}
}

func (s *scheduler) start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	for _, schedule := range s.r.bundle.Schedules {
		parsed, err := cron.ParseStandard(schedule.Cron)
		if err != nil {
			cancel()
			return err
		}
		schedule := schedule
		s.cron.Schedule(utcSchedule{parsed}, cron.FuncJob(func() {
			s.tick(ctx, schedule)
		}))
	}
	s.catchUp(ctx)
	s.cron.Start()
	return nil
}

func (s *scheduler) tick(ctx context.Context, schedule project.Schedule) {
	slot := tickSlot(schedule.Cron, s.r.now().UTC())
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.r.acceptSlot(runCtx, schedule, slot); err != nil {
		s.r.logger.Warn("schedule slot not accepted", "schedule", schedule.Name, "slot", slot, "error", err)
	}
}

// catchUp enqueues the most recent missed slot once. On a first start there is
// no watermark, so the runner records the current slot and waits for the next
// tick, the same way a fresh CronJob waits for its first schedule.
func (s *scheduler) catchUp(ctx context.Context) {
	now := s.r.now().UTC()
	for _, schedule := range s.r.bundle.Schedules {
		slot, ok := mostRecentSlot(schedule.Cron, now)
		if !ok {
			continue
		}
		last, seen, err := s.r.state.LastSlot(ctx, schedule.Name)
		if err != nil {
			s.r.logger.Warn("read schedule watermark", "schedule", schedule.Name, "error", err)
			continue
		}
		if !seen {
			if err := s.r.state.SetLastSlot(ctx, schedule.Name, slot); err != nil {
				s.r.logger.Warn("record schedule watermark", "schedule", schedule.Name, "error", err)
			}
			continue
		}
		if !last.Before(slot) {
			continue
		}
		if now.Sub(slot) > catchUpWindow {
			// Too old to replay. Move the watermark forward so the next start
			// does not reconsider it.
			if err := s.r.state.SetLastSlot(ctx, schedule.Name, slot); err != nil {
				s.r.logger.Warn("record schedule watermark", "schedule", schedule.Name, "error", err)
			}
			continue
		}
		runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = s.r.acceptSlot(runCtx, schedule, slot)
		cancel()
		if err != nil {
			s.r.logger.Warn("schedule catch-up failed", "schedule", schedule.Name, "slot", slot, "error", err)
		}
	}
}

func (s *scheduler) close() {
	if s == nil {
		return
	}
	done := s.cron.Stop()
	select {
	case <-done.Done():
	case <-time.After(10 * time.Second):
	}
	if s.cancel != nil {
		s.cancel()
	}
}

// utcSchedule normalizes cron activations to UTC. ParseStandard puts the local
// zone on its spec, and a slot key must not depend on the host zone.
type utcSchedule struct {
	cron.Schedule
}

func (u utcSchedule) Next(t time.Time) time.Time {
	return u.Schedule.Next(t.UTC()).UTC()
}

// tickSlot is the activation a cron tick fires for. It comes from the cron
// expression, not the wake time, so a tick that runs late still uses the
// scheduled slot and its delivery key.
func tickSlot(expr string, now time.Time) time.Time {
	if slot, ok := mostRecentSlot(expr, now); ok {
		return slot
	}
	return now.Truncate(time.Minute)
}

// mostRecentSlot returns the latest cron activation at or before now within
// the catch-up window.
func mostRecentSlot(expr string, now time.Time) (time.Time, bool) {
	parsed, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, false
	}
	schedule := utcSchedule{parsed}
	cursor := now.Add(-catchUpWindow)
	var last time.Time
	for range 20000 {
		next := schedule.Next(cursor)
		if next.IsZero() || !next.After(cursor) || next.After(now) {
			break
		}
		last = next
		cursor = next
	}
	return last, !last.IsZero()
}
