// Command release-gate decides whether a commit passed the release gates.
//
// It reads GitHub API payloads and makes no network call, so the release
// workflow can collect them with `gh api` and a test can use a recorded or
// synthetic response.
//
// Usage:
//
//	go run ./hack/release-gate -sha SHA -runs runs.json -jobs jobs.json
//
// runs.json is the workflow-runs response ({"workflow_runs": [...]}) or the
// bare array. jobs.json is the flattened jobs array ({"jobs": [...]}) or the
// bare array. Both accept "-" for stdin. The command exits 1 when the gate
// fails and prints one reason per line.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hayfever/mayfly/internal/releasegate"
)

func main() {
	sha := flag.String("sha", "", "commit SHA to gate")
	runsPath := flag.String("runs", "", "path of the workflow-runs JSON (or -)")
	jobsPath := flag.String("jobs", "", "path of the flattened jobs JSON (or -)")
	flag.Parse()

	if err := run(*sha, *runsPath, *jobsPath, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "release-gate:", err)
		os.Exit(1)
	}
}

func run(sha, runsPath, jobsPath string, out io.Writer) error {
	if sha == "" {
		return errors.New("-sha is required")
	}
	if runsPath == "" || jobsPath == "" {
		return errors.New("-runs and -jobs are required")
	}
	runsPayload, err := readPayload(runsPath)
	if err != nil {
		return err
	}
	jobsPayload, err := readPayload(jobsPath)
	if err != nil {
		return err
	}
	runs, err := decodeRuns(runsPayload)
	if err != nil {
		return fmt.Errorf("%s: %w", runsPath, err)
	}
	jobs, err := decodeJobs(jobsPayload)
	if err != nil {
		return fmt.Errorf("%s: %w", jobsPath, err)
	}

	decision := releasegate.Decide(sha, runs, jobs)
	if _, err := fmt.Fprintf(out, "sha: %s\nruns: %d\njobs: %d\n", sha, len(runs), len(jobs)); err != nil {
		return err
	}
	if decision.OK {
		_, err := fmt.Fprintf(out, "PASS: e2e-stability run %d (attempt %d), ci, and the release gate\n",
			decision.StabilityRunID, decision.StabilityAttempt)
		return err
	}
	if _, err := fmt.Fprintf(out, "FAIL: %s did not pass the release gate\n", sha); err != nil {
		return err
	}
	for _, reason := range decision.Reasons {
		if _, err := fmt.Fprintf(out, "  - %s\n", reason); err != nil {
			return err
		}
	}
	return fmt.Errorf("%d release gate check(s) failed", len(decision.Reasons))
}

func readPayload(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// decodeRuns accepts the paginated workflow-runs response, the bare array, or
// newline-delimited run objects (one per `gh api --paginate --jq` line).
func decodeRuns(data []byte) ([]releasegate.Run, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var runs []releasegate.Run
		if err := json.Unmarshal(trimmed, &runs); err != nil {
			return nil, err
		}
		return runs, nil
	}
	var page struct {
		WorkflowRuns []releasegate.Run `json:"workflow_runs"`
	}
	if err := json.Unmarshal(trimmed, &page); err == nil {
		return page.WorkflowRuns, nil
	}
	return decodeNDJSON[releasegate.Run](trimmed)
}

// decodeJobs accepts the jobs response, the bare array, or newline-delimited
// job objects.
func decodeJobs(data []byte) ([]releasegate.Job, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var jobs []releasegate.Job
		if err := json.Unmarshal(trimmed, &jobs); err != nil {
			return nil, err
		}
		return jobs, nil
	}
	var page struct {
		Jobs []releasegate.Job `json:"jobs"`
	}
	if err := json.Unmarshal(trimmed, &page); err == nil {
		return page.Jobs, nil
	}
	return decodeNDJSON[releasegate.Job](trimmed)
}

func decodeNDJSON[T any](data []byte) ([]T, error) {
	var out []T
	for line := range bytes.Lines(data) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var value T
		if err := json.Unmarshal(line, &value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}
