package main

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Summary is one completed harness run.
type Summary struct {
	Mode    string
	Workers int
	Window  time.Duration

	Ops map[string]*opStats

	SessionsCreated   int
	SessionsFinalized int
	SessionCompleted  int
	SessionFailed     int
	SessionExpired    int
	Lifecycle         []time.Duration

	WaitFailures    []string
	JobFailures     []string
	CleanupFailures []string

	LateSessionsCleaned int
	LateSessionsLeft    int

	Runtime []RuntimeSample
}

// opStats holds the primary request latencies and failures for one operation.
type opStats struct {
	Latencies []time.Duration
	Failures  []opFailure
}

// RuntimeSample is one scrape of the control plane's runtime metrics.
type RuntimeSample struct {
	At            time.Time
	Goroutines    int
	RSS           int64
	HasGoroutines bool
	HasRSS        bool
	Err           string
}

// Requests counts primary requests, successful or not.
func (s *Summary) Requests() int {
	total := 0
	for _, stats := range s.Ops {
		total += len(stats.Latencies) + len(stats.Failures)
	}
	return total
}

// Errors counts failed primary requests.
func (s *Summary) Errors() int {
	total := 0
	for _, stats := range s.Ops {
		total += len(stats.Failures)
	}
	return total
}

// ErrorRate is the fraction of failed primary requests.
func (s *Summary) ErrorRate() float64 {
	if requests := s.Requests(); requests > 0 {
		return float64(s.Errors()) / float64(requests)
	}
	return 0
}

// Throughput is the finalized sessions per second over the run window.
func (s *Summary) Throughput() float64 {
	if s.Window <= 0 {
		return 0
	}
	return float64(s.SessionsFinalized) / s.Window.Seconds()
}

// Percentile returns the nearest-rank percentile of unsorted durations. The
// value is the smallest sample at or above the requested rank, so the result
// is always one real observation.
func Percentile(values []time.Duration, q float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return percentileSorted(sorted, q)
}

func percentileSorted(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// runtimeGrowth summarizes the goroutine and resident-memory series. Samples
// that failed, or that lacked a series, are skipped for that series.
type runtimeGrowth struct {
	GoroutineSamples int
	GoroutinesStart  int
	GoroutinesEnd    int
	GoroutinesPeak   int

	RSSSamples int
	RSSStart   int64
	RSSEnd     int64
	RSSPeak    int64
}

func (s *Summary) growth() runtimeGrowth {
	var g runtimeGrowth
	for _, sample := range s.Runtime {
		if sample.HasGoroutines {
			if g.GoroutineSamples == 0 {
				g.GoroutinesStart = sample.Goroutines
			}
			g.GoroutinesEnd = sample.Goroutines
			if sample.Goroutines > g.GoroutinesPeak {
				g.GoroutinesPeak = sample.Goroutines
			}
			g.GoroutineSamples++
		}
		if sample.HasRSS {
			if g.RSSSamples == 0 {
				g.RSSStart = sample.RSS
			}
			g.RSSEnd = sample.RSS
			if sample.RSS > g.RSSPeak {
				g.RSSPeak = sample.RSS
			}
			g.RSSSamples++
		}
	}
	return g
}

// printSummary writes the human report.
func printSummary(w io.Writer, s *Summary) {
	if s == nil {
		_, _ = fmt.Fprintln(w, "loadtest: no summary")
		return
	}
	_, _ = fmt.Fprintf(w, "loadtest: mode %s, %d workers, window %s\n\n", s.Mode, s.Workers, roundDuration(s.Window))

	_, _ = fmt.Fprintln(w, "requests")
	if s.Requests() == 0 {
		_, _ = fmt.Fprintln(w, "  none")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  operation\trequests\terrors\tp50\tp95\tp99\tmax")
		for _, name := range sortedOps(s.Ops) {
			stats := s.Ops[name]
			latencies := append([]time.Duration(nil), stats.Latencies...)
			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
			_, _ = fmt.Fprintf(tw, "  %s\t%d\t%d\t%s\t%s\t%s\t%s\n",
				name,
				len(stats.Latencies)+len(stats.Failures),
				len(stats.Failures),
				latencyAt(latencies, 0.50),
				latencyAt(latencies, 0.95),
				latencyAt(latencies, 0.99),
				latencyAt(latencies, 1),
			)
		}
		_ = tw.Flush()
	}
	_, _ = fmt.Fprintf(w, "error rate: %.2f%% (%d of %d)\n", s.ErrorRate()*100, s.Errors(), s.Requests())
	if labels := failureLabels(s); len(labels) > 0 {
		_, _ = fmt.Fprintf(w, "errors: %s\n", strings.Join(labels, "; "))
	}

	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "sessions")
	_, _ = fmt.Fprintf(w, "  created %d, finalized %d, throughput %.2f/s\n",
		s.SessionsCreated, s.SessionsFinalized, s.Throughput())
	_, _ = fmt.Fprintf(w, "  outcomes: completed %d, failed %d, expired %d\n",
		s.SessionCompleted, s.SessionFailed, s.SessionExpired)
	if len(s.Lifecycle) > 0 {
		_, _ = fmt.Fprintf(w, "  lifecycle: p50 %s, p95 %s, p99 %s\n",
			roundDuration(Percentile(s.Lifecycle, 0.50)),
			roundDuration(Percentile(s.Lifecycle, 0.95)),
			roundDuration(Percentile(s.Lifecycle, 0.99)))
	}
	if s.LateSessionsCleaned > 0 || s.LateSessionsLeft > 0 {
		_, _ = fmt.Fprintf(w, "  late sessions cleaned up: %d, left behind: %d\n",
			s.LateSessionsCleaned, s.LateSessionsLeft)
	}

	printFailures(w, "wait failures", s.WaitFailures)
	printFailures(w, "job failures", s.JobFailures)
	printFailures(w, "cleanup failures", s.CleanupFailures)

	_, _ = fmt.Fprintln(w)
	printRuntime(w, s)
}

func printRuntime(w io.Writer, s *Summary) {
	g := s.growth()
	if g.GoroutineSamples == 0 && g.RSSSamples == 0 {
		reason := "no runtime samples"
		for i := len(s.Runtime) - 1; i >= 0; i-- {
			if s.Runtime[i].Err != "" {
				reason = s.Runtime[i].Err
				break
			}
		}
		_, _ = fmt.Fprintf(w, "control plane runtime: unavailable (%s)\n", reason)
		return
	}
	_, _ = fmt.Fprintf(w, "control plane runtime (%d samples)\n", len(s.Runtime))
	if g.GoroutineSamples > 0 {
		_, _ = fmt.Fprintf(w, "  go_goroutines: start %d, end %d, peak %d, change %+d\n",
			g.GoroutinesStart, g.GoroutinesEnd, g.GoroutinesPeak, g.GoroutinesEnd-g.GoroutinesStart)
	}
	if g.RSSSamples > 0 {
		_, _ = fmt.Fprintf(w, "  resident memory: start %s, end %s, peak %s, change %+.1f MiB\n",
			mib(g.RSSStart), mib(g.RSSEnd), mib(g.RSSPeak), float64(g.RSSEnd-g.RSSStart)/(1<<20))
	}
	failed := 0
	for _, sample := range s.Runtime {
		if sample.Err != "" {
			failed++
		}
	}
	if failed > 0 {
		_, _ = fmt.Fprintf(w, "  %d of %d scrapes failed; the last error was: %s\n", failed, len(s.Runtime), lastRuntimeError(s.Runtime))
	}
}

func lastRuntimeError(samples []RuntimeSample) string {
	for i := len(samples) - 1; i >= 0; i-- {
		if samples[i].Err != "" {
			return samples[i].Err
		}
	}
	return "unknown"
}

// failureLabels lists the most common error codes, capped at five.
func failureLabels(s *Summary) []string {
	counts := map[string]int{}
	for _, stats := range s.Ops {
		for _, failure := range stats.Failures {
			counts[failureLabel(failure)]++
		}
	}
	return topCounts(counts, 5)
}

func failureLabel(failure opFailure) string {
	if failure.code == 0 {
		return "transport: " + short(failure.reason, 80)
	}
	kind := failure.reason
	if i := strings.IndexByte(kind, ':'); i >= 0 {
		kind = kind[:i]
	}
	return fmt.Sprintf("%d %s", failure.code, short(kind, 40))
}

func printFailures(w io.Writer, name string, failures []string) {
	if len(failures) == 0 {
		return
	}
	counts := map[string]int{}
	for _, failure := range failures {
		counts[short(failure, 120)]++
	}
	_, _ = fmt.Fprintf(w, "  %s: %d (%s)\n", name, len(failures), strings.Join(topCounts(counts, 3), "; "))
}

func topCounts(counts map[string]int, limit int) []string {
	type entry struct {
		label string
		count int
	}
	entries := make([]entry, 0, len(counts))
	for label, count := range counts {
		entries = append(entries, entry{label: label, count: count})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].label < entries[j].label
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, fmt.Sprintf("%s x%d", e.label, e.count))
	}
	return out
}

func sortedOps(ops map[string]*opStats) []string {
	names := make([]string, 0, len(ops))
	for name := range ops {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// latencyAt formats one percentile, or a dash when no request succeeded.
func latencyAt(sorted []time.Duration, q float64) string {
	if len(sorted) == 0 {
		return "-"
	}
	return roundDuration(percentileSorted(sorted, q))
}

func roundDuration(d time.Duration) string {
	return d.Round(time.Microsecond).String()
}

func mib(bytes int64) string {
	return fmt.Sprintf("%.1f MiB", float64(bytes)/(1<<20))
}

func short(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
