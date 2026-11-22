//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSessionResumesAfterPodDeletion force-deletes the running session pod
// after its first tool call completed. A replacement must resume the journaled
// turn, the user message must stay single, and the completed tool call must
// not run a second time.
func TestSessionResumesAfterPodDeletion(t *testing.T) {
	resumeAfterPodDelete(t, []string{"--grace-period=0", "--force"})
}

// TestSessionResumesAfterGracefulPodDeletion uses the default grace period, so
// the kubelet sends SIGTERM before it kills the container. That is how a real
// drain, eviction, or kubectl delete ends a pod: the runtime must exit without
// finalizing, and the control plane must start a replacement.
func TestSessionResumesAfterGracefulPodDeletion(t *testing.T) {
	resumeAfterPodDelete(t, nil)
}

func resumeAfterPodDelete(t *testing.T, deleteArgs []string) {
	t.Helper()
	mcpBase, stop := env.forwardService(t, "mcpstub", 9100)
	defer stop()
	resetMCPCounts(t, mcpBase)

	prompt := "resume-slow-final pod deletion " + randomHex(4)
	s := env.createSession(t, map[string]any{"prompt": prompt, "agent": "e2e"})
	env.waitStatus(t, s.ID, "running", 3*time.Minute)

	env.waitEvent(t, s.ID, "completed MCP tool call", func(ev event) bool {
		tool, _ := ev.Data["tool"].(string)
		return ev.Type == "tool_result" && strings.HasPrefix(tool, "mcp_")
	})
	// No wait needed: the control plane publishes tool_result only after the
	// step is appended to the journal (session.AppendJournal).

	pods := env.podNames(t, s.ID)
	if len(pods) != 1 {
		t.Fatalf("session pods = %v, want one", pods)
	}
	args := append([]string{"delete", "pod", pods[0]}, deleteArgs...)
	if _, err := env.kubectl(args...); err != nil {
		t.Fatal(err)
	}

	eventually(t, 90*time.Second, "replacement pod", func() (bool, string) {
		names := env.podNames(t, s.ID)
		for _, name := range names {
			if strings.HasSuffix(name, "-r1") {
				return true, strings.Join(names, ", ")
			}
		}
		got, _ := env.getSession(t, s.ID)
		return false, fmt.Sprintf("pods=%v status=%s error=%q", names, got.Status, got.Error)
	})

	env.waitEvent(t, s.ID, "resumed assistant reply", func(ev event) bool {
		return ev.Type == "assistant_message" && strings.Contains(ev.Text, "Completed.")
	})
	if got := mcpCallCount(t, mcpBase, "echo"); got != 1 {
		t.Fatalf("echo tool executed %d times, want 1", got)
	}
	if got := userMessageCount(t, s.ID, prompt); got != 1 {
		t.Fatalf("user message appears %d times, want 1", got)
	}
	got, _ := env.getSession(t, s.ID)
	if got.Status != "running" {
		t.Fatalf("session status after resume = %s", got.Status)
	}
}

// TestControlPlaneRestartContinuesTurn replaces mayflyd while the final model
// call is in flight. The runtime's transient retry, the persisted journal, and
// journal event publishing must finish the turn without repeating the tool
// call or the user message.
func TestControlPlaneRestartContinuesTurn(t *testing.T) {
	mcpBase, stop := env.forwardService(t, "mcpstub", 9100)
	defer stop()
	resetMCPCounts(t, mcpBase)

	prompt := "resume-slow-final control plane " + randomHex(4)
	s := env.createSession(t, map[string]any{"prompt": prompt, "agent": "e2e"})
	env.waitStatus(t, s.ID, "running", 3*time.Minute)
	env.waitEvent(t, s.ID, "completed MCP tool call", func(ev event) bool {
		tool, _ := ev.Data["tool"].(string)
		return ev.Type == "tool_result" && strings.HasPrefix(tool, "mcp_")
	})
	// No wait needed: the control plane publishes tool_result only after the
	// step is appended to the journal (session.AppendJournal).

	env.killControlPlane(t)

	env.waitEvent(t, s.ID, "assistant reply after control-plane restart", func(ev event) bool {
		return ev.Type == "assistant_message" && strings.Contains(ev.Text, "Completed.")
	})
	pods := env.podNames(t, s.ID)
	if len(pods) != 1 || strings.Contains(strings.TrimPrefix(pods[0], "mayfly-"+s.ID), "-r") {
		t.Fatalf("the control-plane restart should keep the session pod, got %v", pods)
	}
	if got := mcpCallCount(t, mcpBase, "echo"); got != 1 {
		t.Fatalf("echo tool executed %d times, want 1", got)
	}
	if got := userMessageCount(t, s.ID, prompt); got != 1 {
		t.Fatalf("user message appears %d times, want 1", got)
	}
}

func resetMCPCounts(t *testing.T, base string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/counts/reset", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := env.http.Do(req)
	if err != nil {
		t.Fatalf("reset mcp counts: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset mcp counts: %s", resp.Status)
	}
}

func mcpCallCount(t *testing.T, base, tool string) int {
	t.Helper()
	resp, err := env.http.Get(base + "/counts")
	if err != nil {
		t.Fatalf("read mcp counts: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read mcp counts: %s", resp.Status)
	}
	var out struct {
		Counts map[string]int `json:"counts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode mcp counts: %v", err)
	}
	return out.Counts[tool]
}

func userMessageCount(t *testing.T, id, prompt string) int {
	t.Helper()
	count := 0
	for _, ev := range env.events(t, id) {
		if ev.Type == "user_message" && ev.Text == prompt {
			count++
		}
	}
	return count
}

// TestControlPlaneRestartKeepsQueuedMessage posts a human message while the
// final model call of the first turn is in flight, then replaces the control
// plane. The message is acknowledged with 202 but cannot start before the turn
// ends, so only the durable pending record can carry it across the restart.
// Adoption must redeliver it once, under its reserved turn key.
func TestControlPlaneRestartKeepsQueuedMessage(t *testing.T) {
	mcpBase, stop := env.forwardService(t, "mcpstub", 9100)
	defer stop()
	resetMCPCounts(t, mcpBase)

	prompt := "resume-slow-final queued " + randomHex(4)
	queued := "queued behind the running turn " + randomHex(4)
	s := env.createSession(t, map[string]any{"prompt": prompt, "agent": "e2e"})
	env.waitStatus(t, s.ID, "running", 3*time.Minute)
	env.waitEvent(t, s.ID, "completed MCP tool call", func(ev event) bool {
		tool, _ := ev.Data["tool"].(string)
		return ev.Type == "tool_result" && strings.HasPrefix(tool, "mcp_")
	})

	// The stub model sleeps five seconds before this turn's final reply. The
	// 202 is the durability point: the reply returns only after the journal
	// holds the message.
	r := env.admin(t, http.MethodPost, "/api/v1/sessions/"+s.ID+"/messages", map[string]string{"text": queued})
	expectStatus(t, r, http.StatusAccepted, "queued message")
	env.killControlPlane(t)

	env.waitEvent(t, s.ID, "assistant reply after control-plane restart", func(ev event) bool {
		return ev.Type == "assistant_message" && strings.Contains(ev.Text, "Completed.")
	})
	eventually(t, 3*time.Minute, "queued message handled once", func() (bool, string) {
		users, replies := 0, 0
		for _, ev := range env.events(t, s.ID) {
			if ev.Type == "user_message" && ev.Text == queued {
				users++
			}
			if ev.Type == "assistant_message" && strings.Contains(ev.Text, "Completed.") {
				replies++
			}
		}
		got, _ := env.getSession(t, s.ID)
		return users == 1 && replies >= 2, fmt.Sprintf("queued user messages=%d assistant replies=%d status=%s", users, replies, got.Status)
	})
	if got := userMessageCount(t, s.ID, prompt); got != 1 {
		t.Fatalf("initial prompt appears %d times, want 1", got)
	}
	if got := mcpCallCount(t, mcpBase, "echo"); got != 1 {
		t.Fatalf("echo tool executed %d times, want 1", got)
	}
}

// TestControlPlaneRestartKeepsInitialPrompt creates a session and replaces the
// control plane before its pod has connected. The initial prompt is accepted
// before the pod exists, so only the durable pending record or the journal can
// carry it across the restart. Adoption must deliver it exactly once.
func TestControlPlaneRestartKeepsInitialPrompt(t *testing.T) {
	prompt := "initial prompt across restart " + randomHex(4)
	s := env.createSession(t, map[string]any{"prompt": prompt, "agent": "e2e"})
	env.killControlPlane(t)

	env.waitEvent(t, s.ID, "initial prompt after control-plane restart", func(ev event) bool {
		return ev.Type == "user_message" && ev.Text == prompt
	})
	env.waitStatus(t, s.ID, "running", 3*time.Minute)
	if got := userMessageCount(t, s.ID, prompt); got != 1 {
		t.Fatalf("initial prompt appears %d times, want 1", got)
	}
}
