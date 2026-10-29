package k3s

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

// ErrInstanceConflict means a request key/name is already bound to another
// intent, owner or execution shape. The existing object is never modified.
var ErrInstanceConflict = errors.New("instance request conflicts with an existing resource")

// PodIdentity identifies the created Pod before a container generation starts.
// A supervisor must discover ContainerID before using Resolver/Observe.
type PodIdentity struct {
	Namespace     string
	PodName       string
	PodUID        string
	ContainerName string
}

// CreateResult reports acknowledged creation or reuse of the same stored Pod.
// It does not report application readiness or successful image pulling.
type CreateResult struct {
	Identity PodIdentity
	Created  bool
}

type Creator struct {
	Client coreclient.CoreV1Interface
}

// Ensure creates at most one current Pod for a namespace-scoped request key.
// Concurrent retries use Kubernetes name uniqueness, then verify ownership,
// intent and execution shape. It never patches, deletes or adopts conflicts.
// Cancellation/transport errors may leave an accepted Pod; retry the same key.
// This is desired-resource idempotence, not a permanent operation receipt: a
// deleted Pod can be recreated. The control plane must retire old operations.
func (c Creator) Ensure(ctx context.Context, request InstanceRequest) (CreateResult, error) {
	if err := ctx.Err(); err != nil {
		return CreateResult{}, err
	}
	if c.Client == nil {
		return CreateResult{}, errors.New("instance creation requires an explicit client")
	}
	desired, err := PrepareInstance(request)
	if err != nil {
		return CreateResult{}, err
	}
	pod, err := c.Client.Pods(request.Namespace).Create(ctx, desired, metav1.CreateOptions{FieldManager: "chmura"})
	if cause := ctx.Err(); cause != nil {
		return CreateResult{}, cause
	}
	if err == nil {
		return creationResult(desired, pod, true)
	}
	if !apierrors.IsAlreadyExists(err) {
		return CreateResult{}, fmt.Errorf("create instance: %w", err)
	}
	pod, err = c.Client.Pods(request.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if cause := ctx.Err(); cause != nil {
		return CreateResult{}, cause
	}
	if err != nil {
		return CreateResult{}, fmt.Errorf("read existing instance: %w", err)
	}
	return creationResult(desired, pod, false)
}

func creationResult(desired, pod *corev1.Pod, created bool) (CreateResult, error) {
	if pod == nil || pod.UID == "" || pod.Namespace != desired.Namespace || pod.Name != desired.Name {
		return CreateResult{}, errors.New("instance API returned an incomplete or different identity")
	}
	if !sameInstance(desired, pod) {
		return CreateResult{}, ErrInstanceConflict
	}
	return CreateResult{Identity: PodIdentity{Namespace: pod.Namespace, PodName: pod.Name, PodUID: string(pod.UID), ContainerName: "app"}, Created: created}, nil
}

func sameInstance(desired, pod *corev1.Pod) bool {
	if len(pod.OwnerReferences) != 0 {
		return false
	}
	for _, key := range []string{managedLabel, instanceLabel} {
		if pod.Labels[key] != desired.Labels[key] {
			return false
		}
	}
	for _, key := range []string{keyAnnotation, intentAnnotation} {
		if pod.Annotations[key] != desired.Annotations[key] {
			return false
		}
	}
	actual := pod.Spec.DeepCopy()
	// These fields come from the already admitted namespace/node context rather
	// than this executor. Everything else, including the full container spec,
	// must still match the requested execution shape.
	if actual.ServiceAccountName != "" && actual.ServiceAccountName != "default" || actual.DeprecatedServiceAccount != "" && actual.DeprecatedServiceAccount != "default" {
		return false
	}
	actual.ServiceAccountName, actual.DeprecatedServiceAccount = "", ""
	actual.Tolerations, actual.ImagePullSecrets = nil, nil
	if actual.SecurityContext != nil && apiequality.Semantic.DeepEqual(*actual.SecurityContext, corev1.PodSecurityContext{}) {
		actual.SecurityContext = nil
	}
	if actual.HostUsers != nil && *actual.HostUsers {
		actual.HostUsers = nil
	}
	if actual.Priority != nil && *actual.Priority == 0 {
		actual.Priority = nil
	}
	if actual.PreemptionPolicy != nil && *actual.PreemptionPolicy == corev1.PreemptLowerPriority {
		actual.PreemptionPolicy = nil
	}
	return apiequality.Semantic.DeepEqual(*actual, desired.Spec)
}
