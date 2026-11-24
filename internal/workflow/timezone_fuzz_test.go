package workflow

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

// FuzzCronTZNext drives the zone-aware cron evaluation that the scheduler
// installs. For any expression, zone, and start instant that parse:
//
//   - every fire is a UTC instant strictly after the previous one, and never
//     equal to the input;
//   - every fire is on a whole minute;
//   - the same input produces the same fire, so the evaluation is stable;
//   - a fixed-hour expression fires at a civil time its fields select, or at
//     the instant of a zone transition (the spring-forward fire), never twice
//     at the same civil minute (the fall-back rule), and, when it selects every
//     day, at most 26 hours apart, so the spring-forward day is not skipped;
//   - an expression whose hour field selects every hour runs on real elapsed
//     time, so its fires match the minute field and a repeated civil hour may
//     legitimately fire twice.
//
// Seeds sit on the 2026 America/New_York and America/Chicago transitions and
// the half-hour Australia/Lord_Howe transitions.
func FuzzCronTZNext(f *testing.F) {
	zones := []string{
		"America/Chicago",
		"America/New_York",
		"Australia/Lord_Howe",
		"Europe/Berlin",
		"UTC",
		"Etc/UTC",
	}
	seededZones := make(map[string]bool, len(zones))
	for _, zone := range zones {
		seededZones[zone] = true
	}
	exprs := []string{
		"0 7 * * *",
		"30 2 * * *",
		"30 1 * * *",
		"45 2 * * *",
		"* * * * *",
		"0 * * * *",
		"*/15 * * * *",
	}
	starts := []time.Time{
		time.Date(2026, 3, 7, 5, 0, 0, 0, time.UTC),   // day before the New York spring forward
		time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC),   // New York spring forward
		time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC),  // New York fall back
		time.Date(2026, 3, 7, 11, 0, 0, 0, time.UTC),  // day before the Chicago spring forward
		time.Date(2026, 3, 8, 11, 0, 0, 0, time.UTC),  // Chicago spring forward
		time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC), // Chicago fall back
		time.Date(2026, 10, 2, 13, 0, 0, 0, time.UTC), // day before the Lord Howe spring shift
		time.Date(2026, 10, 4, 14, 0, 0, 0, time.UTC), // Lord Howe spring shift
		time.Date(2026, 4, 5, 13, 0, 0, 0, time.UTC),  // Lord Howe fall shift
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
	for _, zone := range zones {
		for _, expr := range exprs {
			for _, start := range starts {
				f.Add(zone, expr, start.Unix())
			}
		}
	}
	f.Fuzz(func(t *testing.T, zone, expr string, at int64) {
		if len(zone) > 64 || len(expr) > 64 {
			return
		}
		// Keep civil arithmetic in a range where every zone and the cron
		// parser behave: 1970 through 2100.
		if at < 0 || at > 4102444800 {
			return
		}
		loc, err := time.LoadLocation(zone)
		if err != nil {
			return
		}
		parsed, err := cron.ParseStandard(expr)
		if err != nil {
			return
		}
		spec, ok := parsed.(*cron.SpecSchedule)
		if !ok {
			return
		}
		schedule := zonedSchedule{parsed, loc}
		wildcardHour := spec.Hour&allHours == allHours
		// A schedule that selects every day must fire daily across a DST
		// transition; the zone needs no whole-day date-line shift for the
		// bound to hold, so gate it to the seeded zones.
		daily := spec.Dom&(1<<63) != 0 && spec.Dow&(1<<63) != 0 && seededZones[zone]
		var previousCivil time.Time
		cursor := time.Unix(at, 0).UTC()
		for i := range 8 {
			next := schedule.Next(cursor)
			if next.IsZero() {
				// A valid expression can have no activation within the
				// parser's five-year search, such as 30 February.
				return
			}
			if !next.After(cursor) {
				t.Fatalf("%s in %s: %s does not follow %s", expr, zone, next, cursor)
			}
			if next.Location() != time.UTC {
				t.Fatalf("%s in %s: slot %s is not a UTC instant", expr, zone, next)
			}
			if next.Second() != 0 || next.Nanosecond() != 0 {
				t.Fatalf("%s in %s: slot %s is not on a whole minute", expr, zone, next)
			}
			civil := next.In(loc)
			minuteMatch := 1<<uint(civil.Minute())&spec.Minute != 0
			if wildcardHour {
				if !minuteMatch {
					t.Fatalf("%s in %s: slot %s has minute %d, outside the expression", expr, zone, next, civil.Minute())
				}
			} else {
				hourMatch := 1<<uint(civil.Hour())&spec.Hour != 0
				if !minuteMatch || !hourMatch {
					// A fixed-hour expression may fire at the transition
					// instant when its selected time falls in the gap.
					_, before := next.Add(-time.Minute).In(loc).Zone()
					_, after := next.In(loc).Zone()
					if before == after {
						t.Fatalf("%s in %s: slot %s is outside the expression and not a transition", expr, zone, next)
					}
				}
				if i > 0 && sameCivilMinute(civil, previousCivil) {
					t.Fatalf("%s in %s: civil minute %s fired twice", expr, zone, civil)
				}
				if i > 0 && daily && next.Sub(cursor) > 26*time.Hour {
					t.Fatalf("%s in %s: a daily schedule skipped from %s to %s", expr, zone, cursor, next)
				}
			}
			previousCivil = civil
			if again := schedule.Next(cursor); !again.Equal(next) {
				t.Fatalf("%s in %s: unstable fire, %s then %s", expr, zone, next, again)
			}
			cursor = next
		}
	})
}
