package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// collectRuntime samples the control plane until ctx ends, then takes one
// final sample so the report includes the end of the window. A failed scrape
// is a sample with Err set, not a run failure.
func collectRuntime(ctx context.Context, client *apiClient, metricsURL string, interval time.Duration) []RuntimeSample {
	samples := []RuntimeSample{scrapeRuntime(ctx, client, metricsURL)}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			samples = append(samples, scrapeRuntime(final, client, metricsURL))
			cancel()
			return samples
		case <-ticker.C:
			samples = append(samples, scrapeRuntime(ctx, client, metricsURL))
		}
	}
}

// scrapeRuntime reads /metrics and extracts the two growth series.
func scrapeRuntime(ctx context.Context, client *apiClient, metricsURL string) RuntimeSample {
	sample := RuntimeSample{At: time.Now()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metricsURL, nil)
	if err != nil {
		sample.Err = err.Error()
		return sample
	}
	if client.token != "" {
		req.Header.Set("Authorization", "Bearer "+client.token)
	}
	resp, err := client.http.Do(req)
	if err != nil {
		sample.Err = err.Error()
		return sample
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		sample.Err = err.Error()
		return sample
	}
	if resp.StatusCode != http.StatusOK {
		sample.Err = fmt.Sprintf("GET %s: %s", metricsURL, errorText(resp.StatusCode, body))
		return sample
	}
	sample.Goroutines, sample.HasGoroutines, sample.RSS, sample.HasRSS = parseRuntimeMetrics(body)
	if !sample.HasGoroutines && !sample.HasRSS {
		sample.Err = fmt.Sprintf("GET %s: no go_goroutines or process_resident_memory_bytes", metricsURL)
	}
	return sample
}

// parseRuntimeMetrics reads the two unlabelled gauges from the Prometheus text
// format. It ignores every other series and malformed value.
func parseRuntimeMetrics(body []byte) (goroutines int, hasGoroutines bool, rss int64, hasRSS bool) {
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if brace := strings.IndexByte(name, '{'); brace >= 0 {
			name = name[:brace]
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			continue
		}
		switch name {
		case "go_goroutines":
			goroutines = int(parsed)
			hasGoroutines = true
		case "process_resident_memory_bytes":
			rss = int64(parsed)
			hasRSS = true
		}
	}
	return goroutines, hasGoroutines, rss, hasRSS
}
