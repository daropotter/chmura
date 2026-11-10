package k3s

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

// RetirementResult distinguishes confirmed absence from an accepted/pending
// deletion. A replacement must not start until Absent is true.
type RetirementResult struct {
	Absent bool
}

// Retirer gracefully retires an explicitly selected generation. The owner must
// stop/join its monitors and reporters and authorize retirement separately from
// readiness loss or an observation gap. It never creates a replacement.
type Retirer struct {
	Client coreclient.CoreV1Interface
}

// Ensure binds retirement to the original creation request/key and exact
// Pod/container identity. A name is never sufficient. Deletion uses both UID
// and resource-version preconditions and preserves the configured grace period.
// Replays verify a pending deletion or confirm absence; conflicts are returned
// without retrying against a fresh snapshot or adopting another generation.
func (r Retirer) Ensure(ctx context.Context, request InstanceRequest, ref Instance) (RetirementResult, error) {
	pod, err := r.snapshot(ctx, request, ref)
	if err != nil {
		return RetirementResult{}, err
	}
	if pod == nil {
		return RetirementResult{Absent: true}, nil
	}
	if pod.DeletionTimestamp != nil {
		return RetirementResult{}, nil
	}
	uid, version := pod.UID, pod.ResourceVersion
	err = r.Client.Pods(ref.Namespace).Delete(ctx, ref.PodName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}})
	if cause := ctx.Err(); cause != nil {
		return RetirementResult{}, cause
	}
	if apierrors.IsNotFound(err) {
		return RetirementResult{Absent: true}, nil
	}
	if err != nil {
		return RetirementResult{}, fmt.Errorf("retire instance: %w", err)
	}
	return RetirementResult{}, nil
}

// snapshot performs only a read and generation/intent validation. Nil means
// confirmed absence; callers must not infer deletion from a conflict alone.
func (r Retirer) snapshot(ctx context.Context, request InstanceRequest, ref Instance) (*corev1.Pod, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.Client == nil {
		return nil, errors.New("instance retirement requires an explicit client")
	}
	desired, err := retirementPayload(request, ref)
	if err != nil {
		return nil, err
	}
	pod, err := r.Client.Pods(ref.Namespace).Get(ctx, ref.PodName, metav1.GetOptions{})
	if cause := ctx.Err(); cause != nil {
		return nil, cause
	}
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read retiring instance: %w", err)
	}
	if pod == nil {
		return nil, ErrInstanceUnavailable
	}
	if pod.Namespace != ref.Namespace || pod.Name != ref.PodName || string(pod.UID) != ref.PodUID {
		return nil, ErrStaleInstance
	}
	if pod.ResourceVersion == "" || !sameInstance(desired, pod) {
		return nil, ErrInstanceConflict
	}
	found := false
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name != ref.ContainerName {
			continue
		}
		if container.ContainerID != "" && container.ContainerID != ref.ContainerID {
			return nil, ErrStaleInstance
		}
		found = container.ContainerID == ref.ContainerID
		break
	}
	if !found {
		return nil, ErrInstanceUnavailable
	}
	return pod, nil
}

func retirementPayload(request InstanceRequest, ref Instance) (*corev1.Pod, error) {
	desired, err := PrepareInstance(request)
	if err != nil {
		return nil, err
	}
	if ref.Namespace != desired.Namespace || ref.PodName != desired.Name || ref.PodUID == "" || ref.ContainerName != desired.Spec.Containers[0].Name || ref.ContainerID == "" {
		return nil, errors.New("instance retirement requires the creation request and a matching complete generation")
	}
	return desired, nil
}
