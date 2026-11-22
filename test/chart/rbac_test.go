// Package chart checks properties of the rendered Helm chart. The RBAC check
// is exhaustive: it runs helm template for every equality partition of the
// release, sessions, and plugins namespaces and asserts the rendered Roles and
// RoleBindings against the grants the control plane uses.
package chart

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	defaultChartDir = "../../deploy/helm/mayfly"
	requireHelmEnv  = "MAYFLY_REQUIRE_HELM"
	chartDirEnv     = "MAYFLY_CHART_DIR"
)

// object is the part of a rendered manifest this check reads.
type object struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       string       `yaml:"kind"`
	Metadata   objectMeta   `yaml:"metadata"`
	Rules      []policyRule `yaml:"rules"`
	RoleRef    objectRef    `yaml:"roleRef"`
	Subjects   []subject    `yaml:"subjects"`
}

type objectMeta struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

type policyRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

type objectRef struct {
	Kind string `yaml:"kind"`
	Name string `yaml:"name"`
}

type subject struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// grant is one (apiGroup, resource, verb) permission.
type grant struct {
	group    string
	resource string
	verb     string
}

type grantSet map[grant]struct{}

// spec is a compact description of a grant set: apiGroup -> resource -> verbs.
type spec map[string]map[string][]string

func (s spec) grants() grantSet {
	out := grantSet{}
	for group, resources := range s {
		for resource, verbs := range resources {
			for _, verb := range verbs {
				out[grant{group: group, resource: resource, verb: verb}] = struct{}{}
			}
		}
	}
	return out
}

// sessionRequired is the RBAC the control plane uses for session objects in
// config.sessions.namespace. Every grant maps to a call site in
// internal/k8s/client.go; docs/deployment.md lists the mapping. The services
// and deployments grants are added separately when the plugin namespace is the
// sessions namespace.
func sessionRequired() spec {
	return spec{
		"": {
			"pods":                   {"create", "get", "list", "delete"},
			"pods/log":               {"get"},
			"secrets":                {"create", "get", "list", "update", "delete"},
			"configmaps":             {"create", "get", "list", "delete"},
			"persistentvolumeclaims": {"create", "get", "list", "delete"},
		},
		"networking.k8s.io": {"networkpolicies": {"create", "get", "list", "update", "delete"}},
	}
}

// sessionRetained is granted by every chart release so far but has no call
// site: the control plane polls pods with list and never watches them. It
// stays in the Role so an upgrade does not reduce the default grant.
func sessionRetained() spec {
	return spec{"": {"pods": {"watch"}}}
}

// pluginRequired is the RBAC the control plane uses for service plugin
// objects in config.plugins.namespace. Plugin objects are Secrets, Services,
// Deployments, and NetworkPolicies.
func pluginRequired() spec {
	return spec{
		"":                  {"secrets": {"create", "get", "list", "update", "delete"}, "services": {"create", "get", "list", "update", "delete"}},
		"apps":              {"deployments": {"create", "get", "list", "update", "delete"}},
		"networking.k8s.io": {"networkpolicies": {"create", "get", "list", "update", "delete"}},
	}
}

func union(sets ...grantSet) grantSet {
	out := grantSet{}
	for _, set := range sets {
		for g := range set {
			out[g] = struct{}{}
		}
	}
	return out
}

func (s grantSet) sorted() []grant {
	out := make([]grant, 0, len(s))
	for g := range s {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].group != out[j].group {
			return out[i].group < out[j].group
		}
		if out[i].resource != out[j].resource {
			return out[i].resource < out[j].resource
		}
		return out[i].verb < out[j].verb
	})
	return out
}

func (g grant) String() string {
	return fmt.Sprintf("apiGroup %q resource %q verb %q", g.group, g.resource, g.verb)
}

func roleGrants(role object) grantSet {
	out := grantSet{}
	for _, rule := range role.Rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					out[grant{group: group, resource: resource, verb: verb}] = struct{}{}
				}
			}
		}
	}
	return out
}

// expectation is the rendered state a chart case must produce.
type expectation struct {
	release  string
	sessions string
	plugins  string
	roleName string
	saName   string
	rbac     bool
}

// namespaceGrants is the required grant set for one namespace and the wider
// set the check tolerates there.
type namespaceGrants struct {
	required grantSet
	allowed  grantSet
}

// checkObjects returns every invariant violation for one rendered chart. An
// empty result means the objects satisfy the check.
func checkObjects(objects []object, e expectation) []string {
	var out []string
	roles := map[string][]object{}
	bindings := map[string][]object{}
	seen := map[string]bool{}

	for _, o := range objects {
		namespace := o.Metadata.Namespace
		if namespace == "" {
			namespace = e.release
		}
		switch o.Kind {
		case "Role":
			roles[namespace] = append(roles[namespace], o)
		case "RoleBinding":
			bindings[namespace] = append(bindings[namespace], o)
		default:
			continue
		}
		key := o.Kind + "/" + namespace + "/" + o.Metadata.Name
		if seen[key] {
			out = append(out, fmt.Sprintf("duplicate %s", key))
		}
		seen[key] = true
	}

	if !e.rbac {
		if len(roles) != 0 || len(bindings) != 0 {
			out = append(out, fmt.Sprintf("rbac.create=false renders %d Roles and %d RoleBindings", len(roles), len(bindings)))
		}
		return out
	}

	want := map[string]namespaceGrants{}
	sessionGrants := sessionRequired().grants()
	if e.plugins == e.sessions {
		sessionGrants = union(sessionGrants, pluginRequired().grants())
	}
	want[e.sessions] = namespaceGrants{
		required: sessionGrants,
		allowed:  union(sessionGrants, sessionRetained().grants()),
	}
	if e.plugins != e.sessions {
		want[e.plugins] = namespaceGrants{
			required: pluginRequired().grants(),
			allowed:  pluginRequired().grants(),
		}
	}

	for namespace, expected := range want {
		rendered := roles[namespace]
		if len(rendered) == 0 {
			out = append(out, fmt.Sprintf("namespace %q: no Role is rendered", namespace))
		}
		if len(rendered) > 1 {
			out = append(out, fmt.Sprintf("namespace %q: %d Roles are rendered, want 1", namespace, len(rendered)))
		}
		for _, role := range rendered {
			if role.Metadata.Name != e.roleName {
				out = append(out, fmt.Sprintf("namespace %q: Role name %q, want %q", namespace, role.Metadata.Name, e.roleName))
			}
			got := roleGrants(role)
			for g := range expected.required {
				if _, ok := got[g]; !ok {
					out = append(out, fmt.Sprintf("namespace %q: Role %q does not grant %s", namespace, role.Metadata.Name, g))
				}
			}
			for _, g := range got.sorted() {
				if _, ok := expected.allowed[g]; !ok {
					out = append(out, fmt.Sprintf("namespace %q: Role %q grants %s, which the control plane does not use", namespace, role.Metadata.Name, g))
				}
			}
		}

		bound := bindings[namespace]
		if len(bound) == 0 {
			out = append(out, fmt.Sprintf("namespace %q: no RoleBinding is rendered", namespace))
		}
		if len(bound) > 1 {
			out = append(out, fmt.Sprintf("namespace %q: %d RoleBindings are rendered, want 1", namespace, len(bound)))
		}
		for _, binding := range bound {
			if binding.Metadata.Name != e.roleName {
				out = append(out, fmt.Sprintf("namespace %q: RoleBinding name %q, want %q", namespace, binding.Metadata.Name, e.roleName))
			}
			if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != e.roleName {
				out = append(out, fmt.Sprintf("namespace %q: RoleBinding %q refers to %s/%q, want Role/%q", namespace, binding.Metadata.Name, binding.RoleRef.Kind, binding.RoleRef.Name, e.roleName))
			}
			if len(binding.Subjects) != 1 {
				out = append(out, fmt.Sprintf("namespace %q: RoleBinding %q has %d subjects, want 1", namespace, binding.Metadata.Name, len(binding.Subjects)))
				continue
			}
			got := binding.Subjects[0]
			if got.Kind != "ServiceAccount" || got.Name != e.saName || got.Namespace != e.release {
				out = append(out, fmt.Sprintf("namespace %q: RoleBinding %q binds %s %q in %q, want ServiceAccount %q in %q", namespace, binding.Metadata.Name, got.Kind, got.Name, got.Namespace, e.saName, e.release))
			}
		}
	}

	for namespace := range roles {
		if _, ok := want[namespace]; !ok {
			out = append(out, fmt.Sprintf("namespace %q: unexpected Role", namespace))
		}
	}
	for namespace := range bindings {
		if _, ok := want[namespace]; !ok {
			out = append(out, fmt.Sprintf("namespace %q: unexpected RoleBinding", namespace))
		}
	}
	return out
}

// partition is one equality pattern of the three chart namespaces.
type partition struct {
	name     string
	release  string
	sessions string
	plugins  string
}

func partitions() []partition {
	return []partition{
		{name: "all-equal", release: "mayfly", sessions: "mayfly", plugins: "mayfly"},
		{name: "release-equals-sessions", release: "mayfly", sessions: "mayfly", plugins: "plugins"},
		{name: "release-equals-plugins", release: "mayfly", sessions: "sessions", plugins: "mayfly"},
		{name: "sessions-equals-plugins", release: "mayfly", sessions: "sessions", plugins: "sessions"},
		{name: "all-distinct", release: "mayfly", sessions: "sessions", plugins: "plugins"},
	}
}

func helmPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err == nil {
		return path
	}
	if os.Getenv(requireHelmEnv) == "1" {
		t.Fatalf("helm is required when %s=1: %v", requireHelmEnv, err)
	}
	t.Skipf("helm not on PATH; set %s=1 to fail instead", requireHelmEnv)
	return ""
}

func resolveChartDir(t *testing.T) string {
	t.Helper()
	dir := defaultChartDir
	if override := os.Getenv(chartDirEnv); override != "" {
		dir = override
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("resolve chart directory %q: %v", dir, err)
	}
	if _, err := os.Stat(filepath.Join(abs, "Chart.yaml")); err != nil {
		t.Fatalf("chart directory %q has no Chart.yaml: %v", abs, err)
	}
	return abs
}

func templateArgs(dir string, p partition, rbac bool, saName string) []string {
	args := []string{
		"template", "mayfly", dir,
		"--namespace", p.release,
		"--set", "existingSecret=chart-test-secrets",
		"--set-string", "config.sessions.namespace=" + p.sessions,
		"--set-string", "config.plugins.namespace=" + p.plugins,
	}
	if !rbac {
		args = append(args, "--set", "rbac.create=false")
	}
	if saName != "" {
		args = append(args, "--set", "serviceAccount.controlPlaneName="+saName)
	}
	return args
}

func renderChart(t *testing.T, helm, dir string, args []string) []object {
	t.Helper()
	cmd := exec.Command(helm, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return decodeManifests(t, out)
}

func decodeManifests(t *testing.T, data []byte) []object {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var out []object
	for {
		var rendered object
		err := decoder.Decode(&rendered)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode rendered manifest: %v", err)
		}
		if rendered.Kind == "" {
			continue
		}
		out = append(out, rendered)
	}
	return out
}

func reportViolations(t *testing.T, context string, got []string) {
	t.Helper()
	if len(got) == 0 {
		return
	}
	t.Errorf("%s:\n%s", context, strings.Join(got, "\n"))
}

func TestRBACProperties(t *testing.T) {
	helm := helmPath(t)
	dir := resolveChartDir(t)

	cases := []struct {
		saName string
		saWant string
	}{
		{saName: "", saWant: "mayfly"},
		{saName: "mayfly-cp", saWant: "mayfly-cp"},
	}

	for _, p := range partitions() {
		for _, rbac := range []bool{true, false} {
			for _, sa := range cases {
				name := fmt.Sprintf("%s/rbac=%t/sa=%s", p.name, rbac, sa.saWant)
				t.Run(name, func(t *testing.T) {
					objects := renderChart(t, helm, dir, templateArgs(dir, p, rbac, sa.saName))
					reportViolations(t, name, checkObjects(objects, expectation{
						release:  p.release,
						sessions: p.sessions,
						plugins:  p.plugins,
						roleName: "mayfly",
						saName:   sa.saWant,
						rbac:     rbac,
					}))
				})
			}
		}
	}
}

// TestRBACDefaultValues covers the chart as installed with no namespace
// values: sessions and plugins default to the release namespace and one Role
// carries the whole grant set.
func TestRBACDefaultValues(t *testing.T) {
	helm := helmPath(t)
	dir := resolveChartDir(t)

	args := []string{
		"template", "mayfly", dir,
		"--namespace", "mayfly",
		"--set", "existingSecret=chart-test-secrets",
	}
	objects := renderChart(t, helm, dir, args)
	reportViolations(t, "default values", checkObjects(objects, expectation{
		release:  "mayfly",
		sessions: "mayfly",
		plugins:  "mayfly",
		roleName: "mayfly",
		saName:   "mayfly",
		rbac:     true,
	}))
}

// TestRBACEndToEndValues covers the values file the e2e suite installs.
func TestRBACEndToEndValues(t *testing.T) {
	helm := helmPath(t)
	dir := resolveChartDir(t)
	values, err := filepath.Abs("../../e2e/values.yaml")
	if err != nil {
		t.Fatalf("resolve values file: %v", err)
	}

	objects := renderChart(t, helm, dir, []string{
		"template", "mayfly", dir,
		"--namespace", "mayfly",
		"-f", values,
	})
	reportViolations(t, "e2e values", checkObjects(objects, expectation{
		release:  "mayfly",
		sessions: "mayfly",
		plugins:  "mayfly",
		roleName: "mayfly",
		saName:   "mayfly",
		rbac:     true,
	}))
}

// TestRBACPrefixTemplateIsRejected is the negative check. It renders the
// pre-fix rbac.yaml (testdata/rbac-prefix.yaml) and asserts the check catches
// the bug for every partition where a namespace differs. Only the all-equal
// partition may pass, which shows the check reports a defect and not a general
// mismatch.
func TestRBACPrefixTemplateIsRejected(t *testing.T) {
	helm := helmPath(t)
	src := resolveChartDir(t)
	dir := filepath.Join(t.TempDir(), "mayfly")
	copyTree(t, src, dir)

	prefix, err := os.ReadFile("testdata/rbac-prefix.yaml")
	if err != nil {
		t.Fatalf("read pre-fix template: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "rbac.yaml"), prefix, 0o644); err != nil {
		t.Fatalf("write pre-fix template: %v", err)
	}

	for _, p := range partitions() {
		t.Run(p.name, func(t *testing.T) {
			objects := renderChart(t, helm, dir, templateArgs(dir, p, true, ""))
			got := checkObjects(objects, expectation{
				release:  p.release,
				sessions: p.sessions,
				plugins:  p.plugins,
				roleName: "mayfly",
				saName:   "mayfly",
				rbac:     true,
			})
			if p.sessions == p.release && p.plugins == p.sessions {
				if len(got) != 0 {
					t.Errorf("pre-fix template must pass when all three namespaces are equal, got:\n%s", strings.Join(got, "\n"))
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("pre-fix template passed the check; the check does not bite")
			}
			if p.sessions == p.release {
				return
			}
			quoted := fmt.Sprintf("namespace %q", p.sessions)
			for _, violation := range got {
				if strings.Contains(violation, quoted) {
					return
				}
			}
			t.Errorf("pre-fix template fails, but no violation names the sessions namespace %q:\n%s", p.sessions, strings.Join(got, "\n"))
		})
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatalf("copy chart %q: %v", src, err)
	}
}
