package k8s

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// PodPhase is what a sandbox pod's state means for the sandbox.
type PodPhase int

const (
	PodStarting    PodPhase = iota // not ready yet (or restarting); no status change
	PodReady                       // sshd accepts connections
	PodExpired                     // killed by activeDeadlineSeconds (D27)
	PodFailed                      // will never become ready
	PodTerminating                 // being deleted; whoever deleted it records why
)

// PodState summarizes a pod for the status syncer.
type PodState struct {
	Phase  PodPhase
	IP     string
	Reason string // for PodFailed: human-readable cause
}

// Container wait reasons that won't fix themselves, so the sandbox is failed
// rather than left pending (or "running" but unreachable) forever.
//
// Sandbox pods restart crashed containers (the workspace survives a restart,
// D8), so one crash is fine; CrashLoopBackOff means it keeps crashing.
var fatalWaitReasons = map[string]bool{
	"CrashLoopBackOff":           true,
	"ErrImagePull":               true,
	"ImagePullBackOff":           true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
}

// StateOf interprets a sandbox pod.
func StateOf(pod *corev1.Pod) PodState {
	if pod.DeletionTimestamp != nil {
		return PodState{Phase: PodTerminating}
	}
	switch pod.Status.Phase {
	case corev1.PodFailed:
		if pod.Status.Reason == "DeadlineExceeded" {
			return PodState{Phase: PodExpired}
		}
		return PodState{Phase: PodFailed, Reason: failureReason(pod)}
	case corev1.PodSucceeded:
		return PodState{Phase: PodFailed, Reason: "sandbox process exited"}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && fatalWaitReasons[w.Reason] {
			reason := strings.TrimSpace(w.Reason + ": " + w.Message)
			if t := cs.LastTerminationState.Terminated; t != nil && w.Reason == "CrashLoopBackOff" {
				reason += fmt.Sprintf(" (last exit code %d, %d restarts)", t.ExitCode, cs.RestartCount)
			}
			return PodState{Phase: PodFailed, Reason: reason}
		}
	}
	if pod.Status.Phase == corev1.PodRunning && isReady(pod) {
		return PodState{Phase: PodReady, IP: pod.Status.PodIP}
	}
	return PodState{Phase: PodStarting}
}

func isReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func failureReason(pod *corev1.Pod) string {
	if pod.Status.Reason != "" || pod.Status.Message != "" {
		return strings.TrimSpace(pod.Status.Reason + ": " + pod.Status.Message)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			return fmt.Sprintf("container exited with code %d (%s)", t.ExitCode, t.Reason)
		}
	}
	return "pod failed"
}

// SandboxID returns the sandbox a pod belongs to, from its label.
func SandboxID(pod *corev1.Pod) string {
	return pod.Labels[LabelSandboxID]
}

// PodHandler receives sandbox pod changes from the Watcher.
type PodHandler interface {
	PodChanged(pod *corev1.Pod)
	PodGone(sandboxID string)
}

// Watcher keeps a local, continuously updated cache of sandbox pods (a
// client-go informer). Kubernetes pushes changes over a single watch
// connection, so nothing polls the API server per sandbox.
type Watcher struct {
	factory informers.SharedInformerFactory
	lister  listersv1.PodNamespaceLister
	synced  cache.InformerSynced
}

// NewWatcher watches pods in namespace that carry our managed-by label.
func NewWatcher(client kubernetes.Interface, namespace string, h PodHandler) (*Watcher, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = LabelManagedBy + "=" + managedByValue
		}))
	inf := factory.Core().V1().Pods()

	_, err := inf.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { h.PodChanged(obj.(*corev1.Pod)) },
		UpdateFunc: func(_, obj any) { h.PodChanged(obj.(*corev1.Pod)) },
		DeleteFunc: func(obj any) {
			// If the watch dropped, the final state arrives wrapped.
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			if pod, ok := obj.(*corev1.Pod); ok {
				h.PodGone(SandboxID(pod))
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &Watcher{
		factory: factory,
		lister:  inf.Lister().Pods(namespace),
		synced:  inf.Informer().HasSynced,
	}, nil
}

// Start begins watching and blocks until the cache holds every current pod
// (or ctx ends).
func (w *Watcher) Start(ctx context.Context) error {
	w.factory.Start(ctx.Done())
	syncCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), w.synced) {
		return fmt.Errorf("sandbox pod cache did not sync")
	}
	return nil
}

// Pods returns every sandbox pod from the local cache (no API call).
func (w *Watcher) Pods() ([]*corev1.Pod, error) {
	return w.lister.List(labels.Everything())
}
