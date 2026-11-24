package workflow

import (
	"context"
	"sort"
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
		s.cron.Schedule(zonedSchedule{parsed, scheduleLocation(schedule.Location)}, cron.FuncJob(func() {
			s.tick(ctx, schedule)
		}))
	}
	s.catchUp(ctx)
	s.cron.Start()
	return nil
}

func (s *scheduler) tick(ctx context.Context, schedule project.Schedule) {
	slot := tickSlot(schedule.Cron, s.r.now().UTC(), schedule.Location)
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
		slot, ok := mostRecentSlot(schedule.Cron, now, schedule.Location)
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

// allHours is the bit set of every hour in a parsed cron expression.
const allHours = 1<<24 - 1

// zonedSchedule evaluates a cron expression in a schedule's zone and returns
// the activation as a UTC instant. ParseStandard attaches the host's local
// zone; the wrapper supplies the bundle's zone instead, so a slot key never
// depends on the host.
//
// An expression that selects some hours but not all follows Vixie cron across
// a daylight-saving transition:
//
//   - A selected civil time inside the spring-forward gap does not exist. The
//     schedule fires once at the transition instant, the first valid local
//     time after the gap.
//   - A selected civil time inside the fall-back overlap exists twice. The
//     schedule fires once, at the first occurrence.
//
// An expression whose hour field selects every hour (every-minute, hourly, or
// every N minutes) is not special-cased. It runs on real elapsed time, which
// is the behavior cron gives it, so a repeated hour runs twice and a skipped
// hour does not run at all.
type zonedSchedule struct {
	cron.Schedule
	loc *time.Location
}

func (z zonedSchedule) Next(t time.Time) time.Time {
	if spec, ok := z.Schedule.(*cron.SpecSchedule); ok && spec.Hour&allHours != allHours {
		if next := nextWallClockSlot(spec, z.loc, t); !next.IsZero() {
			return next
		}
		return time.Time{}
	}
	return z.Schedule.Next(t.In(z.loc)).UTC()
}

// nextWallClockSlot returns the next activation of a fixed-hour expression,
// resolved through the zone's transition rules. It searches the cron fields in
// civil time and then maps the matching civil minute to its UTC instants.
func nextWallClockSlot(spec *cron.SpecSchedule, loc *time.Location, t time.Time) time.Time {
	// spec.Next on a UTC pseudo-time runs the field search on the calendar,
	// with no zone offset, because ParseStandard's schedule uses the input's
	// location when it is not time.Local.
	civil := civilMinute(t.In(loc))
	for range 8 {
		candidate := spec.Next(civil)
		if candidate.IsZero() {
			return time.Time{}
		}
		instants := civilInstants(candidate, loc)
		if len(instants) == 0 {
			fired := gapInstant(candidate, loc)
			if fired.IsZero() {
				return time.Time{}
			}
			if fired.After(t) {
				return fired.UTC()
			}
		} else if instants[0].After(t) {
			return instants[0].UTC()
		}
		// Every instant for this civil minute is at or before t: the first
		// fall-back occurrence already fired, or the transition already
		// passed. Search after the civil minute.
		civil = candidate
	}
	return time.Time{}
}

// civilMinute renders t's wall-clock minute in loc as a UTC pseudo-time, the
// calendar value the cron field search uses.
func civilMinute(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}

// civilInstants returns the UTC instants whose local time in loc is the given
// civil minute, in increasing order. One instant is normal, two mean the
// fall-back overlap, and none means the spring-forward gap.
func civilInstants(civil time.Time, loc *time.Location) []time.Time {
	offsets := make(map[int]struct{})
	// A gap or overlap is within a day of the civil time in UTC; probe both
	// sides so both offsets are seen even for zones far east or west.
	for _, probe := range []time.Duration{-30 * time.Hour, -12 * time.Hour, 0, 12 * time.Hour, 30 * time.Hour} {
		_, offset := civil.Add(probe).In(loc).Zone()
		offsets[offset] = struct{}{}
	}
	out := make([]time.Time, 0, 2)
	for offset := range offsets {
		instant := civil.Add(-time.Duration(offset) * time.Second)
		if sameCivilMinute(instant.In(loc), civil) {
			out = append(out, instant)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// sameCivilMinute reports whether local renders the given calendar minute.
func sameCivilMinute(local, civil time.Time) bool {
	return local.Second() == 0 &&
		local.Year() == civil.Year() && local.Month() == civil.Month() && local.Day() == civil.Day() &&
		local.Hour() == civil.Hour() && local.Minute() == civil.Minute()
}

// gapInstant returns the first UTC instant whose local time is at or after the
// civil minute, which for a civil time in the spring-forward gap is the
// transition instant. It returns zero when the surrounding offsets do not
// bracket a forward transition.
func gapInstant(civil time.Time, loc *time.Location) time.Time {
	_, before := civil.Add(-30 * time.Hour).In(loc).Zone()
	_, after := civil.Add(30 * time.Hour).In(loc).Zone()
	if after <= before {
		return time.Time{}
	}
	lo := civil.Add(-time.Duration(after) * time.Second).Unix()
	hi := civil.Add(-time.Duration(before) * time.Second).Unix()
	for lo < hi {
		mid := lo + (hi-lo)/2
		if civilAt(time.Unix(mid, 0), loc).Before(civil) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return time.Unix(lo, 0).UTC()
}

// civilAt renders a UTC instant's local calendar time as a UTC pseudo-time, so
// two civil times on the same date compare in wall-clock order.
func civilAt(t time.Time, loc *time.Location) time.Time {
	local := t.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(),
		local.Hour(), local.Minute(), local.Second(), 0, time.UTC)
}

// scheduleLocation returns the zone the cron fields are read in. A schedule
// from a loaded bundle always has one; an unset location is UTC.
func scheduleLocation(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}

// tickSlot is the activation a cron tick fires for. It comes from the cron
// expression, not the wake time, so a tick that runs late still uses the
// scheduled slot and its delivery key.
func tickSlot(expr string, now time.Time, loc *time.Location) time.Time {
	if slot, ok := mostRecentSlot(expr, now, loc); ok {
		return slot
	}
	return now.Truncate(time.Minute)
}

// mostRecentSlot returns the latest cron activation at or before now within
// the catch-up window. A fall-back overlap fires once at its first instant,
// so a later tick or a restart does not replay it.
func mostRecentSlot(expr string, now time.Time, loc *time.Location) (time.Time, bool) {
	parsed, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, false
	}
	schedule := zonedSchedule{parsed, scheduleLocation(loc)}
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
