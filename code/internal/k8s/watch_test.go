package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podWith(mut func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{}
	p.Status.Phase = corev1.PodPending
	mut(p)
	return p
}

func ready(p *corev1.Pod) {
	p.Status.Phase = corev1.PodRunning
	p.Status.PodIP = "10.0.0.7"
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
}

func waiting(reason string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "details"}},
		}}
	}
}

func TestStateOf(t *testing.T) {
	now := metav1.Now()
	for _, tc := range []struct {
		name       string
		pod        *corev1.Pod
		want       PodPhase
		wantReason string
	}{
		{"just scheduled", podWith(func(*corev1.Pod) {}), PodStarting, ""},
		{"pulling image", podWith(waiting("ContainerCreating")), PodStarting, ""},
		{"running, not ready yet", podWith(func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning }), PodStarting, ""},
		{"ready", podWith(ready), PodReady, ""},
		{"bad image", podWith(waiting("ImagePullBackOff")), PodFailed, "ImagePullBackOff"},
		{"deadline", podWith(func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed; p.Status.Reason = "DeadlineExceeded" }), PodExpired, ""},
		{"evicted", podWith(func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.Reason = "Evicted"
			p.Status.Message = "low on memory"
		}), PodFailed, "Evicted: low on memory"},
		{"container exited", podWith(func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
			}}
		}), PodFailed, "code 137 (OOMKilled)"},
		{"succeeded", podWith(func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }), PodFailed, "exited"},
		{"being deleted", podWith(func(p *corev1.Pod) { ready(p); p.DeletionTimestamp = &now }), PodTerminating, ""},
	} {
		got := StateOf(tc.pod)
		if got.Phase != tc.want || !strings.Contains(got.Reason, tc.wantReason) {
			t.Errorf("%s: got %+v, want phase %d with reason containing %q", tc.name, got, tc.want, tc.wantReason)
		}
	}
	if ip := StateOf(podWith(ready)).IP; ip != "10.0.0.7" {
		t.Errorf("ready pod IP = %q", ip)
	}
}
