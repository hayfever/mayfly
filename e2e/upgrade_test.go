//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	// upgradeRelease is a second release in the same namespace, so the
	// upgrade never rewrites the release under test.
	upgradeRelease = "mayfly-upgrade"
	// chartRepository and imagePrefix publish every release; see the README.
	chartRepository = "oci://ghcr.io/hayfever/charts/mayfly"
	imagePrefix     = "ghcr.io/hayfever"
)

// TestUpgradeFromLastRelease installs the last released chart and images,
// fills the session slots so a webhook job stays queued, upgrades the release
// to the chart and images built from HEAD, and checks that the old sessions,
// the queued job, and the queue's ability to run it all survive.
//
// The test needs a released version and, when the GHCR packages are private, a
// pull secret for the cluster. See docs/testing.md.
func TestUpgradeFromLastRelease(t *testing.T) {
	from := getenv("MAYFLY_E2E_UPGRADE_FROM", "")
	if from == "" {
		t.Skip("set MAYFLY_E2E_UPGRADE_FROM to a released chart version, for example 0.1.0-rc.2")
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Dir(root)
	chart := getenv("MAYFLY_E2E_CHART_REPOSITORY", chartRepository)
	prefix := strings.TrimRight(getenv("MAYFLY_E2E_IMAGE_PREFIX", imagePrefix), "/")
	pullSecret := getenv("MAYFLY_E2E_PULL_SECRET", "")
	headControlPlane := env.podImage(t, env.release)
	headAgent := env.agentImage

	up := env.forRelease(upgradeRelease)
	var sessions []session
	t.Cleanup(func() {
		up.cleanupUpgrade(t, root, sessions)
	})

	t.Logf("installing %s version %s", chart, from)
	up.installRelease(t, root, chart, []string{"--version", from}, prefix+"/mayflyd:"+from, prefix+"/mayfly-agent:"+from, pullSecret)
	up.connectAndWait(t)

	// Fill sessions.max_concurrent (3 in e2e/values.yaml) so the job below
	// cannot start before the upgrade.
	for i := range 3 {
		title := fmt.Sprintf("upgrade probe %d", i)
		r := up.admin(t, http.MethodPost, "/api/v1/sessions", map[string]any{
			"prompt": "upgrade probe: hold a session slot",
			"title":  title,
			"agent":  "e2e",
			"ttl":    "15m",
		})
		expectStatus(t, r, http.StatusCreated, "create a session on the released release")
		s := decode[struct {
			Session session `json:"session"`
		}](t, r).Session
		sessions = append(sessions, s)
		up.waitStatus(t, s.ID, "running", 3*time.Minute)
	}

	r := up.webhook(t, map[string]any{"prompt": "upgrade probe: queued job", "agent": "e2e"}, "")
	expectStatus(t, r, http.StatusAccepted, "webhook on the released release")
	jobID := decode[accepted](t, r).JobID
	// With every slot taken the worker polls the job but must not start it or
	// spend an attempt. The state can be queued or leased.
	eventually(t, time.Minute, "queued job visible", func() (bool, string) {
		j, r := up.job(t, jobID)
		if r.Status != http.StatusOK {
			return false, r.String()
		}
		if j.State == "done" || j.SessionID != "" || j.Attempts != 0 {
			t.Fatalf("job ran while the session cap is full: %+v", j)
		}
		return true, j.State
	})

	t.Logf("upgrading %s to the chart and images built from HEAD", upgradeRelease)
	up.installRelease(t, root, filepath.Join(root, "deploy", "helm", "mayfly"), nil, headControlPlane, headAgent, pullSecret)
	up.connectAndWait(t)

	// The released session records survive the upgrade with their fields.
	for _, want := range sessions {
		var got session
		eventually(t, 2*time.Minute, "session "+want.ID+" adopted after the upgrade", func() (bool, string) {
			var r response
			got, r = up.getSession(t, want.ID)
			if r.Status != http.StatusOK {
				return false, r.String()
			}
			return got.Trigger.Kind == "adopted", "trigger " + got.Trigger.Kind
		})
		if got.Title != want.Title || got.Agent != want.Agent {
			t.Errorf("session %s after the upgrade = %+v, want title %q and agent %q", want.ID, got, want.Title, want.Agent)
		}
		// The journaled thread came back with the session, not just its record.
		eventually(t, time.Minute, "journal of session "+want.ID+" after the upgrade", func() (bool, string) {
			for _, ev := range up.events(t, want.ID) {
				if ev.Type == "user_message" && ev.Text == "upgrade probe: hold a session slot" {
					return true, ""
				}
			}
			return false, "the original user message is missing"
		})
	}
	// A list client, not just a point read, sees the old sessions.
	ids, err := up.trySessionIDs()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range sessions {
		if !slices.Contains(ids, want.ID) {
			t.Errorf("session %s is missing from the session list after the upgrade: %v", want.ID, ids)
		}
	}

	// The job survived. It was queued before the upgrade; the upgraded
	// control plane may have run it only if a slot is free, which it is not.
	j, r := up.job(t, jobID)
	if r.Status != http.StatusOK {
		t.Fatalf("queued job lost in the upgrade: %s", r)
	}
	switch j.State {
	case "queued", "leased", "done":
	default:
		t.Fatalf("job %s state after the upgrade = %q, want queued, leased, or done", jobID, j.State)
	}

	// Free one slot: the upgraded control plane must run the old job and
	// start its session from the queue record.
	expectStatus(t, up.admin(t, http.MethodDelete, "/api/v1/sessions/"+sessions[0].ID, nil), http.StatusNoContent, "free a session slot")
	j = up.waitJob(t, jobID, "done", 2*time.Minute)
	s, r := up.getSession(t, j.SessionID)
	if r.Status != http.StatusOK {
		t.Fatalf("session %s of the queued job is unreadable after the upgrade: %s", j.SessionID, r)
	}
	if s.Trigger.JobID != jobID {
		t.Errorf("session %s trigger job id = %q, want %q", s.ID, s.Trigger.JobID, jobID)
	}
	sessions = append(sessions, s)
}

// install runs helm upgrade --install for the upgrade release with the e2e
// values, the e2e agent, and the incident-triage skill. controlPlane and agent
// are full image references with a tag or digest.
func (c *cluster) installRelease(t *testing.T, root, chart string, extra []string, controlPlane, agent, pullSecret string) {
	t.Helper()
	cpRepo, cpTag, ok := splitImage(controlPlane)
	if !ok {
		t.Fatalf("control-plane image %q has no tag", controlPlane)
	}
	agentRepo, agentTag, ok := splitImage(agent)
	if !ok {
		t.Fatalf("agent image %q has no tag", agent)
	}
	args := []string{
		"upgrade", "--install", c.release, chart,
		"--namespace", c.namespace, "--create-namespace",
		"--wait", "--timeout", "5m",
		"-f", filepath.Join(root, "e2e", "values.yaml"),
		"--set", "image.repository=" + cpRepo,
		"--set", "image.tag=" + cpTag,
		"--set", "agentImage.repository=" + agentRepo,
		"--set", "agentImage.tag=" + agentTag,
		"--set-file", `agents.files.e2e\.yaml=` + filepath.Join(root, "e2e", "agents.yaml"),
		"--set-file", `skills.files.incident-triage\.md=` + filepath.Join(root, "examples", "skills", "incident-triage.md"),
	}
	if c.kubeContext != "" {
		args = append(args, "--kube-context", c.kubeContext)
	}
	if pullSecret != "" {
		args = append(args, "--set", "imagePullSecrets[0].name="+pullSecret)
	}
	args = append(args, extra...)
	cmd := exec.Command("helm", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helm %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

// forRelease is a connection to another release in the same namespace, with
// its own port-forward. It borrows the release credentials.
func (c *cluster) forRelease(release string) *cluster {
	return &cluster{
		kubeContext:   c.kubeContext,
		namespace:     c.namespace,
		release:       release,
		service:       release,
		token:         c.token,
		webhookSecret: c.webhookSecret,
		slackSecret:   c.slackSecret,
		agentImage:    c.agentImage,
		http:          c.http,
	}
}

// connectAndWait opens a port-forward to the release and waits for /readyz.
func (c *cluster) connectAndWait(t *testing.T) {
	t.Helper()
	c.stopPortForward()
	if _, err := c.startPortForward(); err != nil {
		t.Fatal(err)
	}
	if err := c.waitReady(2 * time.Minute); err != nil {
		t.Fatal(err)
	}
}

// podImage returns the image of the first container of a deployment.
func (c *cluster) podImage(t *testing.T, deployment string) string {
	t.Helper()
	out, err := c.kubectl("get", "deployment", deployment, "-o", "jsonpath={.spec.template.spec.containers[0].image}")
	if err != nil {
		t.Fatal(err)
	}
	image := strings.TrimSpace(out)
	if image == "" {
		t.Fatalf("deployment %s has no container image", deployment)
	}
	return image
}

// splitImage splits repo:tag at the last colon, keeping a registry port in the
// repository.
func splitImage(image string) (repo, tag string, ok bool) {
	i := strings.LastIndex(image, ":")
	if i <= strings.LastIndex(image, "/") {
		return "", "", false
	}
	return image[:i], image[i+1:], true
}

// trySessionIDs lists the sessions of the release without failing the test.
// The API returns {"sessions": [...]} today and moves to
// {"items": [...], "next_cursor": ...}; the upgrade test must read either shape
// while that changes.
func (c *cluster) trySessionIDs() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, c.url("/api/v1/sessions"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/v1/sessions: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return sessionListIDs(body)
}

// deleteSession deletes a session without failing the test; cleanup must keep
// going when the release is already broken.
func (c *cluster) deleteSession(id string) {
	req, err := http.NewRequest(http.MethodDelete, c.url("/api/v1/sessions/"+id), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// cleanupUpgrade removes the upgrade release, its state, and its sessions so
// the suite's leak check starts clean. The release is a separate control plane,
// so every session it lists belongs to this test.
func (c *cluster) cleanupUpgrade(t *testing.T, root string, sessions []session) {
	t.Helper()
	ids := make([]string, 0, len(sessions)+1)
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	if listed, err := c.trySessionIDs(); err == nil {
		ids = append(ids, listed...)
	} else {
		t.Logf("list sessions of %s during cleanup: %v", c.release, err)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		c.deleteSession(id)
	}
	for _, id := range ids {
		deadline := time.Now().Add(90 * time.Second)
		for {
			objs, err := c.sessionObjects(id)
			if err == nil && len(objs) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("objects of session %s are still present after the upgrade test: %v (%v)", id, objs, err)
				break
			}
			time.Sleep(2 * time.Second)
		}
	}
	c.stopPortForward()
	args := []string{"uninstall", c.release, "--namespace", c.namespace, "--wait", "--timeout", "2m"}
	if c.kubeContext != "" {
		args = append(args, "--kube-context", c.kubeContext)
	}
	cmd := exec.Command("helm", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("helm uninstall %s: %v: %s", c.release, err, strings.TrimSpace(string(out)))
	}
	if _, err := c.kubectl("delete", "pvc", c.release+"-state", "--ignore-not-found"); err != nil {
		t.Logf("delete pvc of %s: %v", c.release, err)
	}
}
