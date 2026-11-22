package session

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"

	"github.com/hayfever/mayfly/internal/model"
)

func TestPodFailureDetailReportsWaitingAndTerminated(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ImagePullBackOff",
					Message: "Back-off pulling image",
				}},
			}},
		},
	}
	reason, message := podFailureDetail(pod)
	if reason != "ImagePullBackOff" || message != "Back-off pulling image" {
		t.Fatalf("waiting detail = %q %q", reason, message)
	}

	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Reason:  "StartError",
		Message: "exec: no such file",
	}}
	reason, message = podFailureDetail(pod)
	if reason != "StartError" || message != "exec: no such file" {
		t.Fatalf("terminated detail = %q %q", reason, message)
	}

	pod.Status = corev1.PodStatus{}
	if reason, message := podFailureDetail(pod); reason != "" || message != "" {
		t.Fatalf("empty detail = %q %q", reason, message)
	}
}

func TestEmitRuntimeEventsTruncatesOversizedFields(t *testing.T) {
	m := testManager(t)
	m.cfg.Sessions.MaxEventBytes = 64
	inst := &instance{
		sess:      model.Session{ID: "s1", Status: model.StatusRunning},
		tokenHash: hashToken("token"),
	}
	m.sessions["s1"] = inst
	m.tokenIndex[inst.tokenHash] = "s1"

	long := strings.Repeat("日", 100) // 300 bytes of valid UTF-8
	stored, err := m.EmitRuntimeEvents("s1", "token", []model.Event{
		{Type: model.EventAssistantMessage, Text: long},
		{Type: model.EventToolCall, Text: long, Data: model.ToolCallData{
			Tool: long, CallID: "call_1", Arguments: long,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored %d events, want 2", len(stored))
	}
	for _, ev := range stored {
		if len(ev.Text) > 64 {
			t.Fatalf("%s text is %d bytes, over the 64-byte cap", ev.Type, len(ev.Text))
		}
		if !utf8.ValidString(ev.Text) {
			t.Fatalf("%s text is not valid UTF-8", ev.Type)
		}
		if !strings.HasPrefix(long, strings.TrimSuffix(ev.Text, eventTruncationMark)) {
			t.Fatalf("%s text is not a prefix of the input: %q", ev.Type, ev.Text)
		}
		if !strings.HasSuffix(ev.Text, eventTruncationMark) {
			t.Fatalf("%s text has no truncation marker: %q", ev.Type, ev.Text)
		}
	}
	call, ok := model.DataAs[model.ToolCallData](stored[1].Data)
	if !ok {
		t.Fatalf("tool call data = %T", stored[1].Data)
	}
	for name, value := range map[string]string{"tool": call.Tool, "arguments": call.Arguments} {
		if len(value) > 64 || !utf8.ValidString(value) {
			t.Fatalf("%s = %d bytes, want at most 64 and valid UTF-8", name, len(value))
		}
	}
	retained := m.hub.EventsAfter("s1", 0)
	if len(retained) != 2 || len(retained[0].Text) > 64 || len(retained[1].Text) > 64 {
		t.Fatalf("retained events = %+v", retained)
	}
}

func TestCreateTruncatesOversizedTitle(t *testing.T) {
	m := testManager(t)
	m.runner = nopRunner{}
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	explicit, err := m.Create(ctx, CreateOptions{Prompt: "hello", Title: strings.Repeat("t", 5000)})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(explicit.Title); got != MaxTitleBytes {
		t.Fatalf("explicit title is %d bytes, want %d", got, MaxTitleBytes)
	}
	derived, err := m.Create(ctx, CreateOptions{Prompt: strings.Repeat("p", 1000)})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(derived.Title); got > MaxTitleBytes {
		t.Fatalf("derived title is %d bytes, over %d", got, MaxTitleBytes)
	}
	if !utf8.ValidString(derived.Title) {
		t.Fatalf("derived title is not valid UTF-8: %q", derived.Title)
	}
}

// TestCreateSanitizesInvalidTitle checks that a prompt with invalid UTF-8
// cannot put an invalid title in the session record: the derived title takes
// the same replacement rune the webhook triggers use.
func TestCreateSanitizesInvalidTitle(t *testing.T) {
	m := testManager(t)
	m.runner = nopRunner{}
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}
	sess, err := m.Create(context.Background(), CreateOptions{Prompt: "bad\xffprompt"})
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(sess.Title) {
		t.Fatalf("title is not valid UTF-8: %q", sess.Title)
	}
	if !strings.Contains(sess.Title, "\uFFFD") {
		t.Fatalf("title = %q, want a replacement rune", sess.Title)
	}
}

func TestValidateRejectsOversizedDeliveryKey(t *testing.T) {
	m := testManager(t)
	m.runner = nopRunner{}
	if err := m.loadAgents(); err != nil {
		t.Fatal(err)
	}
	err := m.Validate(CreateOptions{
		Prompt:  "hello",
		Trigger: model.TriggerInfo{Delivery: strings.Repeat("d", maxDeliveryKeyBytes+1)},
	})
	if err == nil || !strings.Contains(err.Error(), "delivery key") {
		t.Fatalf("oversized delivery key error = %v", err)
	}
	if err := m.Validate(CreateOptions{
		Prompt:         "hello",
		IdempotencyKey: strings.Repeat("k", maxDeliveryKeyBytes+1),
	}); err == nil || !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("oversized idempotency key error = %v", err)
	}
}
