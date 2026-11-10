package k3s

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// ErrInstanceExited is a runtime-reported exit for an exact process generation.
// It remains an ErrInstanceUnavailable for existing callers; generic
// unavailability alone must never be interpreted as evidence of an exit.
var ErrInstanceExited = fmt.Errorf("%w: selected process exited", ErrInstanceUnavailable)

// InstanceExitError retains the reported facts and snapshot identity. These are
// private engine facts, not a restart decision or proof of physical fencing.
type InstanceExitError struct {
	Instance        Instance
	ResourceVersion string
	ExitCode        int32
	Reason          string
	StartedAt       time.Time
	FinishedAt      time.Time
}

func (e *InstanceExitError) Error() string {
	return fmt.Sprintf("runtime instance exited with code %d", e.ExitCode)
}

func (e *InstanceExitError) Unwrap() error { return ErrInstanceExited }

func reportedExit(pod *corev1.Pod, status corev1.ContainerStatus, ref Instance) error {
	terminated := status.State.Terminated
	if pod.ResourceVersion == "" || terminated == nil || status.State.Running != nil || status.State.Waiting != nil {
		return nil
	}
	if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded {
		return nil
	}
	if terminated.ContainerID != ref.ContainerID || terminated.StartedAt.IsZero() || terminated.FinishedAt.IsZero() || terminated.FinishedAt.Before(&terminated.StartedAt) {
		return nil
	}
	// Kubelet synthesizes this termination when the runtime status is unknown.
	if terminated.Reason == "ContainerStatusUnknown" {
		return nil
	}
	return &InstanceExitError{Instance: ref, ResourceVersion: pod.ResourceVersion,
		ExitCode: terminated.ExitCode, Reason: terminated.Reason,
		StartedAt: terminated.StartedAt.Time, FinishedAt: terminated.FinishedAt.Time}
}
