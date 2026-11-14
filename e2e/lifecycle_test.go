//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
)

// TestWebhookSessionLifecycle follows one webhook from delivery to cleanup:
// queue, session pod, agent turn through the LLM and MCP proxies, live attach,
// a human follow-up, graceful close, artifact export, and object deletion.
func TestWebhookSessionLifecycle(t *testing.T) {
	key := "lifecycle-" + randomHex(6)
	payload := map[string]any{
		"prompt": "Summarize the e2e lifecycle run.",
		"title":  "e2e lifecycle",
		"agent":  "e2e",
	}
	r := env.webhook(t, payload, key)
	expectStatus(t, r, http.StatusAccepted, "signed webhook")
	acc := decode[accepted](t, r)
	if !acc.Accepted || acc.Duplicate || acc.Status != "queued" || acc.Delivery != key || acc.JobID == "" {
		t.Fatalf("accept response = %+v", acc)
	}
	r = env.webhook(t, payload, key)
	expectStatus(t, r, http.StatusAccepted, "retried delivery")
	if dup := decode[accepted](t, r); !dup.Duplicate || dup.JobID != acc.JobID {
		t.Fatalf("retried delivery was not deduplicated: %+v", dup)
	}

	job := env.waitJob(t, acc.JobID, "done", 2*time.Minute)
	if job.SessionID == "" {
		t.Fatalf("done job has no session: %+v", job)
	}
	id := job.SessionID
	env.cleanupSession(t, id)

	s := env.waitStatus(t, id, "running", 3*time.Minute)
	if s.Agent != "e2e" || s.Harness != "mayfly" || s.Title != "e2e lifecycle" {
		t.Fatalf("session = %+v", s)
	}
	if s.Trigger.Kind != "generic" || s.Trigger.JobID != acc.JobID || s.Trigger.Delivery != key {
		t.Fatalf("session trigger = %+v", s.Trigger)
	}

	t.Run("pod is hardened", func(t *testing.T) { checkPodHardening(t, "mayfly-"+id) })

	env.waitEvent(t, id, "assistant reply", func(ev event) bool {
		return ev.Type == "assistant_message" && ev.Text != ""
	})
	env.waitEvent(t, id, "MCP tool result through the proxy", func(ev event) bool {
		tool, _ := ev.Data["tool"].(string)
		return ev.Type == "tool_result" && strings.HasPrefix(tool, "mcp_") && strings.Contains(ev.Text, "echo: hello from mayfly")
	})

	t.Run("attach over WebSocket", func(t *testing.T) { attachAndFollowUp(t, id) })

	r = env.admin(t, http.MethodPost, "/api/v1/sessions/"+id+"/close", map[string]string{"reason": "e2e"})
	expectStatus(t, r, http.StatusAccepted, "close")
	s = env.waitStatus(t, id, "completed", 2*time.Minute)
	if s.Artifact == nil || s.Artifact.Partial || s.Artifact.Size == 0 {
		t.Fatalf("completed session artifact = %+v", s.Artifact)
	}

	files := env.artifactFiles(t, id)
	if !strings.Contains(string(files["files/report.md"]), "Mayfly session report") {
		t.Fatalf("bundle is missing the workspace report; files: %v", keys(files))
	}
	if !strings.Contains(string(files["traces.jsonl"]), "echo: hello from mayfly") {
		t.Fatal("bundle traces are missing the MCP call")
	}
	m := manifestOf(t, files)
	if m["status"] != "completed" || m["partial"] != false || m["agent"] != "e2e" || m["harness"] != "mayfly" {
		t.Fatalf("manifest = %v", m)
	}

	env.waitObjectsGone(t, id)
	r = env.admin(t, http.MethodPost, "/api/v1/sessions/"+id+"/messages", map[string]string{"text": "too late"})
	expectStatus(t, r, http.StatusConflict, "message to a closed session")
	expectStatus(t, env.admin(t, http.MethodGet, "/api/v1/sessions/"+id+"/artifact", nil), http.StatusOK, "artifact after cleanup")
}

func checkPodHardening(t *testing.T, name string) {
	raw, err := env.kubectl("get", "pod", name, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := json.Unmarshal([]byte(raw), &pod); err != nil {
		t.Fatal(err)
	}
	spec := pod.Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("service account token is mounted")
	}
	if spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
		t.Error("service links are enabled")
	}
	if spec.SecurityContext == nil || spec.SecurityContext.RunAsNonRoot == nil || !*spec.SecurityContext.RunAsNonRoot {
		t.Error("pod may run as root")
	}
	if spec.ActiveDeadlineSeconds == nil {
		t.Error("pod has no activeDeadlineSeconds backstop")
	}
	if len(spec.Containers) != 1 {
		t.Fatalf("pod has %d containers", len(spec.Containers))
	}
	c := spec.Containers[0]
	sc := c.SecurityContext
	switch {
	case sc == nil:
		t.Fatal("container has no security context")
	case sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem:
		t.Error("root filesystem is writable")
	case sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
		t.Error("privilege escalation is allowed")
	case sc.Capabilities == nil || !slices.Contains(sc.Capabilities.Drop, "ALL"):
		t.Error("capabilities are not dropped")
	case sc.RunAsUser == nil || *sc.RunAsUser != 65532:
		t.Error("container does not run as 65532")
	}
	if c.Resources.Limits.Memory().IsZero() || c.Resources.Limits.Cpu().IsZero() {
		t.Error("container has no CPU or memory limit")
	}
	for _, e := range c.Env {
		if strings.Contains(strings.ToUpper(e.Name), "OPENAI") || e.Name == "MAYFLY_API_TOKEN" {
			t.Errorf("control plane secret %s leaked into the session pod", e.Name)
		}
	}
	session := pod.Labels["mayfly.io/session"]
	if session == "" {
		t.Error("pod has no mayfly.io/session label")
	}
	// The policy follows the session, not the pod name, so a replacement pod
	// keeps it. Its name is mayfly-<id>-netpol.
	if _, err := env.kubectl("get", "networkpolicy", "mayfly-"+session+"-netpol"); err != nil {
		t.Errorf("session NetworkPolicy missing: %v", err)
	}
}

type wsMessage struct {
	Type    string  `json:"type"`
	Event   event   `json:"event"`
	Session session `json:"session"`
	Message string  `json:"message"`
}

// attachAndFollowUp attaches with a one-time ticket, sees the replayed
// thread, sends a human message, and waits for the agent to answer it.
func attachAndFollowUp(t *testing.T, id string) {
	r := env.admin(t, http.MethodPost, "/api/v1/sessions/"+id+"/attach-ticket", map[string]string{"actor": "e2e-human"})
	expectStatus(t, r, http.StatusOK, "attach ticket")
	ticket := decode[struct {
		Ticket string `json:"ticket"`
	}](t, r).Ticket
	wsURL := "ws" + strings.TrimPrefix(env.url("/api/v1/sessions/"+id+"/ws?ticket="+ticket), "http")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer conn.Close()
	if second, resp, err := websocket.DefaultDialer.Dial(wsURL, nil); err == nil {
		second.Close()
		t.Error("an attach ticket was accepted twice")
	} else if resp == nil || resp.StatusCode < 400 {
		t.Errorf("reusing a ticket failed without an HTTP error: %v", err)
	}

	read := func(what string, match func(wsMessage) bool) wsMessage {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		for {
			var msg wsMessage
			if err := conn.ReadJSON(&msg); err != nil {
				t.Fatalf("waiting for %s: %v", what, err)
			}
			if msg.Type == "error" {
				t.Fatalf("waiting for %s: server error %q", what, msg.Message)
			}
			if match(msg) {
				return msg
			}
		}
	}
	hello := read("hello", func(m wsMessage) bool { return m.Type == "hello" })
	if hello.Session.ID != id {
		t.Fatalf("hello for session %q", hello.Session.ID)
	}
	read("replayed assistant reply", func(m wsMessage) bool {
		return m.Type == "event" && m.Event.Type == "assistant_message"
	})

	text := "Follow-up from the e2e suite " + randomHex(4)
	if err := conn.WriteJSON(map[string]string{"type": "user_message", "text": text}); err != nil {
		t.Fatal(err)
	}
	mine := read("echo of the human message", func(m wsMessage) bool {
		return m.Type == "event" && m.Event.Type == "user_message" && m.Event.Text == text
	})
	if mine.Event.Actor != "e2e-human" {
		t.Errorf("human message actor = %q, want the ticket's actor", mine.Event.Actor)
	}
	read("agent answer to the follow-up", func(m wsMessage) bool {
		return m.Type == "event" && m.Event.Type == "assistant_message" && m.Event.Seq > mine.Event.Seq
	})
}

// TestSessionTTLExpiry checks that a session past its TTL is closed, exports
// a bundle that records why, and has its objects deleted.
func TestSessionTTLExpiry(t *testing.T) {
	s := env.createSession(t, map[string]any{"prompt": "TTL probe", "agent": "e2e", "ttl": "15s"})
	s = env.waitStatus(t, s.ID, "expired", 3*time.Minute)
	if s.Artifact == nil {
		t.Fatal("expired session has no artifact")
	}
	if m := manifestOf(t, env.artifactFiles(t, s.ID)); m["reason"] != "expired" {
		t.Fatalf("manifest reason = %v, want expired", m["reason"])
	}
	env.waitObjectsGone(t, s.ID)
}

// TestSessionImagePullFailure checks that a session whose image cannot be
// pulled fails with the reason instead of hanging until its TTL.
func TestSessionImagePullFailure(t *testing.T) {
	s := env.createSession(t, map[string]any{"prompt": "bad image probe", "agent": "broken"})
	s = env.waitStatus(t, s.ID, "failed", 3*time.Minute)
	if !strings.Contains(s.Error, "Image") && !strings.Contains(s.Error, "image") {
		t.Fatalf("failure does not name the image problem: %q", s.Error)
	}
	env.waitObjectsGone(t, s.ID)
}

// TestDeleteSession checks that DELETE stops a running session at once.
func TestDeleteSession(t *testing.T) {
	s := env.createSession(t, map[string]any{"prompt": "delete probe", "agent": "e2e"})
	env.waitStatus(t, s.ID, "running", 3*time.Minute)
	expectStatus(t, env.admin(t, http.MethodDelete, "/api/v1/sessions/"+s.ID, nil), http.StatusNoContent, "delete")
	got, _ := env.getSession(t, s.ID)
	if got.Status != "failed" {
		t.Fatalf("deleted session status = %s", got.Status)
	}
	env.waitObjectsGone(t, s.ID)
}
