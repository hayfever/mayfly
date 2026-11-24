package releasegate

import (
	"strings"
	"testing"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

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
	r.HeadSHA = "ffffffffffffffffffffffffffffffffffffffff"
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
