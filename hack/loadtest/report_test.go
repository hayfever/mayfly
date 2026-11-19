package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	values := make([]time.Duration, 0, 100)
	for i := 1; i <= 100; i++ {
		values = append(values, time.Duration(i)*time.Millisecond)
	}
	cases := []struct {
		q    float64
		want time.Duration
	}{
		{0, time.Millisecond},
		{0.50, 50 * time.Millisecond},
		{0.95, 95 * time.Millisecond},
		{0.99, 99 * time.Millisecond},
		{1, 100 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := Percentile(values, tc.q); got != tc.want {
			t.Errorf("Percentile(q=%v) = %v, want %v", tc.q, got, tc.want)
		}
	}
	if got := Percentile(nil, 0.95); got != 0 {
		t.Errorf("Percentile(nil) = %v, want 0", got)
	}
	one := []time.Duration{7 * time.Millisecond}
	for _, q := range []float64{0, 0.5, 1} {
		if got := Percentile(one, q); got != one[0] {
			t.Errorf("Percentile(single, %v) = %v, want %v", q, got, one[0])
		}
	}
}

func TestPercentileIgnoresInputOrder(t *testing.T) {
	values := []time.Duration{5 * time.Millisecond, 1 * time.Millisecond, 3 * time.Millisecond}
	if got := Percentile(values, 0.50); got != 3*time.Millisecond {
		t.Errorf("p50 = %v, want 3ms", got)
	}
	if values[0] != 5*time.Millisecond {
		t.Errorf("Percentile mutated the input: %v", values)
	}
}

func TestSummaryErrorRateAndThroughput(t *testing.T) {
	summary := &Summary{
		Window:            10 * time.Second,
		SessionsFinalized: 25,
		Ops: map[string]*opStats{
			opSessionCreate: {
				Latencies: make([]time.Duration, 90),
				Failures:  make([]opFailure, 10),
			},
		},
	}
	if got, want := summary.Requests(), 100; got != want {
		t.Errorf("requests = %d, want %d", got, want)
	}
	if got, want := summary.Errors(), 10; got != want {
		t.Errorf("errors = %d, want %d", got, want)
	}
	if got, want := summary.ErrorRate(), 0.1; got != want {
		t.Errorf("error rate = %v, want %v", got, want)
	}
	if got, want := summary.Throughput(), 2.5; got != want {
		t.Errorf("throughput = %v, want %v", got, want)
	}

	summary.Window = 0
	if got := summary.Throughput(); got != 0 {
		t.Errorf("throughput with zero window = %v, want 0", got)
	}
	empty := &Summary{}
	if got := empty.ErrorRate(); got != 0 {
		t.Errorf("error rate without requests = %v, want 0", got)
	}
}

func TestRuntimeGrowth(t *testing.T) {
	summary := &Summary{Runtime: []RuntimeSample{
		{Goroutines: 100, HasGoroutines: true, RSS: 1000, HasRSS: true},
		{Goroutines: 150, HasGoroutines: true, RSS: 3000, HasRSS: true},
		{Goroutines: 120, HasGoroutines: true, RSS: 2000, HasRSS: true},
		{Err: "scrape failed"},
	}}
	growth := summary.growth()
	if got, want := growth.GoroutineSamples, 3; got != want {
		t.Errorf("goroutine samples = %d, want %d", got, want)
	}
	if growth.GoroutinesStart != 100 || growth.GoroutinesEnd != 120 || growth.GoroutinesPeak != 150 {
		t.Errorf("goroutine series = %d/%d/%d, want 100/120/150",
			growth.GoroutinesStart, growth.GoroutinesEnd, growth.GoroutinesPeak)
	}
	if got, want := growth.RSSSamples, 3; got != want {
		t.Errorf("rss samples = %d, want %d", got, want)
	}
	if growth.RSSStart != 1000 || growth.RSSEnd != 2000 || growth.RSSPeak != 3000 {
		t.Errorf("rss series = %d/%d/%d, want 1000/2000/3000",
			growth.RSSStart, growth.RSSEnd, growth.RSSPeak)
	}
}

func TestPrintSummaryReportsUnavailableRuntime(t *testing.T) {
	summary := &Summary{
		Mode:    "sessions",
		Workers: 1,
		Window:  time.Second,
		Ops: map[string]*opStats{
			opSessionCreate: {Latencies: []time.Duration{time.Millisecond}},
		},
		Runtime: []RuntimeSample{{Err: "403 forbidden"}},
	}
	var out bytes.Buffer
	printSummary(&out, summary)
	if !strings.Contains(out.String(), "unavailable (403 forbidden)") {
		t.Errorf("report does not explain the missing runtime samples:\n%s", out.String())
	}
}

func TestFailureLabelsAreCapped(t *testing.T) {
	summary := &Summary{Ops: map[string]*opStats{}}
	stats := &opStats{}
	for i := 0; i < 8; i++ {
		stats.Failures = append(stats.Failures, opFailure{
			operation: opSessionCreate,
			code:      400 + i,
			reason:    "invalid_request: bad",
		})
	}
	summary.Ops[opSessionCreate] = stats
	if got := failureLabels(summary); len(got) != 5 {
		t.Errorf("labels = %d, want 5", len(got))
	}
}
