// The publish job of the release workflow turns the draft into a published
// release. It has no checkout, so every gh call must name the repository, and
// it must not ask `gh release view` for the isLatest JSON field, which the gh
// CLI does not accept. These checks cover every job in every workflow, not one
// sample step.

package release

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const publishScript = "hack/publish-release.sh"

var (
	// ghCallRE matches a `gh` word after a command separator or in a command
	// substitution. The argument text after it is one line, because a
	// continuation is joined before the scan.
	ghCallRE = regexp.MustCompile(`(?:^|[\s;&|("'$])gh\s`)
	// ghJSONRE captures the field list of --json, in the `--json a,b` form and
	// the `--json=a` form.
	ghJSONRE = regexp.MustCompile(`--json(?:=|\s+)([A-Za-z0-9_,.-]+)`)
	// repoArgRE matches the explicit repository flags.
	repoArgRE = regexp.MustCompile(`(?:^|\s)(?:-R|--repo)(?:\s|=)`)
	// singleQuotedRE removes jq filters, so a filter that mentions repos/
	// cannot make an unqualified call look repo-qualified.
	singleQuotedRE = regexp.MustCompile(`'[^']*'`)
)

type workflowStep struct {
	Name string         `yaml:"name"`
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	Env  map[string]any `yaml:"env"`
}

type workflowJob struct {
	Env   map[string]any  `yaml:"env"`
	Steps []*workflowStep `yaml:"steps"`
}

type workflowFile struct {
	Jobs map[string]*workflowJob `yaml:"jobs"`
}

// loadWorkflowFiles parses every workflow under .github/workflows.
func loadWorkflowFiles(t *testing.T) map[string]*workflowFile {
	t.Helper()
	dir := filepath.Join(repoRoot, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}
	files := map[string]*workflowFile{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var file workflowFile
		if err := yaml.Unmarshal(raw, &file); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = &file
	}
	if len(files) == 0 {
		t.Fatal("no workflows found")
	}
	return files
}

// ghCalls returns each gh invocation in a run script, from the word gh to the
// end of its line. A backslash continuation is joined first, so a multi-line
// gh command counts as one call.
func ghCalls(run string) []string {
	run = strings.ReplaceAll(run, "\\\n", " ")
	var calls []string
	for _, line := range strings.Split(run, "\n") {
		for _, loc := range ghCallRE.FindAllStringIndex(line, -1) {
			match := line[loc[0]:loc[1]]
			start := loc[0] + strings.Index(match, "gh")
			calls = append(calls, strings.TrimSpace(line[start:]))
		}
	}
	return calls
}

// jsonFields returns every field named by a --json option in a run script.
func jsonFields(run string) []string {
	var fields []string
	for _, match := range ghJSONRE.FindAllStringSubmatch(run, -1) {
		fields = append(fields, strings.Split(match[1], ",")...)
	}
	return fields
}

// repoQualified reports whether a gh invocation names its repository, with
// -R, --repo, or an api path that starts repos/.
func repoQualified(call string) bool {
	if repoArgRE.MatchString(call) {
		return true
	}
	return strings.HasPrefix(call, "gh api ") && strings.Contains(singleQuotedRE.ReplaceAllString(call, "''"), "repos/")
}

// envSet reports whether env sets key to a non-empty value.
func envSet(env map[string]any, key string) bool {
	value, ok := env[key]
	if !ok {
		return false
	}
	return strings.TrimSpace(fmt.Sprint(value)) != ""
}

// checkGHJobs reports a gh call in a job that has no actions/checkout step and
// no GH_REPO in the step or job env, and whose gh call is not repo-qualified.
// It also reports any step that asks gh for the isLatest JSON field.
func checkGHJobs(files map[string]*workflowFile) []string {
	var violations []string
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file := files[name]
		if file == nil {
			continue
		}
		jobIDs := make([]string, 0, len(file.Jobs))
		for id := range file.Jobs {
			jobIDs = append(jobIDs, id)
		}
		sort.Strings(jobIDs)
		for _, id := range jobIDs {
			job := file.Jobs[id]
			if job == nil {
				continue
			}
			checkout := false
			for _, step := range job.Steps {
				if step != nil && strings.HasPrefix(step.Uses, "actions/checkout@") {
					checkout = true
				}
			}
			for _, step := range job.Steps {
				if step == nil {
					continue
				}
				where := fmt.Sprintf("%s: job %s step %q", name, id, step.Name)
				for _, field := range jsonFields(step.Run) {
					if field == "isLatest" {
						violations = append(violations, fmt.Sprintf("%s: must not ask gh for --json isLatest; read repos/{owner}/{repo}/releases/latest with gh api", where))
					}
				}
				for _, call := range ghCalls(step.Run) {
					if checkout || envSet(job.Env, "GH_REPO") || envSet(step.Env, "GH_REPO") || repoQualified(call) {
						continue
					}
					violations = append(violations, fmt.Sprintf("%s: gh call %q has no checkout, GH_REPO, or -R/--repo", where, call))
				}
			}
		}
	}
	return violations
}

// checkPublishJob reports a publish job that does not run the shared script
// with GH_REPO set, or that keeps gh inline where the script belongs.
func checkPublishJob(files map[string]*workflowFile) []string {
	file, ok := files["release.yml"]
	if !ok || file == nil {
		return []string{"release.yml: missing"}
	}
	job, ok := file.Jobs["publish"]
	if !ok || job == nil {
		return []string{"release.yml: job publish is missing"}
	}
	found := false
	var violations []string
	for _, step := range job.Steps {
		if step == nil || !strings.Contains(step.Run, publishScript) {
			continue
		}
		found = true
		where := fmt.Sprintf("release.yml: job publish step %q", step.Name)
		if !envSet(step.Env, "GH_REPO") {
			violations = append(violations, where+": env must set GH_REPO, so the job needs no checkout")
		}
		if strings.Contains(step.Run, "gh ") {
			violations = append(violations, where+": must run "+publishScript+" instead of inline gh calls")
		}
	}
	if !found {
		violations = append(violations, "release.yml: job publish must run "+publishScript)
	}
	return violations
}

func checkReleasePublish(files map[string]*workflowFile) []string {
	return append(checkPublishJob(files), checkGHJobs(files)...)
}

func TestReleasePublishPolicy(t *testing.T) {
	for _, violation := range checkReleasePublish(loadWorkflowFiles(t)) {
		t.Error(violation)
	}
}

// TestReleasePublishPolicyRejectsBrokenCopies is the negative check. Each
// mutation is a plausible way to reintroduce the rc.4 failure, and the check
// must report it.
func TestReleasePublishPolicyRejectsBrokenCopies(t *testing.T) {
	mutations := []struct {
		name   string
		want   string
		mutate func(map[string]*workflowFile)
	}{
		{
			name: "publish step loses GH_REPO",
			want: "env must set GH_REPO",
			mutate: func(files map[string]*workflowFile) {
				for _, step := range files["release.yml"].Jobs["publish"].Steps {
					if step != nil && strings.Contains(step.Run, publishScript) {
						delete(step.Env, "GH_REPO")
					}
				}
			},
		},
		{
			name: "a step asks for the isLatest JSON field",
			want: "--json isLatest",
			mutate: func(files map[string]*workflowFile) {
				gate := files["release.yml"].Jobs["gate"]
				gate.Steps = append(gate.Steps, &workflowStep{
					Name: "verify latest",
					Run:  `gh release view v1.0.0 --json isLatest --jq .isLatest`,
				})
			},
		},
		{
			name: "a JSON field list includes isLatest",
			want: "isLatest",
			mutate: func(files map[string]*workflowFile) {
				gate := files["release.yml"].Jobs["gate"]
				gate.Steps = append(gate.Steps, &workflowStep{
					Name: "verify latest",
					Run:  `gh release view v1.0.0 --json tagName,isLatest --jq .tagName`,
				})
			},
		},
		{
			name: "a job runs gh without a checkout, GH_REPO, or repo flag",
			want: "no checkout, GH_REPO, or -R/--repo",
			mutate: func(files map[string]*workflowFile) {
				files["release.yml"].Jobs["probe"] = &workflowJob{
					Steps: []*workflowStep{{
						Name: "probe",
						Run:  `gh release view v1.0.0 --json isDraft`,
					}},
				}
			},
		},
		{
			name: "publish keeps gh inline",
			want: "instead of inline gh calls",
			mutate: func(files map[string]*workflowFile) {
				for _, step := range files["release.yml"].Jobs["publish"].Steps {
					if step != nil && strings.Contains(step.Run, publishScript) {
						step.Run = publishScript + "\ngh release view v1.0.0 --json isDraft"
					}
				}
			},
		},
		{
			name: "a checkout is removed from a job with an unqualified gh call",
			want: "no checkout, GH_REPO, or -R/--repo",
			mutate: func(files map[string]*workflowFile) {
				job := files["image-scan.yml"].Jobs["published"]
				steps := job.Steps[:0]
				for _, step := range job.Steps {
					if step != nil && strings.HasPrefix(step.Uses, "actions/checkout@") {
						continue
					}
					steps = append(steps, step)
				}
				job.Steps = steps
			},
		},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			files := loadWorkflowFiles(t)
			mutation.mutate(files)
			violations := checkReleasePublish(files)
			if !strings.Contains(strings.Join(violations, "\n"), mutation.want) {
				t.Fatalf("want a violation containing %q, got %v", mutation.want, violations)
			}
		})
	}
}
