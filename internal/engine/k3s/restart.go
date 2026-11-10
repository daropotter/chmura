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

// RestartRequest binds an authorized replacement to the old generation and an
// explicit new creation key/allocation. The portable declaration and resolved
// artifact stay unchanged; the owner may supply a newly admitted node/allocation.
type RestartRequest struct {
	Previous InstanceRequest
	Instance Instance
	Next     InstanceRequest
}

// RestartResult either awaits old resource absence or acknowledges the new Pod.
// Acknowledging a Pod is not evidence of successful image pulling or readiness.
type RestartResult struct {
	Retiring bool
	Instance CreateResult
}

// Restarter reconciles one explicitly authorized operation per generation/slot.
// The owner serializes operations, retires obsolete requests durably, and stops
// and drains the old monitors/reporters before invoking it. This adapter makes
// no automatic restart-policy decision from readiness or observation errors.
type Restarter struct {
	Client coreclient.CoreV1Interface
}

// Ensure performs one reconciliation step. It validates the successor and any
// existing collision before retiring the old generation. Creation starts only
// after confirmed absence. The caller repeats the exact request while Retiring;
// it owns deadlines/backoff and must not submit obsolete restart operations.
func (r Restarter) Ensure(ctx context.Context, request RestartRequest) (RestartResult, error) {
	if err := ctx.Err(); err != nil {
		return RestartResult{}, err
	}
	if r.Client == nil {
		return RestartResult{}, errors.New("instance restart requires an explicit client")
	}
	desired, err := restartPayload(request)
	if err != nil {
		return RestartResult{}, err
	}
	stored, err := r.Client.Pods(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if cause := ctx.Err(); cause != nil {
		return RestartResult{}, cause
	}
	if err == nil {
		if _, err := creationResult(desired, stored, false); err != nil {
			return RestartResult{}, err
		}
	} else if !apierrors.IsNotFound(err) {
		return RestartResult{}, fmt.Errorf("inspect restart successor: %w", err)
	}
	retired, err := (Retirer{Client: r.Client}).Ensure(ctx, request.Previous, request.Instance)
	if err != nil {
		return RestartResult{}, err
	}
	if !retired.Absent {
		return RestartResult{Retiring: true}, nil
	}
	created, err := (Creator{Client: r.Client}).Ensure(ctx, request.Next)
	if err != nil {
		return RestartResult{}, err
	}
	return RestartResult{Instance: created}, nil
}

func restartPayload(request RestartRequest) (*corev1.Pod, error) {
	previous, err := retirementPayload(request.Previous, request.Instance)
	if err != nil {
		return nil, err
	}
	next, err := PrepareInstance(request.Next)
	if err != nil {
		return nil, err
	}
	if next.Namespace != previous.Namespace || next.Name == previous.Name {
		return nil, ErrInstanceConflict
	}
	// Compare canonical intent under the old key/allocation. Admission may
	// change explicitly, while a restart cannot silently become a rollout.
	comparison := request.Next
	comparison.IdempotencyKey = request.Previous.IdempotencyKey
	comparison.NodeName = request.Previous.NodeName
	comparison.CPUMilli, comparison.MemoryBytes = request.Previous.CPUMilli, request.Previous.MemoryBytes
	canonical, err := PrepareInstance(comparison)
	if err != nil {
		return nil, err
	}
	if canonical.Annotations[intentAnnotation] != previous.Annotations[intentAnnotation] {
		return nil, ErrInstanceConflict
	}
	return next, nil
}
