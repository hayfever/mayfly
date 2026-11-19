package main

import "testing"

func TestParseRuntimeMetrics(t *testing.T) {
	body := []byte(`# HELP go_goroutines Number of goroutines.
# TYPE go_goroutines gauge
go_goroutines 42
go_goroutines{instance="test"} 43
process_resident_memory_bytes 1.048576e+08
mayfly_sessions{state="running"} 2
`)
	goroutines, hasGoroutines, rss, hasRSS := parseRuntimeMetrics(body)
	if !hasGoroutines || goroutines != 43 {
		t.Errorf("goroutines = %d, present %v, want 43, true", goroutines, hasGoroutines)
	}
	if !hasRSS || rss != 104857600 {
		t.Errorf("rss = %d, present %v, want 104857600, true", rss, hasRSS)
	}
}

func TestParseRuntimeMetricsMissing(t *testing.T) {
	goroutines, hasGoroutines, rss, hasRSS := parseRuntimeMetrics([]byte("mayfly_build_info 1\n"))
	if hasGoroutines || hasRSS || goroutines != 0 || rss != 0 {
		t.Errorf("got %d/%v and %d/%v, want no runtime series", goroutines, hasGoroutines, rss, hasRSS)
	}
}

func TestParseRuntimeMetricsIgnoresMalformedValues(t *testing.T) {
	goroutines, hasGoroutines, _, _ := parseRuntimeMetrics([]byte("go_goroutines nope\n"))
	if hasGoroutines || goroutines != 0 {
		t.Errorf("got %d/%v, want the malformed value ignored", goroutines, hasGoroutines)
	}
}
