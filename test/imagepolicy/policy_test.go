// Package imagepolicy checks the image scan policy that ci, the nightly
// image-scan workflow, the e2e stability gate, and the release workflow
// share. It reads the repository's own files, so a policy that drifts from
// hack/image-scan.sh or .github/trivy-version fails `go test ./...`.
//
// The check is a property check over every workflow and the script, not a
// sample: every step is examined, and a table of in-memory mutations proves
// that each property rejects a broken copy. IMAGE_POLICY_ROOT points the
// check at another tree; see docs/formal-verification.md.
package imagepolicy

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

const (
	versionFile      = ".github/trivy-version"
	scanScript       = "hack/image-scan.sh"
	trivyVersionExpr = "${{ steps.trivy-version.outputs.value }}"
)

var (
	versionRE   = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	pinnedRE    = regexp.MustCompile(`^[^@\s]+@[0-9a-f]{40}$`)
	argsRE      = regexp.MustCompile(`(?m)^readonly TRIVY_ARGS=\((.*)\)$`)
	platformsRE = regexp.MustCompile(`(?m)^readonly PLATFORMS=\((.*)\)$`)
	trivyRunRE  = regexp.MustCompile(`(?im)(?:^|[\s;&|(])(?:[\w.~/+-]*/)?trivy\s+(?:image|filesystem|fs|rootfs|repo|config|sbom|kubernetes|server|aws|vm)\b`)
)

// policyArgs is the exact scan policy, one value per flag.
var policyArgs = map[string]string{
	"--scanners":       "vuln",
	"--severity":       "HIGH,CRITICAL",
	"--ignore-unfixed": "",
	"--exit-code":      "1",
}

var scanPlatforms = []string{"linux/amd64", "linux/arm64"}

type needs []string

func (n *needs) UnmarshalYAML(v *yaml.Node) error {
	if v.Kind == yaml.ScalarNode {
		*n = needs{v.Value}
		return nil
	}
	var list []string
	if err := v.Decode(&list); err != nil {
		return err
	}
	*n = list
	return nil
}

func (n needs) has(want string) bool {
	for _, got := range n {
		if got == want {
			return true
		}
	}
	return false
}

type step struct {
	Name            string         `yaml:"name"`
	ID              string         `yaml:"id"`
	Uses            string         `yaml:"uses"`
	Run             string         `yaml:"run"`
	If              string         `yaml:"if"`
	ContinueOnError any            `yaml:"continue-on-error"`
	With            map[string]any `yaml:"with"`
}

func (s *step) withString(key string) string {
	got, _ := s.With[key].(string)
	return got
}

func (s *step) blocking() bool {
	switch v := s.ContinueOnError.(type) {
	case bool:
		return !v
	case string:
		return v != "true"
	}
	return true
}

type job struct {
	Name     string `yaml:"name"`
	Needs    needs  `yaml:"needs"`
	If       string `yaml:"if"`
	Strategy struct {
		Matrix yaml.Node `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []*step `yaml:"steps"`
}

func (j *job) matrixImages() []string {
	if j.Strategy.Matrix.Kind != yaml.MappingNode {
		return nil
	}
	var m struct {
		Image []string `yaml:"image"`
	}
	if err := j.Strategy.Matrix.Decode(&m); err != nil {
		return nil
	}
	return m.Image
}

func (j *job) step(pred func(*step) bool) int {
	for i, s := range j.Steps {
		if s != nil && pred(s) {
			return i
		}
	}
	return -1
}

type workflow struct {
	On   map[string]any  `yaml:"on"`
	Jobs map[string]*job `yaml:"jobs"`
}

type repo struct {
	workflows map[string]*workflow
	script    string
	version   string
}

func loadRepo(t *testing.T) *repo {
	t.Helper()
	root := filepath.Join("..", "..")
	if override := os.Getenv("IMAGE_POLICY_ROOT"); override != "" {
		root = override
	}
	r := &repo{workflows: map[string]*workflow{}}

	versions, err := os.ReadFile(filepath.Join(root, versionFile))
	if err != nil {
		t.Fatalf("read %s: %v", versionFile, err)
	}
	r.version = strings.TrimSpace(string(versions))

	script, err := os.ReadFile(filepath.Join(root, scanScript))
	if err != nil {
		t.Fatalf("read %s: %v", scanScript, err)
	}
	r.script = string(script)

	entries, err := os.ReadDir(filepath.Join(root, ".github", "workflows"))
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var w workflow
		if err := yaml.Unmarshal(raw, &w); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		r.workflows[name] = &w
	}
	if len(r.workflows) == 0 {
		t.Fatal("no workflows found")
	}
	return r
}

// check returns every policy violation, one line each. An empty result means
// the repository conforms.
func check(r *repo) []string {
	var v []string
	v = append(v, checkVersion(r)...)
	v = append(v, checkScript(r)...)

	names := make([]string, 0, len(r.workflows))
	for name := range r.workflows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v = append(v, checkWorkflow(name, r.workflows[name])...)
	}
	v = append(v, checkRelease(r)...)
	for _, name := range []string{"ci.yml", "e2e-stability.yml", "image-scan.yml"} {
		v = append(v, checkScanWorkflows(r, name)...)
	}
	return v
}

func checkVersion(r *repo) []string {
	if !versionRE.MatchString(r.version) {
		return []string{fmt.Sprintf("%s: want a pinned vMAJOR.MINOR.PATCH version, got %q", versionFile, r.version)}
	}
	return nil
}

func checkScript(r *repo) []string {
	var v []string
	args := argsRE.FindAllStringSubmatch(r.script, -1)
	if len(args) != 1 {
		return append(v, fmt.Sprintf("%s: want exactly one TRIVY_ARGS declaration, found %d", scanScript, len(args)))
	}
	tokens := strings.Fields(args[0][1])
	got := map[string]string{}
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if !strings.HasPrefix(token, "--") {
			continue
		}
		got[token] = "present"
		if policyArgs[token] != "" && i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "--") {
			got[token] = tokens[i+1]
			i++
		}
	}
	for flag, want := range policyArgs {
		have, ok := got[flag]
		if !ok {
			v = append(v, fmt.Sprintf("%s: TRIVY_ARGS must contain %s", scanScript, flag))
			continue
		}
		if want != "" && have != want {
			v = append(v, fmt.Sprintf("%s: TRIVY_ARGS must contain %s %s, got %q", scanScript, flag, want, have))
		}
	}

	// A second, weaker scan invocation would bypass the policy above.
	for _, flag := range []string{"--scanners", "--severity", "--ignore-unfixed", "--exit-code"} {
		if count := strings.Count(r.script, flag); count != 1 {
			v = append(v, fmt.Sprintf("%s: %s appears %d times, want once", scanScript, flag, count))
		}
	}
	if !strings.Contains(r.script, `"${TRIVY_ARGS[@]}"`) {
		v = append(v, fmt.Sprintf("%s: the trivy invocation must expand TRIVY_ARGS", scanScript))
	}

	platforms := platformsRE.FindAllStringSubmatch(r.script, -1)
	if len(platforms) != 1 {
		return append(v, fmt.Sprintf("%s: want exactly one PLATFORMS declaration, found %d", scanScript, len(platforms)))
	}
	if have := strings.Fields(platforms[0][1]); !sameStrings(have, scanPlatforms) {
		v = append(v, fmt.Sprintf("%s: PLATFORMS must be %v, got %v", scanScript, scanPlatforms, have))
	}
	if !strings.Contains(r.script, "set -euo pipefail") {
		v = append(v, fmt.Sprintf("%s: must set -euo pipefail", scanScript))
	}
	return v
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func checkWorkflow(name string, w *workflow) []string {
	var v []string
	ids := make([]string, 0, len(w.Jobs))
	for id := range w.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		j := w.Jobs[id]
		if j == nil {
			continue
		}
		callsScan := false
		hasSetupTrivy := false
		for _, s := range j.Steps {
			if s == nil {
				continue
			}
			where := fmt.Sprintf("%s: job %s step %q", name, id, s.Name)
			if s.Uses != "" {
				if !strings.HasPrefix(s.Uses, "./") && !pinnedRE.MatchString(s.Uses) {
					v = append(v, fmt.Sprintf("%s: action %q is not pinned to a 40-hex commit", where, s.Uses))
				}
				if strings.Contains(s.Uses, "aquasecurity/setup-trivy") {
					hasSetupTrivy = true
					if got := s.withString("version"); got != trivyVersionExpr {
						v = append(v, fmt.Sprintf("%s: setup-trivy version must be %q (read from %s), got %q", where, trivyVersionExpr, versionFile, got))
					}
					if j.step(func(x *step) bool {
						return x.ID == "trivy-version" && strings.Contains(x.Run, "cat "+versionFile)
					}) < 0 {
						v = append(v, fmt.Sprintf("%s: job %s must read %s into the trivy-version step", name, id, versionFile))
					}
				}
			}
			if s.Run != "" && trivyRunRE.MatchString(s.Run) {
				v = append(v, fmt.Sprintf("%s: the run step must not invoke trivy directly; call %s", where, scanScript))
			}
			// The shellcheck step lists the script as an argument; only a step
			// that does not lint it can run it.
			if strings.Contains(s.Run, scanScript) && !strings.Contains(s.Run, "shellcheck") {
				callsScan = true
				if strings.Contains(s.Run, "--platform ") {
					v = append(v, fmt.Sprintf("%s: must not narrow the scan platforms", where))
				}
			}
		}
		if callsScan && !hasSetupTrivy {
			v = append(v, fmt.Sprintf("%s: job %s calls %s without installing the pinned trivy", name, id, scanScript))
		}
	}
	return v
}

// checkRelease proves the images job still builds, scans, signs, and tags in
// that order, and that the scan cannot be made non-blocking.
func checkRelease(r *repo) []string {
	var v []string
	w, ok := r.workflows["release.yml"]
	if !ok {
		return []string{"release.yml: missing"}
	}
	images, ok := w.Jobs["images"]
	if !ok {
		return []string{"release.yml: job images is missing"}
	}
	build := images.step(func(s *step) bool { return strings.Contains(s.Uses, "docker/build-push-action") })
	scan := images.step(func(s *step) bool { return strings.Contains(s.Run, scanScript) })
	sign := images.step(func(s *step) bool { return strings.Contains(s.Run, "cosign sign") })
	tag := images.step(func(s *step) bool { return strings.Contains(s.Run, "imagetools create") })
	if build < 0 || scan < 0 || sign < 0 || tag < 0 {
		return append(v, "release.yml: images needs a build step, the shared scan, cosign sign, and imagetools create")
	}
	if build >= scan || scan >= sign || sign >= tag {
		v = append(v, "release.yml: images must build, then scan, then sign, then tag")
	}
	if !images.Steps[scan].blocking() {
		v = append(v, "release.yml: the scan step must not set continue-on-error")
	}
	return v
}

// checkScanWorkflows proves each scheduled or gating workflow builds both
// images without pushing them and scans them with the shared script.
func checkScanWorkflows(r *repo, name string) []string {
	w, ok := r.workflows[name]
	if !ok {
		return []string{fmt.Sprintf("%s: missing", name)}
	}
	var v []string

	switch name {
	case "ci.yml":
		changes, ok := w.Jobs["changes"]
		if !ok {
			return append(v, "ci.yml: no changes job gates the image scan")
		}
		if changes.step(func(s *step) bool { return strings.Contains(s.Uses, "dorny/paths-filter") }) < 0 {
			v = append(v, "ci.yml: the changes job must use dorny/paths-filter, so an unrelated pull request still reports a skipped check")
		}
		job, ok := w.Jobs["image-scan"]
		if !ok {
			return append(v, "ci.yml: no image-scan job")
		}
		if !job.Needs.has("changes") {
			v = append(v, "ci.yml: image-scan must need changes")
		}
		v = append(v, checkImageMatrix("ci.yml", "image-scan", job)...)
	case "e2e-stability.yml":
		job, ok := w.Jobs["preflight-scan"]
		if !ok {
			return append(v, "e2e-stability.yml: no preflight-scan job")
		}
		v = append(v, checkImageMatrix("e2e-stability.yml", "preflight-scan", job)...)
		if job.Needs == nil || !job.Needs.has("plan") {
			v = append(v, "e2e-stability.yml: preflight-scan must need plan")
		}
		checkout := job.step(func(s *step) bool { return strings.Contains(s.Uses, "actions/checkout") })
		if checkout < 0 {
			v = append(v, "e2e-stability.yml: preflight-scan must check out the gated ref")
		} else if got := job.Steps[checkout].withString("ref"); got != "${{ needs.plan.outputs.sha }}" {
			v = append(v, fmt.Sprintf("e2e-stability.yml: preflight-scan must build the gated ref, got checkout ref %q", got))
		}
		gate, ok := w.Jobs["gate"]
		if !ok {
			return append(v, "e2e-stability.yml: no gate job")
		}
		if !gate.Needs.has("preflight-scan") {
			v = append(v, "e2e-stability.yml: gate needs preflight-scan")
		}
		return v
	case "image-scan.yml":
		for _, trigger := range []string{"schedule", "workflow_dispatch"} {
			if _, ok := w.On[trigger]; !ok {
				v = append(v, fmt.Sprintf("image-scan.yml: must run on %s", trigger))
			}
		}
		job, ok := w.Jobs["main"]
		if !ok {
			return append(v, "image-scan.yml: no main job")
		}
		v = append(v, checkImageMatrix("image-scan.yml", "main", job)...)
		published, ok := w.Jobs["published"]
		if !ok {
			return append(v, "image-scan.yml: no published job")
		}
		if published.step(func(s *step) bool {
			return strings.Contains(s.Run, scanScript) && strings.Contains(s.Run, "ghcr.io/")
		}) < 0 {
			v = append(v, "image-scan.yml: published must scan the published ghcr.io images")
		}
	default:
		v = append(v, fmt.Sprintf("%s: no policy checks for this workflow", name))
	}
	return v
}

func checkImageMatrix(name, jobID string, j *job) []string {
	var v []string
	images := j.matrixImages()
	sort.Strings(images)
	want := []string{"mayfly-agent", "mayflyd"}
	if !sameStrings(images, want) {
		v = append(v, fmt.Sprintf("%s: job %s matrix must build %v, got %v", name, jobID, want, images))
	}
	if j.step(func(s *step) bool { return strings.Contains(s.Run, scanScript) && strings.Contains(s.Run, "--dir") }) < 0 {
		v = append(v, fmt.Sprintf("%s: job %s must scan the local archives with %s --dir", name, jobID, scanScript))
	}
	if j.step(func(s *step) bool {
		return strings.Contains(s.Uses, "docker/build-push-action") || strings.Contains(s.Uses, "docker/login-action")
	}) >= 0 {
		v = append(v, fmt.Sprintf("%s: job %s must build without pushing", name, jobID))
	}
	return v
}

func TestWorkflowPolicy(t *testing.T) {
	r := loadRepo(t)
	for _, violation := range check(r) {
		t.Error(violation)
	}
}

// TestWorkflowPolicyRejectsBrokenCopies is the negative check: each mutation
// is a plausible way to weaken the scan, and check must report it.
func TestWorkflowPolicyRejectsBrokenCopies(t *testing.T) {
	mutations := []struct {
		name   string
		want   string
		mutate func(*repo)
	}{
		{
			name: "severity lowered to CRITICAL only",
			want: "--severity HIGH,CRITICAL",
			mutate: func(r *repo) {
				r.script = strings.Replace(r.script, "--severity HIGH,CRITICAL", "--severity CRITICAL", 1)
			},
		},
		{
			name: "unfixed findings stop blocking",
			want: "--ignore-unfixed",
			mutate: func(r *repo) {
				r.script = strings.Replace(r.script, " --ignore-unfixed", "", 1)
			},
		},
		{
			name: "only one platform is scanned",
			want: "PLATFORMS",
			mutate: func(r *repo) {
				r.script = strings.Replace(r.script, "readonly PLATFORMS=(linux/amd64 linux/arm64)", "readonly PLATFORMS=(linux/amd64)", 1)
			},
		},
		{
			name: "a workflow runs trivy directly",
			want: "must not invoke trivy directly",
			mutate: func(r *repo) {
				w := r.workflows["ci.yml"]
				w.Jobs["test"].Steps = append(w.Jobs["test"].Steps, &step{
					Name: "inline scan",
					Run:  "trivy image --severity HIGH,CRITICAL example.invalid/mayflyd",
				})
			},
		},
		{
			name: "setup-trivy drifts from the pinned version",
			want: "setup-trivy version must be",
			mutate: func(r *repo) {
				j := r.workflows["ci.yml"].Jobs["image-scan"]
				i := j.step(func(s *step) bool { return strings.Contains(s.Uses, "aquasecurity/setup-trivy") })
				j.Steps[i].With["version"] = "v0.76.1"
			},
		},
		{
			name: "an action is pinned by tag",
			want: "not pinned to a 40-hex commit",
			mutate: func(r *repo) {
				j := r.workflows["ci.yml"].Jobs["image-scan"]
				i := j.step(func(s *step) bool { return strings.Contains(s.Uses, "actions/checkout") })
				j.Steps[i].Uses = "actions/checkout@v7.0.1"
			},
		},
		{
			name: "the release signs before it scans",
			want: "build, then scan, then sign, then tag",
			mutate: func(r *repo) {
				j := r.workflows["release.yml"].Jobs["images"]
				scan := j.step(func(s *step) bool { return strings.Contains(s.Run, scanScript) })
				sign := j.step(func(s *step) bool { return strings.Contains(s.Run, "cosign sign") })
				j.Steps[scan], j.Steps[sign] = j.Steps[sign], j.Steps[scan]
			},
		},
		{
			name: "the release scan no longer blocks",
			want: "must not set continue-on-error",
			mutate: func(r *repo) {
				j := r.workflows["release.yml"].Jobs["images"]
				i := j.step(func(s *step) bool { return strings.Contains(s.Run, scanScript) })
				j.Steps[i].ContinueOnError = true
			},
		},
		{
			name: "the stability gate does not depend on the scan",
			want: "gate needs preflight-scan",
			mutate: func(r *repo) {
				gate := r.workflows["e2e-stability.yml"].Jobs["gate"]
				var kept needs
				for _, n := range gate.Needs {
					if n != "preflight-scan" {
						kept = append(kept, n)
					}
				}
				gate.Needs = kept
			},
		},
		{
			name: "the preflight scans the default branch, not the gated ref",
			want: "must build the gated ref",
			mutate: func(r *repo) {
				j := r.workflows["e2e-stability.yml"].Jobs["preflight-scan"]
				i := j.step(func(s *step) bool { return strings.Contains(s.Uses, "actions/checkout") })
				j.Steps[i].With["ref"] = "main"
			},
		},
		{
			name: "the nightly workflow is gone",
			want: "image-scan.yml: missing",
			mutate: func(r *repo) {
				delete(r.workflows, "image-scan.yml")
			},
		},
		{
			name: "the nightly no longer runs on a schedule",
			want: "must run on schedule",
			mutate: func(r *repo) {
				delete(r.workflows["image-scan.yml"].On, "schedule")
			},
		},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			r := loadRepo(t)
			tt.mutate(r)
			violations := check(r)
			if len(violations) == 0 {
				t.Fatalf("the check accepted a broken copy")
			}
			if !strings.Contains(strings.Join(violations, "\n"), tt.want) {
				t.Fatalf("violations do not mention %q:\n%s", tt.want, strings.Join(violations, "\n"))
			}
		})
	}
}
