package k3s

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

// ErrReadinessConflict means a result's base snapshot is obsolete, or its key
// already identifies a different update. Reconcile before issuing a new result.
var ErrReadinessConflict = errors.New("readiness update conflicts with current instance state")

// ReadinessUpdate binds one readiness decision to a generation and a snapshot.
// Retries must retain all fields, including the original base ResourceVersion.
// A new decision requires a new key and a current observed resource version.
type ReadinessUpdate struct {
	Instance        Instance
	ResourceVersion string
	IdempotencyKey  string
	Ready           bool
}

// ReadinessWriter updates only the Chmura gate on an explicitly selected Pod.
// The owner serializes decisions; probe callbacks must not perform this I/O.
type ReadinessWriter struct {
	Client coreclient.CoreV1Interface
}

const readinessReason = "chmura-readiness"

// Apply verifies ownership and the running generation, then uses the base
// resource version to fence the status update. It returns the accepted/current
// version. A receipt in the gate condition permits identical retries after a
// lost response or unrelated kubelet status updates. Only the latest receipt
// is retained; an older result cannot overwrite a newer decision. Conflicts
// are read once to recognize concurrent identical updates, never blindly retried.
func (w ReadinessWriter) Apply(ctx context.Context, update ReadinessUpdate) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(update.IdempotencyKey) == "" || update.ResourceVersion == "" {
		return "", errors.New("readiness update requires a request key and base resource version")
	}
	resolver := Resolver{Client: w.Client}
	pod, err := resolver.readPod(ctx, update.Instance)
	if err != nil {
		return "", err
	}
	// All fields have fixed JSON representations and cannot fail marshaling.
	intent, _ := json.Marshal(struct {
		Version  int
		Instance Instance
		Base     string
		Ready    bool
	}{1, update.Instance, update.ResourceVersion, update.Ready})
	keyPrefix := fmt.Sprintf("key-sha256=%x;intent-sha256=", sha256.Sum256([]byte(update.IdempotencyKey)))
	receipt := fmt.Sprintf("%s%x", keyPrefix, sha256.Sum256(intent))
	status := corev1.ConditionFalse
	if update.Ready {
		status = corev1.ConditionTrue
	}
	index, replay, err := readinessState(pod, update.Instance, keyPrefix, receipt, status)
	if err != nil {
		return "", err
	}
	if replay {
		return pod.ResourceVersion, nil
	}
	if pod.ResourceVersion != update.ResourceVersion {
		return "", ErrReadinessConflict
	}
	condition := corev1.PodCondition{Type: ReadinessGate, Status: status, Reason: readinessReason, Message: receipt, LastTransitionTime: metav1.Now()}
	if index < 0 {
		pod.Status.Conditions = append(pod.Status.Conditions, condition)
	} else {
		previous := pod.Status.Conditions[index]
		if previous.Status == status && !previous.LastTransitionTime.IsZero() {
			condition.LastTransitionTime = previous.LastTransitionTime
		}
		pod.Status.Conditions[index] = condition
	}
	accepted, err := w.Client.Pods(update.Instance.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{FieldManager: "chmura"})
	if cause := ctx.Err(); cause != nil {
		return "", cause
	}
	if apierrors.IsConflict(err) {
		// A concurrent call may have applied this exact result. Reading its
		// receipt is safe; retrying the old decision against a fresh version is not.
		accepted, readErr := resolver.readPod(ctx, update.Instance)
		if readErr != nil {
			return "", readErr
		}
		_, replay, checkErr := readinessState(accepted, update.Instance, keyPrefix, receipt, status)
		if checkErr != nil {
			return "", checkErr
		}
		if replay {
			return accepted.ResourceVersion, nil
		}
		return "", fmt.Errorf("%w: %w", ErrReadinessConflict, err)
	}
	if err != nil {
		return "", fmt.Errorf("update instance readiness: %w", err)
	}
	if _, replay, err := readinessState(accepted, update.Instance, keyPrefix, receipt, status); err != nil {
		return "", err
	} else if !replay {
		return "", ErrReadinessConflict
	}
	return accepted.ResourceVersion, nil
}

func readinessState(pod *corev1.Pod, ref Instance, keyPrefix, receipt string, status corev1.ConditionStatus) (int, bool, error) {
	if _, err := targetFromPod(pod, ref, nil); err != nil {
		return -1, false, err
	}
	if pod.ResourceVersion == "" || len(pod.OwnerReferences) != 0 || pod.Labels[managedLabel] != "chmura" || pod.Labels[instanceLabel] != pod.Name {
		return -1, false, ErrInstanceConflict
	}
	gate := false
	for _, declared := range pod.Spec.ReadinessGates {
		gate = gate || declared.ConditionType == ReadinessGate
	}
	if !gate {
		return -1, false, ErrInstanceConflict
	}
	index := -1
	for i, condition := range pod.Status.Conditions {
		if condition.Type != ReadinessGate {
			continue
		}
		if index >= 0 {
			return -1, false, ErrReadinessConflict
		}
		index = i
	}
	if index >= 0 && strings.HasPrefix(pod.Status.Conditions[index].Message, keyPrefix) {
		condition := pod.Status.Conditions[index]
		if condition.Message != receipt || condition.Status != status || condition.Reason != readinessReason || condition.LastTransitionTime.IsZero() {
			return index, false, ErrReadinessConflict
		}
		return index, true, nil
	}
	return index, false, nil
}
