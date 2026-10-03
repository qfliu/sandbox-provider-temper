package k8s

import (
	"context"
	corev1 "k8s.io/api/core/v1"
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
	if c.Resources.Limits.StorageEphemeral().IsZero() {
		t.Fatal("pod must have an ephemeral-storage limit")
	}
	// An explicit, smaller request: with only a limit the scheduler
	// reserves the whole limit, and disk capped the node (load test, D48).
	if req, lim := c.Resources.Requests.StorageEphemeral(), c.Resources.Limits.StorageEphemeral(); req.IsZero() || req.Cmp(*lim) >= 0 {
		t.Fatalf("ephemeral-storage request = %v, limit = %v; want an explicit request below the limit", req, lim)
	}
	sc := c.SecurityContext
	if sc == nil || sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatal("pod must drop all capabilities")
	}
	for _, capability := range sc.Capabilities.Add {
		if capability == "NET_RAW" || capability == "SYS_ADMIN" || capability == "MKNOD" || capability == "NET_ADMIN" {
			t.Fatalf("pod adds dangerous capability %s", capability)
		}
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("pod must use the RuntimeDefault seccomp profile")
	}
	if sc.Privileged == nil || *sc.Privileged {
		t.Fatal("pod must not be privileged")
	}
	var hostKeyVol bool
	for _, v := range spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "sb-abc-hostkey" && *v.Secret.DefaultMode == 0o400 {
			hostKeyVol = true
		}
	}
	if !hostKeyVol {
		t.Fatal("pod must mount its own host key secret, readable only by its owner")
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

	if err := pods.Create(ctx, testSandbox, []byte("PRIVATE KEY")); err != nil {
		t.Fatal(err)
	}
	pod, err := client.CoreV1().Pods("sandboxes").Get(ctx, "sb-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	secret, err := client.CoreV1().Secrets("sandboxes").Get(ctx, "sb-abc-hostkey", metav1.GetOptions{})
	if err != nil || string(secret.Data[HostKeyFile]) != "PRIVATE KEY" {
		t.Fatalf("host key secret = %v, %v", secret, err)
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Name != pod.Name {
		t.Fatalf("secret should be owned by the pod: %+v", secret.OwnerReferences)
	}
	if err := pods.Delete(ctx, "sb-abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Secrets("sandboxes").Get(ctx, "sb-abc-hostkey", metav1.GetOptions{}); err == nil {
		t.Fatal("secret survived delete")
	}
	if err := pods.Delete(ctx, "sb-abc"); err != nil {
		t.Fatalf("deleting an already-deleted pod should succeed: %v", err)
	}
}

func TestCleanupSecrets(t *testing.T) {
	client := fake.NewClientset()
	pods := NewPods(client, testCfg)
	ctx := context.Background()
	for _, id := range []string{"sb-live", "sb-dead"} {
		client.CoreV1().Secrets("sandboxes").Create(ctx,
			BuildHostKeySecret(store.Sandbox{ID: id}, testCfg, nil), metav1.CreateOptions{})
	}

	deleted, err := pods.CleanupSecrets(ctx, func(id string) bool { return id == "sb-live" })
	if err != nil || len(deleted) != 1 || deleted[0] != "sb-dead-hostkey" {
		t.Fatalf("deleted %v, %v; want only sb-dead-hostkey", deleted, err)
	}
}
