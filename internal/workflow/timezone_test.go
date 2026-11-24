package workflow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/hayfever/mayfly/internal/project"
)

// nextSlots returns the next n activations of expr in zone, starting after
// from. It exercises the same wrapper the scheduler installs.
func nextSlots(t *testing.T, expr, zone string, from time.Time, n int) []time.Time {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	schedule := newZonedSchedule(t, expr, loc)
	out := make([]time.Time, 0, n)
	cursor := from
	for range n {
		next := schedule.Next(cursor)
		if next.IsZero() {
			t.Fatalf("%s in %s: no activation after %s", expr, zone, cursor)
		}
		if !next.After(cursor) {
			t.Fatalf("%s in %s: %s is not after %s", expr, zone, next, cursor)
		}
		out = append(out, next)
		cursor = next
	}
	return out
}

func newZonedSchedule(t *testing.T, expr string, loc *time.Location) zonedSchedule {
	t.Helper()
	parsed, err := cron.ParseStandard(expr)
	if err != nil {
		t.Fatal(err)
	}
	return zonedSchedule{parsed, loc}
}

func localAt(t *testing.T, zone string, year int, month time.Month, day, hour, minute int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(year, month, day, hour, minute, 0, 0, loc)
}

func wantSlots(t *testing.T, got []time.Time, want ...time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("slots = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("slot %d = %s, want %s (all %v)", i, got[i], want[i], got)
		}
	}
}

// TestChicagoWallClockAcrossDST checks the issue's first acceptance case:
// 07:00 America/Chicago stays 07:00 local through both transitions, so the
// UTC instant moves by an hour.
func TestChicagoWallClockAcrossDST(t *testing.T) {
	spring := nextSlots(t, "0 7 * * *", "America/Chicago", time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC), 2)
	wantSlots(t, spring,
		time.Date(2026, 3, 7, 13, 0, 0, 0, time.UTC), // 07:00 CST
		time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC), // 07:00 CDT
	)

	fall := nextSlots(t, "0 7 * * *", "America/Chicago", time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), 2)
	wantSlots(t, fall,
		time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), // 07:00 CDT
		time.Date(2026, 11, 1, 13, 0, 0, 0, time.UTC),  // 07:00 CST
	)
}

// TestNewYorkSpringForwardFiresAtTransition checks the Vixie rule: 02:30 does
// not exist on the transition day, so the schedule fires once at the
// transition instant (03:00 EDT) and resumes its normal time the next day.
func TestNewYorkSpringForwardFiresAtTransition(t *testing.T) {
	got := nextSlots(t, "30 2 * * *", "America/New_York", time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC), 3)
	wantSlots(t, got,
		time.Date(2026, 3, 7, 7, 30, 0, 0, time.UTC), // 02:30 EST
		time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC),  // 03:00 EDT, the transition
		time.Date(2026, 3, 9, 6, 30, 0, 0, time.UTC), // 02:30 EDT
	)
}

// TestNewYorkFallBackFiresOnce checks the Vixie rule: 01:30 occurs twice, and
// the schedule fires only at the first occurrence.
func TestNewYorkFallBackFiresOnce(t *testing.T) {
	got := nextSlots(t, "30 1 * * *", "America/New_York", time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), 3)
	wantSlots(t, got,
		time.Date(2026, 10, 31, 5, 30, 0, 0, time.UTC), // 01:30 EDT
		time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC),  // 01:30 EDT, the first copy
		time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC),  // 01:30 EST, the next day
	)

	// Between the two copies, the next slot is the next day: the second copy
	// is not a slot.
	between := nextSlots(t, "30 1 * * *", "America/New_York", time.Date(2026, 11, 1, 5, 45, 0, 0, time.UTC), 1)
	wantSlots(t, between, time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC))
}

// TestMostRecentSlotUsesFirstFallBack checks that catch-up and a late tick
// keep the first fall-back instant and never replay the second one.
func TestMostRecentSlotUsesFirstFallBack(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)
	for _, now := range []time.Time{
		time.Date(2026, 11, 1, 5, 45, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 6, 45, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC),
	} {
		slot, ok := mostRecentSlot("30 1 * * *", now, loc)
		if !ok {
			t.Fatalf("%s: no slot", now)
		}
		if !slot.Equal(want) {
			t.Fatalf("%s: slot = %s, want %s", now, slot, want)
		}
		if got := tickSlot("30 1 * * *", now, loc); !got.Equal(want) {
			t.Fatalf("%s: tick slot = %s, want %s", now, got, want)
		}
	}
}

// TestMostRecentSlotUsesGapTransition checks that a missed spring-forward fire
// is recorded at the transition instant.
func TestMostRecentSlotUsesGapTransition(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	slot, ok := mostRecentSlot("30 2 * * *", time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC), loc)
	if !ok {
		t.Fatal("no slot")
	}
	if want := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC); !slot.Equal(want) {
		t.Fatalf("slot = %s, want %s", slot, want)
	}
}

// TestHourlyAcrossTransitions checks that an hourly expression fires at both
// offsets of a repeated hour, and that the spring-forward day has one fire per
// existing local hour.
func TestHourlyAcrossTransitions(t *testing.T) {
	fall := nextSlots(t, "0 * * * *", "America/New_York", time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), 3)
	wantSlots(t, fall,
		time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC), // 01:00 EDT
		time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC), // 01:00 EST
		time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC), // 02:00 EST
	)
	if fall[0].Equal(fall[1]) {
		t.Fatal("the repeated hour shares one slot key")
	}

	spring := nextSlots(t, "0 * * * *", "America/New_York", time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), 3)
	wantSlots(t, spring,
		time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC), // 01:00 EST
		time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC), // 03:00 EDT
		time.Date(2026, 3, 8, 8, 0, 0, 0, time.UTC), // 04:00 EDT
	)
}

// TestEveryMinuteIsStrictlyIncreasingUTC checks that a sub-hour expression
// keeps one activation per UTC minute across both transitions.
func TestEveryMinuteIsStrictlyIncreasingUTC(t *testing.T) {
	for _, from := range []time.Time{
		time.Date(2026, 3, 8, 6, 58, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 5, 58, 0, 0, time.UTC),
	} {
		got := nextSlots(t, "* * * * *", "America/New_York", from, 4)
		for i, slot := range got {
			want := from.Add(time.Duration(i+1) * time.Minute)
			if !slot.Equal(want) {
				t.Fatalf("every-minute slot %d after %s = %s, want %s", i, from, slot, want)
			}
		}
	}
}

// TestLordHoweHalfHourShift checks a zone that shifts by 30 minutes, not 60.
func TestLordHoweHalfHourShift(t *testing.T) {
	// DST starts 2026-10-04 at 02:00 (+10:30 becomes +11:00), so 02:15 is in
	// the 30-minute gap and fires at the transition; 02:45 exists normally.
	gap := nextSlots(t, "15 2 * * *", "Australia/Lord_Howe", localAt(t, "Australia/Lord_Howe", 2026, 10, 4, 0, 0), 2)
	wantSlots(t, gap,
		time.Date(2026, 10, 3, 15, 30, 0, 0, time.UTC), // 02:30 +11, the transition
		time.Date(2026, 10, 4, 15, 15, 0, 0, time.UTC), // 02:15 +11 on the 5th
	)

	exist := nextSlots(t, "45 2 * * *", "Australia/Lord_Howe", localAt(t, "Australia/Lord_Howe", 2026, 10, 4, 0, 0), 1)
	wantSlots(t, exist, time.Date(2026, 10, 3, 15, 45, 0, 0, time.UTC)) // 02:45 +11 on the 4th

	// DST ends 2026-04-05 at 02:00 (+11:00 becomes +10:30), so 01:45 occurs
	// twice, 30 minutes apart. The schedule fires at the first only.
	once := nextSlots(t, "45 1 * * *", "Australia/Lord_Howe", localAt(t, "Australia/Lord_Howe", 2026, 4, 5, 0, 0), 2)
	wantSlots(t, once,
		time.Date(2026, 4, 4, 14, 45, 0, 0, time.UTC), // 01:45 +11
		time.Date(2026, 4, 5, 15, 15, 0, 0, time.UTC), // 01:45 +10:30 on the 6th
	)
}

// TestSchedulesExposeTimezone checks the API description carries the zone and
// omits it for a UTC schedule.
func TestSchedulesExposeTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{bundle: &project.Bundle{Schedules: []project.Schedule{
		{Name: "chicago", Agent: "watcher", Cron: "0 7 * * *", Timezone: "America/Chicago", Location: loc, Source: "pages"},
		{Name: "utc", Agent: "watcher", Cron: "0 7 * * *", Location: time.UTC, Source: "pages"},
	}}}
	got := runner.Schedules()
	if len(got) != 2 {
		t.Fatalf("schedules = %d", len(got))
	}
	if got[0].Timezone != "America/Chicago" {
		t.Fatalf("timezone = %q", got[0].Timezone)
	}
	if got[1].Timezone != "" {
		t.Fatalf("UTC timezone = %q, want empty", got[1].Timezone)
	}
	raw, err := json.Marshal(got[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "timezone") {
		t.Fatalf("UTC schedule JSON = %s, want no timezone key", raw)
	}
}
