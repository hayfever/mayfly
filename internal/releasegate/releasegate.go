// Package releasegate decides whether a commit passed the gates that the
// release workflow requires before it publishes anything: a successful ci run
// and a successful e2e-stability run with the full 20-run matrix, both for the
// tagged commit.
//
// Decide reads data, never the network. The release workflow collects the
// payloads with `gh api`; a test feeds a recorded or synthetic payload. A
// successful workflow attempt counts on its own, because #52 allows one rerun
// of an infrastructure glitch: if attempt 1 failed and attempt 2 passed, the
// commit is gated.
package releasegate

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// RequiredStabilityRuns is the number of e2e jobs in a qualifying
// e2e-stability run. The e2e-stability workflow names each matrix job
// `e2e (<run>/<queue>)`, so the count is the `runs` input the run used.
const RequiredStabilityRuns = 20

const (
	ciWorkflowName    = "ci"
	ciWorkflowFile    = "ci.yml"
	gateWorkflowName  = "e2e-stability"
	gateWorkflowFile  = "e2e-stability.yml"
	stabilityGateName = "gate"
)

// e2eJob matches the e2e-stability matrix job name, for example `e2e (1/bbolt)`.
var e2eJob = regexp.MustCompile(`^e2e \((\d+)/(bbolt|redis)\)$`)

// titleSHA matches the run-name that e2e-stability.yml sets:
// `e2e-stability runs=20 sha=<40-hex>`. The sha is a whole token, so a prefix,
// a longer hex string, `xsha=`, or a different sha never matches.
var titleSHA = regexp.MustCompile(`(?:^|\s)sha=([0-9a-fA-F]{40})(?:\s|$)`)

// Run is the subset of a GitHub Actions workflow run that the gate reads.
type Run struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	HeadSHA      string `json:"head_sha"`
	DisplayTitle string `json:"display_title"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	RunAttempt   int    `json:"run_attempt"`
	CreatedAt    string `json:"created_at"`
}

// Job is the subset of a GitHub Actions workflow job that the gate reads.
type Job struct {
	RunID      int64  `json:"run_id"`
	Name       string `json:"name"`
	RunAttempt int    `json:"run_attempt"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// Decision is the gate outcome. OK is true only when Reasons is empty.
type Decision struct {
	OK bool `json:"ok"`
	// Reasons lists every failed check, one line each.
	Reasons []string `json:"reasons,omitempty"`
	// StabilityRunID and StabilityAttempt identify the run that satisfied the
	// stability gate. They are zero when the gate failed.
	StabilityRunID   int64 `json:"stability_run_id,omitempty"`
	StabilityAttempt int   `json:"stability_attempt,omitempty"`
}

// Decide checks the workflow runs and jobs that belong to sha.
func Decide(sha string, runs []Run, jobs []Job) Decision {
	var d Decision
	sha = strings.TrimSpace(sha)
	if sha == "" {
		d.Reasons = append(d.Reasons, "no commit sha was given")
		return d
	}
	if reason := checkCI(sha, runs); reason != "" {
		d.Reasons = append(d.Reasons, reason)
	}
	if reason := checkStability(sha, runs, jobs, &d); reason != "" {
		d.Reasons = append(d.Reasons, reason)
	}
	d.OK = len(d.Reasons) == 0
	return d
}

// checkCI returns "" when a completed ci run succeeded for sha.
func checkCI(sha string, runs []Run) string {
	var latest *Run
	for i := range runs {
		r := &runs[i]
		if r.HeadSHA != sha || !isWorkflow(*r, ciWorkflowName, ciWorkflowFile) {
			continue
		}
		if r.Conclusion == "success" && completed(*r) {
			return ""
		}
		if latest == nil || newer(*r, *latest) {
			latest = r
		}
	}
	if latest == nil {
		return fmt.Sprintf("no ci workflow run exists for %s", sha)
	}
	return fmt.Sprintf("the ci workflow did not succeed for %s (attempt %d concluded %s)",
		sha, latest.RunAttempt, describe(latest))
}

// checkStability returns "" when a successful e2e-stability attempt with the
// full 20-run matrix exists for sha, and records which run satisfied it.
//
// A stability run can be dispatched on main while it names the gated commit in
// its run-name, so a run also belongs to sha when display_title carries an
// exact `sha=<sha>` token. ci stays on head_sha, because it runs on push.
func checkStability(sha string, runs []Run, jobs []Job, d *Decision) string {
	var succeeded, attempted []Run
	for _, r := range runs {
		if !isWorkflow(r, gateWorkflowName, gateWorkflowFile) || !matchesStabilitySHA(r, sha) {
			continue
		}
		attempted = append(attempted, r)
		if r.Conclusion == "success" && completed(r) {
			succeeded = append(succeeded, r)
		}
	}
	if len(succeeded) == 0 {
		if len(attempted) == 0 {
			return fmt.Sprintf("no e2e-stability workflow run exists for %s", sha)
		}
		latest := attempted[0]
		for _, r := range attempted[1:] {
			if newer(r, latest) {
				latest = r
			}
		}
		return fmt.Sprintf("the e2e-stability workflow did not succeed for %s (attempt %d concluded %s)",
			sha, latest.RunAttempt, describe(&latest))
	}

	sort.SliceStable(succeeded, func(i, j int) bool { return newer(succeeded[i], succeeded[j]) })
	var firstReason string
	for _, run := range succeeded {
		reason, ok := qualifies(run, jobs)
		if ok {
			d.StabilityRunID = run.ID
			d.StabilityAttempt = run.RunAttempt
			return ""
		}
		if firstReason == "" {
			firstReason = reason
		}
	}
	return firstReason
}

// qualifies reports whether one successful e2e-stability run has the full
// matrix and a green stability gate job.
func qualifies(run Run, jobs []Job) (string, bool) {
	prefix := fmt.Sprintf("the successful e2e-stability run %d (attempt %d)", run.ID, run.RunAttempt)

	selected := 0
	runNumbers := make(map[int]string, RequiredStabilityRuns)
	for _, j := range jobs {
		if j.RunID != run.ID {
			continue
		}
		// A rerun appends jobs from every attempt. Only the attempt that made
		// the run successful counts.
		if run.RunAttempt != 0 && j.RunAttempt != 0 && j.RunAttempt != run.RunAttempt {
			continue
		}
		selected++
		if m := e2eJob.FindStringSubmatch(j.Name); m != nil {
			n, err := strconv.Atoi(m[1])
			if err == nil {
				runNumbers[n] = j.Conclusion
			}
		}
	}
	if selected == 0 {
		return prefix + " has no recorded jobs", false
	}
	if len(runNumbers) != RequiredStabilityRuns {
		return fmt.Sprintf("%s has %d of %d e2e jobs",
			prefix, len(runNumbers), RequiredStabilityRuns), false
	}
	var failed []string
	for n := 1; n <= RequiredStabilityRuns; n++ {
		conclusion, ok := runNumbers[n]
		if !ok {
			return fmt.Sprintf("%s has no e2e job for run %d", prefix, n), false
		}
		if conclusion != "success" {
			failed = append(failed, fmt.Sprintf("%d (%s)", n, orUnknown(conclusion)))
		}
	}
	if len(failed) != 0 {
		return fmt.Sprintf("%s has failed e2e jobs: %s", prefix, strings.Join(failed, ", ")), false
	}

	for _, j := range jobs {
		if j.RunID != run.ID || j.Name != stabilityGateName {
			continue
		}
		if run.RunAttempt != 0 && j.RunAttempt != 0 && j.RunAttempt != run.RunAttempt {
			continue
		}
		if j.Conclusion == "success" {
			return "", true
		}
		return fmt.Sprintf("%s has a %s gate job", prefix, orUnknown(j.Conclusion)), false
	}
	return prefix + " has no gate job", false
}

// isWorkflow reports whether run belongs to the workflow with the given name
// or file. The name is what the API reports today; the file keeps the check
// working if a workflow is renamed.
func isWorkflow(r Run, name, file string) bool {
	if r.Name == name {
		return true
	}
	return r.Path != "" && path.Base(r.Path) == file
}

// matchesStabilitySHA reports whether an e2e-stability run tested sha. A
// dispatch can run on another commit and name the target in its run-name.
func matchesStabilitySHA(r Run, sha string) bool {
	if r.HeadSHA == sha {
		return true
	}
	return titleHasSHA(r.DisplayTitle, sha)
}

// titleHasSHA reports whether title carries `sha=<sha>` as a whole token.
func titleHasSHA(title, sha string) bool {
	if title == "" || sha == "" {
		return false
	}
	for _, m := range titleSHA.FindAllStringSubmatch(title, -1) {
		if strings.EqualFold(m[1], sha) {
			return true
		}
	}
	return false
}

func completed(r Run) bool { return r.Status == "" || r.Status == "completed" }

func newer(a, b Run) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	if a.RunAttempt != b.RunAttempt {
		return a.RunAttempt > b.RunAttempt
	}
	return a.ID > b.ID
}

func describe(r *Run) string {
	if r.Conclusion != "" {
		return fmt.Sprintf("%q", r.Conclusion)
	}
	if r.Status != "" {
		return fmt.Sprintf("%q", r.Status)
	}
	return "unknown"
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
