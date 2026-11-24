package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hayfever/mayfly/internal/releasegate"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func TestDecodeRuns(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"page wrapper", `{"total_count":1,"workflow_runs":[{"id":1,"name":"ci"}]}`, 1},
		{"bare array", `[{"id":1},{"id":2}]`, 2},
		{"newline delimited", "{\"id\":1}\n{\"id\":2}\n", 2},
		{"empty", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeRuns([]byte(tt.input))
			if err != nil {
				t.Fatalf("decodeRuns: %v", err)
			}
			if len(got) != tt.want {
				t.Fatalf("decoded %d runs, want %d", len(got), tt.want)
			}
		})
	}
}

func TestDecodeJobs(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"page wrapper", `{"total_count":1,"jobs":[{"id":1,"name":"gate"}]}`, 1},
		{"bare array", `[{"id":1},{"id":2}]`, 2},
		{"newline delimited", "{\"id\":1}\n{\"id\":2}\n", 2},
		{"empty", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeJobs([]byte(tt.input))
			if err != nil {
				t.Fatalf("decodeJobs: %v", err)
			}
			if len(got) != tt.want {
				t.Fatalf("decoded %d jobs, want %d", len(got), tt.want)
			}
		})
	}
}

func TestRunPassesAndFails(t *testing.T) {
	dir := t.TempDir()
	runsPath := filepath.Join(dir, "runs.json")
	jobsPath := filepath.Join(dir, "jobs.json")
	if err := os.WriteFile(runsPath, []byte(`{"workflow_runs":`+testRuns()+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobsPath, []byte(testJobs()), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := run(testSHA, runsPath, jobsPath, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "PASS") {
		t.Fatalf("output does not pass:\n%s", out.String())
	}

	if err := os.WriteFile(runsPath, []byte(`{"workflow_runs":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(testSHA, runsPath, jobsPath, &out); err == nil {
		t.Fatalf("empty history passed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "FAIL") || !strings.Contains(out.String(), "no ci workflow run") {
		t.Fatalf("output does not explain the failure:\n%s", out.String())
	}
}

func testRuns() string {
	runs := []releasegate.Run{
		{ID: 10, Name: "ci", Path: ".github/workflows/ci.yml", HeadSHA: testSHA, Status: "completed", Conclusion: "success", RunAttempt: 1},
		{ID: 20, Name: "e2e-stability", Path: ".github/workflows/e2e-stability.yml", HeadSHA: testSHA, Status: "completed", Conclusion: "success", RunAttempt: 1},
	}
	data, err := json.Marshal(runs)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func testJobs() string {
	jobs := []releasegate.Job{{RunID: 20, Name: "gate", RunAttempt: 1, Conclusion: "success"}}
	for n := 1; n <= releasegate.RequiredStabilityRuns; n++ {
		queue := "bbolt"
		if n%2 == 0 {
			queue = "redis"
		}
		jobs = append(jobs, releasegate.Job{
			RunID:      20,
			Name:       fmt.Sprintf("e2e (%d/%s)", n, queue),
			RunAttempt: 1,
			Conclusion: "success",
		})
	}
	data, err := json.Marshal(jobs)
	if err != nil {
		panic(err)
	}
	return string(data)
}
