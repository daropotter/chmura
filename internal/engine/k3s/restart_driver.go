package k3s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/daropotter/chmura/internal/state"
)

// RestartJournal reads admission/currency and acknowledges durable completion.
// These snapshots are not a lease: an external owner serializes execution with
// admissions/cancellation and drains reporters. sqlite.Store implements it.
type RestartJournal interface {
	Get(context.Context, string, string) (state.Operation, error)
	GetGeneration(context.Context, string, string, string) (state.Generation, error)
	GetCurrentRestart(context.Context, string, string, string, string) (state.Restart, error)
	Update(context.Context, state.OperationUpdate) (state.Operation, bool, error)
}

// RestartSelection identifies an explicitly authorized admitted operation.
type RestartSelection struct {
	Scope        string
	SlotID       string
	GenerationID string
	OperationID  string
}

// RestartProgress waits for retirement or returns durably acknowledged creation.
// Identity is not evidence of a started container, image pull or readiness.
type RestartProgress struct {
	Retiring bool
	Identity PodIdentity
}

// EnsureAdmitted recovers private v1 intent and performs one current restart
// step. It exposes creation only after durable operation completion. Historical
// success replays without runtime I/O, even after supersession or Pod deletion.
// The caller owns authorization, serialization, deadlines/backoff and reporter
// shutdown. A running operation with a bound successor needs reconciliation;
// this method never reruns creation in that case or automatically admits work.
func (r Restarter) EnsureAdmitted(ctx context.Context, journal RestartJournal, selection RestartSelection) (RestartProgress, error) {
	if err := ctx.Err(); err != nil {
		return RestartProgress{}, err
	}
	if journal == nil || strings.TrimSpace(selection.Scope) == "" || strings.TrimSpace(selection.SlotID) == "" || strings.TrimSpace(selection.GenerationID) == "" || strings.TrimSpace(selection.OperationID) == "" {
		return RestartProgress{}, errors.New("admitted restart requires journal, scope, slot, generation and operation")
	}
	op, err := journal.Get(ctx, selection.Scope, selection.OperationID)
	if cause := ctx.Err(); cause != nil {
		return RestartProgress{}, cause
	}
	if err != nil {
		return RestartProgress{}, fmt.Errorf("read restart operation: %w", err)
	}
	generation, err := journal.GetGeneration(ctx, selection.Scope, selection.SlotID, selection.GenerationID)
	if cause := ctx.Err(); cause != nil {
		return RestartProgress{}, cause
	}
	if err != nil {
		return RestartProgress{}, fmt.Errorf("read admitted restart generation: %w", err)
	}
	intent, err := decodeRestartOperationIntent(op.Intent)
	if err != nil {
		return RestartProgress{}, err
	}
	if !selection.matches(op, generation, intent) {
		return RestartProgress{}, fmt.Errorf("%w: restart selection does not match admitted intent", state.ErrConflict)
	}
	if op.Status == state.OperationSucceeded {
		identity, err := decodeRestartResult(op.Result, op.Intent, intent)
		if err != nil {
			return RestartProgress{}, err
		}
		return RestartProgress{Identity: identity}, nil
	}
	if op.Status != state.OperationRunning || len(op.Result) != 0 {
		return RestartProgress{}, fmt.Errorf("%w: restart operation is not running", state.ErrConflict)
	}
	current, err := journal.GetCurrentRestart(ctx, selection.Scope, selection.SlotID, selection.GenerationID, selection.OperationID)
	if cause := ctx.Err(); cause != nil {
		return RestartProgress{}, cause
	}
	if err != nil {
		return RestartProgress{}, fmt.Errorf("read current restart: %w", err)
	}
	if !selection.matches(current.Operation, current.Generation, intent) || current.Operation.Version != op.Version || current.Operation.Status != state.OperationRunning ||
		!bytes.Equal(current.Operation.Intent, op.Intent) || len(current.Operation.Result) != 0 ||
		current.Previous.Scope != selection.Scope || current.Previous.SlotID != selection.SlotID || current.Previous.ID != intent.PreviousGenerationID || current.Previous.Version != intent.PreviousVersion ||
		current.Process.Scope != selection.Scope || current.Process.SlotID != selection.SlotID || current.Process.GenerationID != intent.PreviousGenerationID {
		return RestartProgress{}, fmt.Errorf("%w: current restart does not match selected lineage", state.ErrConflict)
	}
	ref, err := decodeProcessIdentity(current.Process.Identity)
	if err != nil {
		return RestartProgress{}, err
	}
	request, err := PrepareProcessRestart(ProcessRestartRequest{Process: current.Process, Instance: ref, Previous: intent.Previous.request(), Next: intent.Next.request()})
	if err != nil {
		return RestartProgress{}, err
	}
	step, err := r.Ensure(ctx, request)
	if err != nil {
		return RestartProgress{}, err
	}
	if step.Retiring {
		return RestartProgress{Retiring: true}, nil
	}
	result, err := encodeRestartResult(step.Instance.Identity, op.Intent, intent)
	if err != nil {
		return RestartProgress{}, err
	}
	ack, _, err := journal.Update(ctx, state.OperationUpdate{Scope: selection.Scope, OperationID: selection.OperationID,
		IdempotencyKey: "restart-complete-v1", ExpectedVersion: op.Version, Status: state.OperationSucceeded, Result: result})
	if cause := ctx.Err(); cause != nil {
		return RestartProgress{}, cause
	}
	if err != nil {
		return RestartProgress{}, fmt.Errorf("acknowledge restart creation: %w", err)
	}
	if !selection.matches(ack, generation, intent) || !bytes.Equal(ack.Intent, op.Intent) || ack.Status != state.OperationSucceeded || ack.Version != op.Version+1 || !bytes.Equal(ack.Result, result) {
		return RestartProgress{}, fmt.Errorf("%w: restart completion acknowledgement differs", state.ErrConflict)
	}
	return RestartProgress{Identity: step.Instance.Identity}, nil
}

func (selection RestartSelection) matches(op state.Operation, generation state.Generation, intent restartOperationIntent) bool {
	return op.Scope == selection.Scope && op.ID == selection.OperationID && op.Kind == state.RestartOperationKind && op.Version > 0 &&
		generation.Scope == selection.Scope && generation.SlotID == selection.SlotID && generation.ID == selection.GenerationID && generation.OperationID == selection.OperationID &&
		bytes.Equal(op.Intent, generation.Intent) && intent.Scope == selection.Scope && intent.SlotID == selection.SlotID && intent.GenerationID == selection.GenerationID && generation.Version == intent.PreviousVersion+1
}

// restartResult omits the transient Created flag so a replayed creation yields
// identical completion bytes. The intent hash prevents cross-intent reuse.
type restartResult struct {
	Version       int    `json:"version"`
	Engine        string `json:"engine"`
	IntentSHA256  string `json:"intent-sha256"`
	Namespace     string `json:"namespace"`
	PodName       string `json:"pod-name"`
	PodUID        string `json:"pod-uid"`
	ContainerName string `json:"container-name"`
}

func encodeRestartResult(identity PodIdentity, data []byte, intent restartOperationIntent) ([]byte, error) {
	desired, err := PrepareInstance(intent.Next.request())
	if err != nil {
		return nil, err
	}
	if identity.Namespace != desired.Namespace || identity.PodName != desired.Name || identity.ContainerName != desired.Spec.Containers[0].Name || strings.TrimSpace(identity.PodUID) == "" || !utf8.ValidString(identity.PodUID) {
		return nil, fmt.Errorf("%w: restart result does not match successor", state.ErrConflict)
	}
	digest := sha256.Sum256(data)
	return json.Marshal(restartResult{1, "k3s", hex.EncodeToString(digest[:]), identity.Namespace, identity.PodName, identity.PodUID, identity.ContainerName})
}

func decodeRestartResult(data, acceptedIntent []byte, intent restartOperationIntent) (PodIdentity, error) {
	var result restartResult
	if err := json.Unmarshal(data, &result); err != nil {
		return PodIdentity{}, fmt.Errorf("decode restart result: %w", err)
	}
	identity := PodIdentity{Namespace: result.Namespace, PodName: result.PodName, PodUID: result.PodUID, ContainerName: result.ContainerName}
	canonical, err := encodeRestartResult(identity, acceptedIntent, intent)
	if err != nil || !bytes.Equal(data, canonical) {
		return PodIdentity{}, fmt.Errorf("%w: unsupported or noncanonical restart result", state.ErrConflict)
	}
	return identity, nil
}
