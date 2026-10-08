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
	"k8s.io/client-go/util/retry"

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
	// client-go's default client-side limit is 5 requests/s (burst 10); a
	// create is two writes (Secret, pod), so one API replica could start only
	// ~2.5 sandboxes/s once the burst was spent (D67). The API server's own
	// priority and fairness limits still apply above this.
	cfg.QPS, cfg.Burst = ClientQPS, ClientBurst
	return kubernetes.NewForConfig(cfg)
}

// ClientQPS and ClientBurst are the API's client-side rate limit toward the
// Kubernetes API (D68).
const (
	ClientQPS   = 50
	ClientBurst = 100
)

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

// AuthorizedKeysFile is the key inside the internal-key Secret that sandbox
// pods mount: the platform's public key in authorized_keys format (D7).
const AuthorizedKeysFile = "authorized_keys"

// PublishInternalKey writes the public half of the API's internal SSH key
// into the Secret every sandbox mounts (cfg.InternalKeySecret), so sshd
// trusts exactly the key the API and gateway log in with. The deployment
// only provides the private key (a Helm chart can generate one but can't
// derive the OpenSSH public key); the API publishes the rest at startup.
// Idempotent: it updates the Secret only when it differs, so replicas
// starting together agree.
//
// The Secret deliberately lacks the managed-by label: CleanupSecrets
// deletes every such Secret that belongs to no live sandbox.
func (p *Pods) PublishInternalKey(ctx context.Context, authorizedKey []byte) error {
	secrets := p.client.CoreV1().Secrets(p.cfg.Namespace)
	want := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.cfg.InternalKeySecret,
			Namespace: p.cfg.Namespace,
			Labels:    map[string]string{LabelComponent: "internal-key"},
		},
		Data: map[string][]byte{AuthorizedKeysFile: authorizedKey},
	}
	_, err := secrets.Create(ctx, want, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	// Retried on a conflict: another replica updated it between our Get
	// and Update (only possible while the key itself is being replaced).
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		have, err := secrets.Get(ctx, want.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if string(have.Data[AuthorizedKeysFile]) == string(authorizedKey) {
			return nil
		}
		have.Data = want.Data
		_, err = secrets.Update(ctx, have, metav1.UpdateOptions{})
		return err
	})
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

// DockerImage is the image whose sandboxes run Docker (D62). Only its pods
// get the gvisor-docker runtime and Docker's capabilities (D61), and more
// memory and CPU, because Docker's storage is a tmpfs inside the sandbox.
const DockerImage = "ubuntu-docker"

// podProfile is what differs between a plain sandbox and a Docker one.
type podProfile struct {
	runtimeClass  string
	docker        bool
	kata          bool
	cpuLimit      string
	memoryRequest string
	memoryLimit   string
	env           []corev1.EnvVar
}

// profileFor gives the pod settings for an image under a runtime (D65).
// Under Kata the RuntimeClass is always `kata` (each pod is a VM), and a
// Docker sandbox also gets SYS_RESOURCE (runc sets nested containers' OOM
// scores; gVisor ignores them, D64).
func profileFor(image, runtime string) podProfile {
	p := profileForImage(image)
	if runtime == "kata" {
		p.runtimeClass = "kata"
		p.kata = true
	}
	return p
}

func profileForImage(image string) podProfile {
	if image == DockerImage {
		return podProfile{
			runtimeClass: "gvisor-docker", docker: true,
			// Docker's images, layers and build cache live in this tmpfs,
			// inside the 4 Gi: half for Docker, half for everything else.
			// The request is a quarter of the limit (review of #73): a
			// node packs more Docker sandboxes than could all fill up, so
			// under memory pressure these Burstable pods are evicted first
			// and lose their Docker state. A request equal to the limit
			// would make that impossible at four times the cost per node.
			cpuLimit: "2", memoryRequest: "1Gi", memoryLimit: "4Gi",
			env: []corev1.EnvVar{{Name: "DOCKER_STORAGE_SIZE", Value: "2g"}},
		}
	}
	return podProfile{runtimeClass: "gvisor", cpuLimit: "1", memoryRequest: "256Mi", memoryLimit: "1Gi"}
}

// BuildPod returns the pod spec for a sandbox. It mirrors the hand-tested
// code/deploy/dev/sandbox-pod.yaml from Phase 1.
func BuildPod(sb store.Sandbox, cfg config.Sandbox) *corev1.Pod {
	prof := profileFor(sb.Image, sb.Runtime)
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
			RuntimeClassName:              ptr(prof.runtimeClass), // D4, D61, D65: gVisor or Kata
			AutomountServiceAccountToken:  &no,                    // sandbox code gets no cluster credentials
			EnableServiceLinks:            &no,                    // don't leak cluster service env vars
			TerminationGracePeriodSeconds: &grace,
			ActiveDeadlineSeconds:         &deadline, // D27: kubelet kills it even if the API is down
			Containers: []corev1.Container{{
				Name:            "sandbox",
				Image:           cfg.ImageRepo + sb.Image + ":" + cfg.ImageTag,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Ports:           []corev1.ContainerPort{{Name: "ssh", ContainerPort: 22}},
				Env:             prof.env,
				Resources: corev1.ResourceRequirements{
					// Requests are what the scheduler reserves. Ephemeral
					// storage needs an explicit request: with only a limit,
					// Kubernetes reserves the whole 5 Gi per sandbox, and disk,
					// not CPU, capped a 37 GB node at ~6 sandboxes (load test,
					// D48). Each can still grow to its 5 Gi limit.
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("250m"),
						corev1.ResourceMemory:           resource.MustParse(prof.memoryRequest),
						corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse(prof.cpuLimit),
						corev1.ResourceMemory:           resource.MustParse(prof.memoryLimit),
						corev1.ResourceEphemeralStorage: resource.MustParse("5Gi"),
					},
				},
				SecurityContext: SandboxSecurityContext(prof.docker, prof.kata),
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
// (deploy/helm/templates/sandbox-pod-policy.yaml) enforces the same list.
var SandboxCapabilities = []corev1.Capability{
	"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETUID", "SETGID",
	"KILL", "NET_BIND_SERVICE", "SYS_CHROOT", "AUDIT_WRITE",
}

// DockerCapabilities are added for Docker sandboxes only (D61, measured):
// the rest of Docker's default container set, which dockerd must hold to
// give nested containers (MKNOD, NET_RAW, SETFCAP, SETPCAP), and what
// dockerd needs for mounts and its bridge (SYS_ADMIN, NET_ADMIN). They are
// granted inside gVisor's kernel, under the gvisor-docker runtime; the
// admission policy allows them with that runtime only.
var DockerCapabilities = []corev1.Capability{
	"MKNOD", "NET_RAW", "SETFCAP", "SETPCAP", "SYS_ADMIN", "NET_ADMIN",
}

// KataDockerCapabilities are added for Docker sandboxes under Kata only
// (D64): runc lowers each nested container's OOM score below the pod's, which
// needs SYS_RESOURCE; inside a Kata VM it reaches only that VM.
var KataDockerCapabilities = []corev1.Capability{"SYS_RESOURCE"}

// SandboxSecurityContext is the sandbox container's security context: drop
// all capabilities but SandboxCapabilities (plus DockerCapabilities for a
// Docker sandbox), and the RuntimeDefault seccomp
// profile (not applied under gVisor, whose own syscall filter is the
// barrier; it applies if a pod ever runs on another runtime, D45).
// Privilege escalation stays allowed
// because sudo (setuid) is part of the sandbox (D2): the user is meant to
// be root inside their own gVisor sandbox.
func SandboxSecurityContext(docker, kata bool) *corev1.SecurityContext {
	add := append([]corev1.Capability{}, SandboxCapabilities...)
	if docker {
		add = append(add, DockerCapabilities...)
	}
	if docker && kata {
		add = append(add, KataDockerCapabilities...)
	}
	return &corev1.SecurityContext{
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  add,
		},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		Privileged:     ptr(false),
	}
}

func ptr[T any](v T) *T { return &v }
