package session

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
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
