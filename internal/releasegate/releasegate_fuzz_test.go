package releasegate

import (
	"encoding/json"
	"path"
	"strings"
	"testing"
)

// FuzzDecide feeds arbitrary workflow-run and job payloads to Decide. The
// decoder is lenient: bytes that are not JSON are an empty history, which is a
// failed gate, not an error. The property is that a PASS cannot be a
// false positive: whenever Decide says OK, a straightforward independent
// search over the same payload also finds a green ci run and a successful
// e2e-stability run with at least 20 e2e jobs and a green gate job.
func FuzzDecide(f *testing.F) {
	seedRuns, err := json.Marshal([]Run{ciRun("success"), stabilityRun(20, 1, "success")})
	if err != nil {
		f.Fatalf("marshal runs: %v", err)
	}
	seedJobs, err := json.Marshal(stabilityJobs(20, 1, RequiredStabilityRuns))
	if err != nil {
		f.Fatalf("marshal jobs: %v", err)
	}
	shortJobs, err := json.Marshal(stabilityJobs(20, 1, 5))
	if err != nil {
		f.Fatalf("marshal jobs: %v", err)
	}
	f.Add(testSHA, seedRuns, seedJobs)
	f.Add(testSHA, seedRuns, shortJobs)
	f.Add(testSHA, seedRuns, []byte("[]"))
	f.Add(testSHA, []byte(`[{"name":"ci","head_sha":"`+testSHA+`","conclusion":"success"}]`), seedJobs)
	f.Add("", []byte("null"), []byte("null"))
	f.Add(testSHA, []byte("{"), []byte("]"))

	// A dispatch on main names the gated commit in the run-name. Only an
	// exact `sha=<40-hex>` token may match; a prefix, a longer hex string, a
	// missing token boundary, or another sha that shares the prefix must not.
	named := func(title string) []byte {
		runs := []Run{
			ciRun("success"),
			stabilityRunNamed(20, 1, "success", otherSHASHA, title),
		}
		data, err := json.Marshal(runs)
		if err != nil {
			f.Fatalf("marshal runs: %v", err)
		}
		return data
	}
	f.Add(testSHA, named("e2e-stability runs=20 sha="+testSHA), seedJobs)
	f.Add(testSHA, named("e2e-stability runs=20 sha="+testSHA[:16]), seedJobs)
	f.Add(testSHA, named("e2e-stability runs=20 sha="+testSHA+"0"), seedJobs)
	f.Add(testSHA, named("e2e-stability runs=20 xsha="+testSHA), seedJobs)
	f.Add(testSHA, named("e2e-stability runs=20 sha="+testSHA[:39]+"0"), seedJobs)

	f.Fuzz(func(t *testing.T, sha string, runsJSON, jobsJSON []byte) {
		var runs []Run
		var jobs []Job
		// Invalid JSON decodes to an empty history, which is the same input a
		// truncated API response gives a caller.
		_ = json.Unmarshal(runsJSON, &runs)
		_ = json.Unmarshal(jobsJSON, &jobs)

		d := Decide(sha, runs, jobs)
		again := Decide(sha, runs, jobs)
		if d.OK != again.OK || len(d.Reasons) != len(again.Reasons) {
			t.Fatalf("Decide is not deterministic: %+v vs %+v", d, again)
		}
		if !d.OK {
			return
		}
		if len(d.Reasons) != 0 {
			t.Fatalf("OK decision carries reasons: %v", d.Reasons)
		}
		if !naiveGate(sha, runs, jobs) {
			t.Fatalf("Decide passed %q but the naive search does not: %+v", sha, d)
		}
	})
}

// naiveGate is a deliberately simpler and more permissive check than Decide:
// it counts any job whose name starts with "e2e (" and accepts 20 or more. A
// PASS from the strict implementation must also pass this one. The stability
// selector still requires an exact `sha=` token, so a title substring cannot
// make the naive check disagree with Decide in the unsafe direction.
func naiveGate(sha string, runs []Run, jobs []Job) bool {
	ci := false
	for _, r := range runs {
		if r.HeadSHA == sha && r.Conclusion == "success" && isCI(r) {
			ci = true
		}
	}
	if !ci {
		return false
	}
	for _, r := range runs {
		if !naiveMatchesSHA(r, sha) || r.Conclusion != "success" || !isStability(r) {
			continue
		}
		e2e, gate := 0, false
		for _, j := range jobs {
			if j.RunID != r.ID {
				continue
			}
			if r.RunAttempt != 0 && j.RunAttempt != 0 && j.RunAttempt != r.RunAttempt {
				continue
			}
			if strings.HasPrefix(j.Name, "e2e (") {
				e2e++
			}
			if j.Name == stabilityGateName && j.Conclusion == "success" {
				gate = true
			}
		}
		if e2e >= RequiredStabilityRuns && gate {
			return true
		}
	}
	return false
}

func isCI(r Run) bool {
	return r.Name == ciWorkflowName || path.Base(r.Path) == ciWorkflowFile
}

// naiveMatchesSHA accepts a run by head_sha or by an exact `sha=<sha>`
// whitespace-separated token. It never accepts a substring.
func naiveMatchesSHA(r Run, sha string) bool {
	if r.HeadSHA == sha {
		return true
	}
	if sha == "" {
		return false
	}
	for _, field := range strings.Fields(r.DisplayTitle) {
		if strings.EqualFold(field, "sha="+sha) {
			return true
		}
	}
	return false
}

func isStability(r Run) bool {
	return r.Name == gateWorkflowName || path.Base(r.Path) == gateWorkflowFile
}
