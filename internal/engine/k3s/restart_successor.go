package k3s

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/daropotter/chmura/internal/state"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RestartSuccessorStore links a completed restart to retained process history.
// The owner authorizes intent and serializes binding with admissions and other
// runtime actions. These snapshots acquire no lease. sqlite.Store implements it.
type RestartSuccessorStore interface {
	Get(context.Context, string, string) (state.Operation, error)
	SupervisionHistory
	RecordProcess(context.Context, state.ProcessObservation) (state.Process, bool, error)
}

// BindRestartSuccessor recovers a current succeeded restart's exact process and
// returns a request for Resume/ResumeDurable only after binding acknowledgement.
// An existing live binding replays without runtime I/O; runtime existence and
// current readiness must still be observed by supervision. Unbound discovery
// reads one verified Pod, including a confirmed exit before first observation.
// The caller owns deadlines/backoff and serialization across this whole method.
// This method never creates, changes or replaces a runtime resource, starts
// monitoring, or grants restart/worker ownership.
func (r Resolver) BindRestartSuccessor(ctx context.Context, history RestartSuccessorStore, selection RestartSelection) (ResumeRequest, error) {
	if err := ctx.Err(); err != nil {
		return ResumeRequest{}, err
	}
	if history == nil || strings.TrimSpace(selection.Scope) == "" || strings.TrimSpace(selection.SlotID) == "" || strings.TrimSpace(selection.GenerationID) == "" || strings.TrimSpace(selection.OperationID) == "" {
		return ResumeRequest{}, errors.New("restart successor binding requires history, scope, slot, generation and operation")
	}
	op, err := history.Get(ctx, selection.Scope, selection.OperationID)
	if cause := ctx.Err(); cause != nil {
		return ResumeRequest{}, cause
	}
	if err != nil {
		return ResumeRequest{}, fmt.Errorf("read completed restart: %w", err)
	}
	if op.Status != state.OperationSucceeded {
		return ResumeRequest{}, fmt.Errorf("%w: successor binding requires acknowledged restart completion", state.ErrConflict)
	}
	intent, err := decodeRestartOperationIntent(op.Intent)
	if err != nil {
		return ResumeRequest{}, err
	}
	if err := checkRestartSuccessorHead(ctx, history, selection, op, intent); err != nil {
		return ResumeRequest{}, err
	}
	identity, err := decodeRestartResult(op.Result, op.Intent, intent)
	if err != nil {
		return ResumeRequest{}, err
	}
	process, err := history.GetProcess(ctx, selection.Scope, selection.SlotID, selection.GenerationID)
	if cause := ctx.Err(); cause != nil {
		return ResumeRequest{}, cause
	}
	if errors.Is(err, state.ErrNotFound) {
		ref, started, err := r.discoverRestartSuccessor(ctx, intent.Next.request(), identity)
		if err != nil {
			return ResumeRequest{}, err
		}
		encoded, err := EncodeProcessIdentity(ref)
		if err != nil {
			return ResumeRequest{}, err
		}
		// Stable false is replayable after a lost reply and never erases a
		// later retained first readiness. Runtime Ready is not Chmura history.
		process, _, err = history.RecordProcess(ctx, state.ProcessObservation{Scope: selection.Scope, SlotID: selection.SlotID,
			GenerationID: selection.GenerationID, IdempotencyKey: "restart-successor-bind-v1", Identity: encoded, StartedAt: started})
		if cause := ctx.Err(); cause != nil {
			return ResumeRequest{}, cause
		}
		if err != nil {
			return ResumeRequest{}, fmt.Errorf("acknowledge restart successor binding: %w", err)
		}
		if !bytes.Equal(process.Identity, encoded) || !process.StartedAt.Equal(started) {
			return ResumeRequest{}, fmt.Errorf("%w: successor binding acknowledgement differs", state.ErrConflict)
		}
	} else if err != nil {
		return ResumeRequest{}, fmt.Errorf("read restart successor process: %w", err)
	}
	if process.Scope != selection.Scope || process.SlotID != selection.SlotID || process.GenerationID != selection.GenerationID || process.StartedAt.IsZero() || process.Terminal != nil {
		return ResumeRequest{}, fmt.Errorf("%w: successor history is mismatched or closed", state.ErrConflict)
	}
	ref, err := decodeProcessIdentity(process.Identity)
	if err != nil {
		return ResumeRequest{}, err
	}
	if ref.Namespace != identity.Namespace || ref.PodName != identity.PodName || ref.PodUID != identity.PodUID || ref.ContainerName != identity.ContainerName {
		return ResumeRequest{}, fmt.Errorf("%w: bound process differs from completed successor", state.ErrConflict)
	}
	// RecordProcess receipt replay can acknowledge an obsolete binding. Recheck
	// before handing off; this remains a snapshot, never distributed fencing.
	if err := checkRestartSuccessorHead(ctx, history, selection, op, intent); err != nil {
		return ResumeRequest{}, err
	}
	return ResumeRequest{Scope: selection.Scope, SlotID: selection.SlotID, GenerationID: selection.GenerationID, Instance: ref, Plan: intent.Next.request().Plan}, nil
}

func checkRestartSuccessorHead(ctx context.Context, history RestartSuccessorStore, selection RestartSelection, op state.Operation, intent restartOperationIntent) error {
	head, err := history.CurrentGeneration(ctx, selection.Scope, selection.SlotID)
	if cause := ctx.Err(); cause != nil {
		return cause
	}
	if err != nil {
		return fmt.Errorf("read restart successor head: %w", err)
	}
	if !selection.matches(op, head, intent) {
		return fmt.Errorf("%w: completed restart successor is not current", state.ErrConflict)
	}
	return nil
}

func (r Resolver) discoverRestartSuccessor(ctx context.Context, request InstanceRequest, identity PodIdentity) (Instance, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return Instance{}, time.Time{}, err
	}
	if r.Client == nil {
		return Instance{}, time.Time{}, errors.New("unbound restart successor discovery requires an explicit runtime client")
	}
	pod, err := r.Client.Pods(identity.Namespace).Get(ctx, identity.PodName, metav1.GetOptions{})
	if cause := ctx.Err(); cause != nil {
		return Instance{}, time.Time{}, cause
	}
	if err != nil {
		return Instance{}, time.Time{}, fmt.Errorf("read restart successor runtime: %w", err)
	}
	if pod == nil || pod.Namespace != identity.Namespace || pod.Name != identity.PodName || string(pod.UID) != identity.PodUID {
		return Instance{}, time.Time{}, ErrStaleInstance
	}
	desired, err := PrepareInstance(request)
	if err != nil {
		return Instance{}, time.Time{}, err
	}
	if _, err := creationResult(desired, pod, false); err != nil {
		return Instance{}, time.Time{}, err
	}
	if pod.ResourceVersion == "" || pod.DeletionTimestamp != nil {
		return Instance{}, time.Time{}, ErrInstanceUnavailable
	}
	var status *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == identity.ContainerName {
			if status != nil {
				return Instance{}, time.Time{}, ErrInstanceUnavailable
			}
			status = &pod.Status.ContainerStatuses[i]
		}
	}
	if status == nil || strings.TrimSpace(status.ContainerID) == "" {
		return Instance{}, time.Time{}, ErrInstanceUnavailable
	}
	ref := Instance{Namespace: identity.Namespace, PodName: identity.PodName, PodUID: identity.PodUID, ContainerName: identity.ContainerName, ContainerID: status.ContainerID}
	if exit, ok := reportedExit(pod, *status, ref).(*InstanceExitError); ok {
		return ref, exit.StartedAt, nil
	}
	if pod.Status.Phase != corev1.PodRunning || status.State.Running == nil || status.State.Running.StartedAt.IsZero() || status.State.Terminated != nil || status.State.Waiting != nil {
		return Instance{}, time.Time{}, ErrInstanceUnavailable
	}
	return ref, status.State.Running.StartedAt.Time, nil
}
