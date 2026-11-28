package releasegate

import (
	"strings"
	"testing"
)

const (
	testSHA = "0123456789abcdef0123456789abcdef01234567"
	// otherSHASHA is a commit that is not testSHA.
	otherSHASHA = "ffffffffffffffffffffffffffffffffffffffff"
)

func ciRun(conclusion string) Run {
	return Run{
		ID:         10,
		Name:       "ci",
		Path:       ".github/workflows/ci.yml",
		HeadSHA:    testSHA,
		Event:      "push",
		Status:     "completed",
		Conclusion: conclusion,
		RunAttempt: 1,
		CreatedAt:  "2026-11-23T10:00:00Z",
	}
}

func ciAttempt(attempt int, conclusion string) Run {
	r := ciRun(conclusion)
	r.RunAttempt = attempt
	return r
}

func stabilityRun(id int64, attempt int, conclusion string) Run {
	return Run{
		ID:         id,
		Name:       "e2e-stability",
		Path:       ".github/workflows/e2e-stability.yml",
		HeadSHA:    testSHA,
		Event:      "workflow_dispatch",
		Status:     "completed",
		Conclusion: conclusion,
		RunAttempt: attempt,
		CreatedAt:  "2026-11-23T09:00:00Z",
	}
}

// stabilityRunNamed returns a run that names headSHA and title, as a dispatch
// on another ref does: the run ran there, and the run-name names the target.
func stabilityRunNamed(id int64, attempt int, conclusion, headSHA, title string) Run {
	r := stabilityRun(id, attempt, conclusion)
	r.HeadSHA = headSHA
	r.DisplayTitle = title
	return r
}

// stabilityJobs returns the jobs of one complete e2e-stability attempt: the
// plan, the 20 alternating e2e runs, the unit stress, and the gate.
func stabilityJobs(runID int64, attempt int, runs int) []Job {
	jobs := []Job{{RunID: runID, Name: "plan", RunAttempt: attempt, Status: "completed", Conclusion: "success"}}
	queues := []string{"bbolt", "redis"}
	for n := 1; n <= runs; n++ {
		jobs = append(jobs, Job{
			RunID:      runID,
			Name:       "e2e (" + itoa(n) + "/" + queues[(n-1)%2] + ")",
			RunAttempt: attempt,
			Status:     "completed",
			Conclusion: "success",
		})
	}
	jobs = append(jobs,
		Job{RunID: runID, Name: "unit stress", RunAttempt: attempt, Status: "completed", Conclusion: "success"},
		Job{RunID: runID, Name: "gate", RunAttempt: attempt, Status: "completed", Conclusion: "success"},
	)
	return jobs
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name       string
		sha        string
		runs       []Run
		jobs       []Job
		wantOK     bool
		wantReason string
	}{
		{
			name:   "green ci and the full stability matrix pass",
			sha:    testSHA,
			runs:   []Run{ciRun("success"), stabilityRun(20, 1, "success")},
			jobs:   stabilityJobs(20, 1, RequiredStabilityRuns),
			wantOK: true,
		},
		{
			// The image scan runs as `preflight-scan (<image>)` in the same
			// stability run. Only `e2e (<run>/<queue>)` may count toward the
			// 20 runs, and that job's result gates the run separately.
			name: "preflight-scan jobs do not count as e2e runs",
			sha:  testSHA,
			runs: []Run{ciRun("success"), stabilityRun(20, 1, "success")},
			jobs: append(stabilityJobs(20, 1, RequiredStabilityRuns),
				Job{RunID: 20, Name: "preflight-scan (mayflyd)", RunAttempt: 1, Status: "completed", Conclusion: "success"},
				Job{RunID: 20, Name: "preflight-scan (mayfly-agent)", RunAttempt: 1, Status: "completed", Conclusion: "success"},
			),
			wantOK: true,
		},
		{
			name:       "no stability run",
			sha:        testSHA,
			runs:       []Run{ciRun("success")},
			wantReason: "no e2e-stability workflow run exists",
		},
		{
			name:       "stability run used five runs",
			sha:        testSHA,
			runs:       []Run{ciRun("success"), stabilityRun(20, 1, "success")},
			jobs:       stabilityJobs(20, 1, 5),
			wantReason: "has 5 of 20 e2e jobs",
		},
		{
			name:       "stability run failed",
			sha:        testSHA,
			runs:       []Run{ciRun("success"), stabilityRun(20, 1, "failure")},
			jobs:       stabilityJobs(20, 1, RequiredStabilityRuns),
			wantReason: "the e2e-stability workflow did not succeed",
		},
		{
			name:       "stability run cancelled",
			sha:        testSHA,
			runs:       []Run{ciRun("success"), stabilityRun(20, 1, "cancelled")},
			jobs:       stabilityJobs(20, 1, RequiredStabilityRuns),
			wantReason: "the e2e-stability workflow did not succeed",
		},
		{
			name:       "ci failed",
			sha:        testSHA,
			runs:       []Run{ciRun("failure"), stabilityRun(20, 1, "success")},
			jobs:       stabilityJobs(20, 1, RequiredStabilityRuns),
			wantReason: "the ci workflow did not succeed",
		},
		{
			name:       "ci missing",
			sha:        testSHA,
			runs:       []Run{stabilityRun(20, 1, "success")},
			jobs:       stabilityJobs(20, 1, RequiredStabilityRuns),
			wantReason: "no ci workflow run exists",
		},
		{
			name:       "gate job failed",
			sha:        testSHA,
			runs:       []Run{ciRun("success"), stabilityRun(20, 1, "success")},
			jobs:       gateJobFailed(20, 1),
			wantReason: "has a failure gate job",
		},
		{
			name:       "e2e job failed",
			sha:        testSHA,
			runs:       []Run{ciRun("success"), stabilityRun(20, 1, "success")},
			jobs:       e2eJobFailed(20, 1),
			wantReason: "has failed e2e jobs: 4 (failure)",
		},
		{
			name:       "stability run belongs to another commit",
			sha:        testSHA,
			runs:       []Run{ciRun("success"), otherSHA(stabilityRun(20, 1, "success"))},
			jobs:       stabilityJobs(20, 1, RequiredStabilityRuns),
			wantReason: "no e2e-stability workflow run exists",
		},
		{
			// #52: attempt 1 failed on an infrastructure glitch, the rerun
			// passed. The list API reports the latest attempt, and the jobs API
			// with filter=all carries both attempts.
			name: "failed first attempt then a passing rerun",
			sha:  testSHA,
			runs: []Run{ciRun("success"), stabilityRun(20, 2, "success")},
			jobs: append(
				stabilityJobs(20, 1, RequiredStabilityRuns),
				stabilityJobs(20, 2, RequiredStabilityRuns)...,
			),
			wantOK: true,
		},
		{
			// The other API shape: one entry per attempt, same run id.
			name: "separate entries per attempt",
			sha:  testSHA,
			runs: []Run{
				ciRun("success"),
				stabilityRun(20, 1, "failure"),
				stabilityRun(20, 2, "success"),
			},
			jobs: append(
				stabilityJobs(20, 1, RequiredStabilityRuns),
				stabilityJobs(20, 2, RequiredStabilityRuns)...,
			),
			wantOK: true,
		},
		{
			// The documented command dispatches e2e-stability on main while it
			// names the release commit in the run-name. The run counts.
			name: "dispatch on main names the gated sha",
			sha:  testSHA,
			runs: []Run{
				ciRun("success"),
				stabilityRunNamed(20, 1, "success", otherSHASHA, "e2e-stability runs=20 sha="+testSHA),
			},
			jobs:   stabilityJobs(20, 1, RequiredStabilityRuns),
			wantOK: true,
		},
		{
			// #52 applies to ci too: attempt 1 passed and the rerun failed.
			name: "ci green attempt survived a failed rerun",
			sha:  testSHA,
			runs: []Run{
				ciAttempt(1, "success"),
				ciAttempt(2, "failure"),
				stabilityRun(20, 1, "success"),
			},
			jobs:   stabilityJobs(20, 1, RequiredStabilityRuns),
			wantOK: true,
		},
		{
			// #52 with the API shape that reports only the latest attempt: the
			// rerun failed, attempt 1 passed, and both attempts' jobs exist.
			name: "latest stability attempt failed but an earlier attempt passed",
			sha:  testSHA,
			runs: []Run{
				ciRun("success"),
				stabilityRun(20, 2, "failure"),
				stabilityRun(20, 1, "success"),
			},
			jobs: append(
				stabilityJobs(20, 1, RequiredStabilityRuns),
				stabilityJobs(20, 2, RequiredStabilityRuns)...,
			),
			wantOK: true,
		},
		{
			name: "run name with another sha fails",
			sha:  testSHA,
			runs: []Run{
				ciRun("success"),
				stabilityRunNamed(20, 1, "success", otherSHASHA,
					"e2e-stability runs=20 sha=ffffffffffffffffffffffffffffffffffffffff"),
			},
			wantReason: "no e2e-stability workflow run exists",
		},
		{
			name: "run name with a short sha fails",
			sha:  testSHA,
			runs: []Run{
				ciRun("success"),
				stabilityRunNamed(20, 1, "success", otherSHASHA,
					"e2e-stability runs=20 sha="+testSHA[:16]),
			},
			wantReason: "no e2e-stability workflow run exists",
		},
		{
			name: "run name with a longer hex string fails",
			sha:  testSHA,
			runs: []Run{
				ciRun("success"),
				stabilityRunNamed(20, 1, "success", otherSHASHA,
					"e2e-stability runs=20 sha="+testSHA+"0"),
			},
			wantReason: "no e2e-stability workflow run exists",
		},
		{
			name: "run name without a token boundary fails",
			sha:  testSHA,
			runs: []Run{
				ciRun("success"),
				stabilityRunNamed(20, 1, "success", otherSHASHA,
					"e2e-stability runs=20 xsha="+testSHA),
			},
			wantReason: "no e2e-stability workflow run exists",
		},
		{
			name: "run name with a sha sharing the prefix fails",
			sha:  testSHA,
			runs: []Run{
				ciRun("success"),
				stabilityRunNamed(20, 1, "success", otherSHASHA,
					"e2e-stability runs=20 sha="+testSHA[:39]+"0"),
			},
			wantReason: "no e2e-stability workflow run exists",
		},
		{
			name:       "no sha",
			sha:        " ",
			runs:       []Run{ciRun("success"), stabilityRun(20, 1, "success")},
			jobs:       stabilityJobs(20, 1, RequiredStabilityRuns),
			wantReason: "no commit sha was given",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.sha, tt.runs, tt.jobs)
			if d.OK != tt.wantOK {
				t.Fatalf("OK = %v, want %v (reasons: %v)", d.OK, tt.wantOK, d.Reasons)
			}
			if tt.wantOK {
				if len(d.Reasons) != 0 {
					t.Fatalf("OK decision carries reasons: %v", d.Reasons)
				}
				if d.StabilityRunID != 20 || d.StabilityAttempt == 0 {
					t.Fatalf("stability identity = %d/%d", d.StabilityRunID, d.StabilityAttempt)
				}
				return
			}
			if len(d.Reasons) == 0 {
				t.Fatal("failed decision carries no reason")
			}
			if !strings.Contains(strings.Join(d.Reasons, "\n"), tt.wantReason) {
				t.Fatalf("reasons %v do not contain %q", d.Reasons, tt.wantReason)
			}
		})
	}
}

func otherSHA(r Run) Run {
	r.HeadSHA = otherSHASHA
	return r
}

func gateJobFailed(runID int64, attempt int) []Job {
	jobs := stabilityJobs(runID, attempt, RequiredStabilityRuns)
	for i := range jobs {
		if jobs[i].Name == "gate" {
			jobs[i].Conclusion = "failure"
		}
	}
	return jobs
}

func e2eJobFailed(runID int64, attempt int) []Job {
	jobs := stabilityJobs(runID, attempt, RequiredStabilityRuns)
	for i := range jobs {
		if jobs[i].Name == "e2e (4/redis)" {
			jobs[i].Conclusion = "failure"
		}
	}
	return jobs
}
