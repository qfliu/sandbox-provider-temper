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

// Labels on every sandbox pod, so they can be found (and later targeted by
// NetworkPolicies) without the database.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelSandboxID = "sandbox.dev/id"
	LabelOwner     = "sandbox.dev/owner"
	managedByValue = "sandbox-api"
)

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

// Pods creates and deletes sandbox pods in one namespace.
type Pods struct {
	client kubernetes.Interface
	cfg    config.Sandbox
}

// NewPods returns a Pods manager for cfg.Namespace.
func NewPods(client kubernetes.Interface, cfg config.Sandbox) *Pods {
	return &Pods{client: client, cfg: cfg}
}

// Create starts the pod for a sandbox.
func (p *Pods) Create(ctx context.Context, sb store.Sandbox) error {
	_, err := p.client.CoreV1().Pods(p.cfg.Namespace).Create(ctx, BuildPod(sb, p.cfg), metav1.CreateOptions{})
	return err
}

// Delete removes a sandbox pod. A pod that is already gone is not an error.
func (p *Pods) Delete(ctx context.Context, podName string) error {
	grace := int64(5)
	err := p.client.CoreV1().Pods(p.cfg.Namespace).Delete(ctx, podName,
		metav1.DeleteOptions{GracePeriodSeconds: &grace})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// BuildPod returns the pod spec for a sandbox. It mirrors the hand-tested
// code/deploy/dev/sandbox-pod.yaml from Phase 1.
func BuildPod(sb store.Sandbox, cfg config.Sandbox) *corev1.Pod {
	no := false
	grace := int64(5)
	keyMode := int32(0o444)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sb.PodName,
			Namespace: cfg.Namespace,
			Labels: map[string]string{
				LabelManagedBy: managedByValue,
				LabelComponent: "sandbox",
				LabelSandboxID: sb.ID,
				LabelOwner:     sb.UserID,
			},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName:              ptr("gvisor"), // D4: gVisor isolation
			AutomountServiceAccountToken:  &no,           // sandbox code gets no cluster credentials
			EnableServiceLinks:            &no,           // don't leak cluster service env vars
			TerminationGracePeriodSeconds: &grace,
			Containers: []corev1.Container{{
				Name:            "sandbox",
				Image:           cfg.ImageRepo + sb.Image + ":" + cfg.ImageTag,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Ports:           []corev1.ContainerPort{{Name: "ssh", ContainerPort: 22}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("250m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("1"),
						corev1.ResourceMemory:           resource.MustParse("1Gi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("5Gi"),
					},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(22)}},
					PeriodSeconds: 2,
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "home", MountPath: "/home/user"},
					{Name: "internal-key", MountPath: "/etc/sandbox", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{ // D8: survives container restarts, not pod loss
					Name: "home",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
						SizeLimit: ptr(resource.MustParse("5Gi")),
					}},
				},
				{ // D7: the only key sandbox sshd trusts
					Name: "internal-key",
					VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName:  cfg.InternalKeySecret,
						DefaultMode: &keyMode,
					}},
				},
			},
		},
	}
}

func ptr[T any](v T) *T { return &v }
