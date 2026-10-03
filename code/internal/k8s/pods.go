// Package k8s creates and deletes sandbox pods. It's the only package that
// talks to the Kubernetes API.
package k8s

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/qfliu/sandbox-provider-temper/code/internal/config"
	"github.com/qfliu/sandbox-provider-temper/code/internal/store"
)

// Labels on every sandbox pod and host-key Secret, so they can be found
// (and later targeted by NetworkPolicies) without the database.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelSandboxID = "sandbox.dev/id"
	LabelOwner     = "sandbox.dev/owner"
	managedByValue = "sandbox-api"
)

// HostKeyFile is the key inside a sandbox's host-key Secret.
const HostKeyFile = "ssh_host_ed25519_key"

// NewClientset connects to Kubernetes: with the pod's service account when
// running in the cluster, otherwise with KUBECONFIG (like kubectl).
func NewClientset() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		if kc := os.Getenv("KUBECONFIG"); kc != "" {
			rules.ExplicitPath = kc
		}
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("load kubernetes config: %w", err)
		}
	}
	return kubernetes.NewForConfig(cfg)
}

// Pods creates and deletes sandbox pods (and their host-key Secrets) in one
// namespace.
type Pods struct {
	client kubernetes.Interface
	cfg    config.Sandbox
}

// NewPods returns a Pods manager for cfg.Namespace.
func NewPods(client kubernetes.Interface, cfg config.Sandbox) *Pods {
	return &Pods{client: client, cfg: cfg}
}

// HostKeySecretName is the Secret holding a sandbox's private host key.
func HostKeySecretName(sandboxID string) string { return sandboxID + "-hostkey" }

// Create starts a sandbox: its host-key Secret first (the pod mounts it),
// then the pod, then makes the pod the Secret's owner so Kubernetes deletes
// the Secret together with the pod (D29).
func (p *Pods) Create(ctx context.Context, sb store.Sandbox, hostKeyPEM []byte) error {
	secrets := p.client.CoreV1().Secrets(p.cfg.Namespace)
	secret, err := secrets.Create(ctx, BuildHostKeySecret(sb, p.cfg, hostKeyPEM), metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create host key secret: %w", err)
	}
	pod, err := p.client.CoreV1().Pods(p.cfg.Namespace).Create(ctx, BuildPod(sb, p.cfg), metav1.CreateOptions{})
	if err != nil {
		secrets.Delete(context.WithoutCancel(ctx), secret.Name, metav1.DeleteOptions{})
		return err
	}
	secret.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID,
	}}
	// Not fatal if this fails: the reconcile's CleanupSecrets removes the
	// Secret once the sandbox ends.
	secrets.Update(ctx, secret, metav1.UpdateOptions{})
	return nil
}

// Delete removes a sandbox pod and its host-key Secret. Already-gone objects
// are not an error.
func (p *Pods) Delete(ctx context.Context, podName string) error {
	grace := int64(5)
	err := p.client.CoreV1().Pods(p.cfg.Namespace).Delete(ctx, podName,
		metav1.DeleteOptions{GracePeriodSeconds: &grace})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	err = p.client.CoreV1().Secrets(p.cfg.Namespace).Delete(ctx, HostKeySecretName(podName), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// CleanupSecrets deletes host-key Secrets whose sandbox isn't live, catching
// any the owner reference missed. Returns the names deleted.
func (p *Pods) CleanupSecrets(ctx context.Context, isLive func(sandboxID string) bool) ([]string, error) {
	list, err := p.client.CoreV1().Secrets(p.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + managedByValue,
	})
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, s := range list.Items {
		if isLive(s.Labels[LabelSandboxID]) {
			continue
		}
		err := p.client.CoreV1().Secrets(p.cfg.Namespace).Delete(ctx, s.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return deleted, err
		}
		deleted = append(deleted, s.Name)
	}
	return deleted, nil
}

func labelsFor(sb store.Sandbox, component string) map[string]string {
	return map[string]string{
		LabelManagedBy: managedByValue,
		LabelComponent: component,
		LabelSandboxID: sb.ID,
		LabelOwner:     sb.UserID,
	}
}

// BuildHostKeySecret returns the Secret carrying a sandbox's private host key.
func BuildHostKeySecret(sb store.Sandbox, cfg config.Sandbox, hostKeyPEM []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      HostKeySecretName(sb.ID),
			Namespace: cfg.Namespace,
			Labels:    labelsFor(sb, "sandbox-hostkey"),
		},
		Data: map[string][]byte{HostKeyFile: hostKeyPEM},
	}
}

// DeadlineGrace is how long past its TTL a sandbox pod may run before the
// kubelet kills it. The reaper (step 2.8) normally deletes the sandbox at its
// TTL and records why; this deadline is the safety net for when the API or
// reaper is down (D27).
const DeadlineGrace = 60

// BuildPod returns the pod spec for a sandbox. It mirrors the hand-tested
// code/deploy/dev/sandbox-pod.yaml from Phase 1.
func BuildPod(sb store.Sandbox, cfg config.Sandbox) *corev1.Pod {
	no := false
	grace := int64(5)
	keyMode := int32(0o444)
	hostKeyMode := int32(0o400) // sshd refuses host keys others can read
	deadline := int64(sb.TTLSeconds + DeadlineGrace)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sb.PodName,
			Namespace: cfg.Namespace,
			Labels:    labelsFor(sb, "sandbox"),
		},
		Spec: corev1.PodSpec{
			RuntimeClassName:              ptr("gvisor"), // D4: gVisor isolation
			AutomountServiceAccountToken:  &no,           // sandbox code gets no cluster credentials
			EnableServiceLinks:            &no,           // don't leak cluster service env vars
			TerminationGracePeriodSeconds: &grace,
			ActiveDeadlineSeconds:         &deadline, // D27: kubelet kills it even if the API is down
			Containers: []corev1.Container{{
				Name:            "sandbox",
				Image:           cfg.ImageRepo + sb.Image + ":" + cfg.ImageTag,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Ports:           []corev1.ContainerPort{{Name: "ssh", ContainerPort: 22}},
				Resources: corev1.ResourceRequirements{
					// Requests are what the scheduler reserves. Ephemeral
					// storage needs an explicit request: with only a limit,
					// Kubernetes reserves the whole 5 Gi per sandbox, and disk,
					// not CPU, capped a 37 GB node at ~6 sandboxes (load test,
					// D48). Each can still grow to its 5 Gi limit.
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("250m"),
						corev1.ResourceMemory:           resource.MustParse("256Mi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("1"),
						corev1.ResourceMemory:           resource.MustParse("1Gi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("5Gi"),
					},
				},
				SecurityContext: SandboxSecurityContext(),
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(22)}},
					PeriodSeconds: 1, // "running" as soon as sshd answers (load test, D48)
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "home", MountPath: "/home/user"},
					{Name: "internal-key", MountPath: "/etc/sandbox", ReadOnly: true},
					{Name: "host-key", MountPath: "/etc/sandbox-hostkey", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{ // D8: survives container restarts, not pod loss
					Name: "home",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
						SizeLimit: ptr(resource.MustParse("5Gi")),
					}},
				},
				{ // D7: the only client key sandbox sshd trusts
					Name: "internal-key",
					VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName:  cfg.InternalKeySecret,
						DefaultMode: &keyMode,
					}},
				},
				{ // D29: this sandbox's own host key, so the API can verify it
					Name: "host-key",
					VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName:  HostKeySecretName(sb.ID),
						DefaultMode: &hostKeyMode,
					}},
				},
			},
		},
	}
}

// SandboxCapabilities are the only Linux capabilities a sandbox keeps (step
// 5.2): what sshd needs (switch users, chroot for privilege separation, bind
// port 22) and what the sandbox's root user needs for everyday admin work
// (sudo, apt, chown/chmod of others' files). Everything else the container
// runtime would grant by default is dropped, notably NET_RAW (raw packets:
// spoofing, ping), MKNOD, SETPCAP and SETFCAP. The admission policy
// (deploy/dev/sandbox-pod-policy.yaml) enforces the same list.
var SandboxCapabilities = []corev1.Capability{
	"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETUID", "SETGID",
	"KILL", "NET_BIND_SERVICE", "SYS_CHROOT", "AUDIT_WRITE",
}

// SandboxSecurityContext is the sandbox container's security context: drop
// all capabilities but SandboxCapabilities, and the runtime's default
// seccomp filter on top of gVisor. Privilege escalation stays allowed
// because sudo (setuid) is part of the sandbox (D2): the user is meant to
// be root inside their own gVisor sandbox.
func SandboxSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  SandboxCapabilities,
		},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		Privileged:     ptr(false),
	}
}

func ptr[T any](v T) *T { return &v }
