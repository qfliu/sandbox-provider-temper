package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/qfliu/sandbox-provider-temper/code/internal/config"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

var testCfg = config.Sandbox{
	Namespace:         "sandboxes",
	ImageRepo:         "sandbox/",
	ImageTag:          "0.1",
	InternalKeySecret: "sandbox-internal-key",
}

var testSandbox = store.Sandbox{ID: "sb-abc", PodName: "sb-abc", UserID: "u1", Image: "ubuntu-dev", TTLSeconds: 600}

// The security-relevant parts of the pod spec must not regress silently.
func TestBuildPodIsolation(t *testing.T) {
	pod := BuildPod(testSandbox, testCfg)
	spec := pod.Spec

	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != "gvisor" {
		t.Fatal("pod must run under gVisor")
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatal("pod must not get a service account token")
	}
	if spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
		t.Fatal("pod must not get service-link env vars")
	}
	if spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds != 600+DeadlineGrace {
		t.Fatalf("activeDeadlineSeconds = %v, want TTL + %d", spec.ActiveDeadlineSeconds, DeadlineGrace)
	}
	c := spec.Containers[0]
	if c.Image != "sandbox/ubuntu-dev:0.1" {
		t.Fatalf("image = %q", c.Image)
	}
	if c.Resources.Limits.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() {
		t.Fatal("pod must have CPU and memory limits")
	}
	if pod.Name != "sb-abc" || pod.Namespace != "sandboxes" ||
		pod.Labels[LabelSandboxID] != "sb-abc" || pod.Labels[LabelOwner] != "u1" {
		t.Fatalf("unexpected metadata %+v", pod.ObjectMeta)
	}
}

func TestPodsCreateAndDelete(t *testing.T) {
	client := fake.NewClientset()
	pods := NewPods(client, testCfg)
	ctx := context.Background()

	if err := pods.Create(ctx, testSandbox); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods("sandboxes").Get(ctx, "sb-abc", metav1.GetOptions{}); err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	if err := pods.Delete(ctx, "sb-abc"); err != nil {
		t.Fatal(err)
	}
	if err := pods.Delete(ctx, "sb-abc"); err != nil {
		t.Fatalf("deleting an already-deleted pod should succeed: %v", err)
	}
}
