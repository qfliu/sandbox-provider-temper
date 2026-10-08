package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

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

// A Docker sandbox differs only where it must (D61, D62): its runtime, its
// six extra capabilities, more memory and CPU. Everything else is the plain
// sandbox's, and plain sandboxes never get Docker's capabilities.
func TestBuildPodDocker(t *testing.T) {
	sb := testSandbox
	sb.Image = DockerImage
	pod := BuildPod(sb, testCfg)
	if rc := pod.Spec.RuntimeClassName; rc == nil || *rc != "gvisor-docker" {
		t.Fatalf("runtime = %v, want gvisor-docker", rc)
	}
	c := pod.Spec.Containers[0]
	if c.Image != "sandbox/ubuntu-docker:0.1" {
		t.Fatalf("image = %q", c.Image)
	}
	want := map[corev1.Capability]bool{}
	for _, c := range append(append([]corev1.Capability{}, SandboxCapabilities...), DockerCapabilities...) {
		want[c] = true
	}
	got := c.SecurityContext.Capabilities.Add
	if len(got) != len(want) {
		t.Fatalf("capabilities = %v, want %d", got, len(want))
	}
	for _, capability := range got {
		if !want[capability] {
			t.Fatalf("unexpected capability %s", capability)
		}
	}
	if d := c.SecurityContext.Capabilities.Drop; len(d) != 1 || d[0] != "ALL" {
		t.Fatal("docker pod must still drop all capabilities first")
	}
	if p := c.SecurityContext.Privileged; p == nil || *p {
		t.Fatal("docker pod must not be privileged")
	}
	if c.Resources.Limits.Memory().Cmp(resource.MustParse("4Gi")) != 0 {
		t.Fatalf("memory limit = %v, want 4Gi", c.Resources.Limits.Memory())
	}
	if len(c.Env) != 1 || c.Env[0].Name != "DOCKER_STORAGE_SIZE" {
		t.Fatalf("env = %v, want DOCKER_STORAGE_SIZE", c.Env)
	}
	if *pod.Spec.AutomountServiceAccountToken || *pod.Spec.EnableServiceLinks {
		t.Fatal("docker pod must keep the plain sandbox's isolation")
	}

	plain := BuildPod(testSandbox, testCfg).Spec.Containers[0]
	for _, capability := range plain.SecurityContext.Capabilities.Add {
		for _, d := range DockerCapabilities {
			if capability == d {
				t.Fatalf("plain sandbox got Docker capability %s", d)
			}
		}
	}
	if len(plain.Env) != 0 {
		t.Fatalf("plain sandbox env = %v", plain.Env)
	}
}

// Under Kata (D65) every sandbox's RuntimeClass is kata; a Docker sandbox
// keeps Docker's capabilities and gains KataDockerCapabilities (SYS_RESOURCE,
// SYS_PTRACE), a plain one keeps
// exactly the plain set.
func TestBuildPodKata(t *testing.T) {
	plain := testSandbox
	plain.Runtime = "kata"
	pod := BuildPod(plain, testCfg)
	if rc := pod.Spec.RuntimeClassName; rc == nil || *rc != "kata" {
		t.Fatalf("runtime = %v, want kata", rc)
	}
	if got := pod.Spec.Containers[0].SecurityContext.Capabilities.Add; len(got) != len(SandboxCapabilities) {
		t.Fatalf("plain kata sandbox capabilities = %v, want the plain set", got)
	}

	docker := testSandbox
	docker.Image, docker.Runtime = DockerImage, "kata"
	pod = BuildPod(docker, testCfg)
	if rc := pod.Spec.RuntimeClassName; rc == nil || *rc != "kata" {
		t.Fatalf("docker runtime = %v, want kata", rc)
	}
	got := pod.Spec.Containers[0].SecurityContext.Capabilities.Add
	if want := len(SandboxCapabilities) + len(DockerCapabilities) + len(KataDockerCapabilities); len(got) != want {
		t.Fatalf("docker kata capabilities = %v, want %d", got, want)
	}
	if got[len(got)-2] != "SYS_RESOURCE" || got[len(got)-1] != "SYS_PTRACE" {
		t.Fatalf("docker kata capabilities = %v, want SYS_RESOURCE and SYS_PTRACE last", got)
	}
	c := pod.Spec.Containers[0]
	if c.Resources.Limits.Memory().Cmp(resource.MustParse("4Gi")) != 0 || len(c.Env) != 1 || c.Env[0].Name != "DOCKER_STORAGE_SIZE" {
		t.Fatalf("docker kata sandbox lost Docker's resources or env: %v %v", c.Resources.Limits, c.Env)
	}

	// gVisor (and an unset runtime, as for rows from before D65) never gets
	// the Kata-only capabilities.
	gv := testSandbox
	gv.Image = DockerImage
	for _, c := range BuildPod(gv, testCfg).Spec.Containers[0].SecurityContext.Capabilities.Add {
		if c == "SYS_RESOURCE" || c == "SYS_PTRACE" {
			t.Fatalf("gvisor-docker sandbox got %s", c)
		}
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

// The API publishes its public key for sandbox sshd: created, left alone
// when equal, replaced when the key changed, and never removed by the
// leftover-Secret cleanup.
func TestPublishInternalKey(t *testing.T) {
	client := fake.NewClientset()
	pods := NewPods(client, testCfg)
	ctx := context.Background()
	get := func() string {
		s, err := client.CoreV1().Secrets("sandboxes").Get(ctx, testCfg.InternalKeySecret, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return string(s.Data[AuthorizedKeysFile])
	}
	for _, key := range []string{"ssh-ed25519 AAAA1\n", "ssh-ed25519 AAAA1\n", "ssh-ed25519 AAAA2\n"} {
		if err := pods.PublishInternalKey(ctx, []byte(key)); err != nil {
			t.Fatal(err)
		}
		if got := get(); got != key {
			t.Fatalf("secret holds %q, want %q", got, key)
		}
	}
	deleted, err := pods.CleanupSecrets(ctx, func(string) bool { return false })
	if err != nil || len(deleted) != 0 {
		t.Fatalf("cleanup deleted %v (%v); the internal key must survive", deleted, err)
	}
	get()
}
