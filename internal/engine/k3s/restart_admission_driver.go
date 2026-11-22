package k3s

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/daropotter/chmura/internal/state"
)

// RestartAdmissionStore supplies immutable source history and the three durable
// admission steps. sqlite.Store implements it. The caller authorizes scope/intent
// and serializes ownership; these records are not execution leases or fences.
type RestartAdmissionStore interface {
	GetGeneration(context.Context, string, string, string) (state.Generation, error)
	GetProcess(context.Context, string, string, string) (state.Process, error)
	Accept(context.Context, state.Submission) (state.Operation, bool, error)
	Update(context.Context, state.OperationUpdate) (state.Operation, bool, error)
	AdmitRestart(context.Context, state.RestartAdmissionRequest) (state.Generation, bool, error)
}

// ProcessRestartAdmission explicitly selects the retained predecessor, successor
// generation, original creation request and newly admitted allocation/key. The
// idempotency key identifies this operation within its authorized scope. The
// caller must reuse every field on retry; no placement or retry policy is chosen.
type ProcessRestartAdmission struct {
	Scope                string
	SlotID               string
	PreviousGenerationID string
	GenerationID         string
	PreviousVersion      int64
	IdempotencyKey       string
	Previous             InstanceRequest
	Next                 InstanceRequest
}

// AdmitProcessRestart validates retained evidence before acceptance, then
// recovers acceptance/start/successor admission after ambiguous writes. A matching
// current admission is returned only after acknowledgement; errors expose zero
// selection. The steps are separate transactions: an error can leave an accepted
// or running operation without a successor. Retry exactly; no compensation,
// failure decision or runtime action is automatic. The returned selection is an
// admission receipt, not permission to execute: EnsureAdmitted rechecks currency.
func AdmitProcessRestart(ctx context.Context, history RestartAdmissionStore, request ProcessRestartAdmission) (RestartSelection, error) {
	if err := ctx.Err(); err != nil {
		return RestartSelection{}, err
	}
	if history == nil || strings.TrimSpace(request.Scope) == "" || strings.TrimSpace(request.SlotID) == "" || strings.TrimSpace(request.PreviousGenerationID) == "" ||
		strings.TrimSpace(request.GenerationID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" || request.GenerationID == request.PreviousGenerationID || request.PreviousVersion <= 0 || request.PreviousVersion == math.MaxInt64 {
		return RestartSelection{}, errors.New("restart admission requires history, scope, slot, different successor, operation key and a positive predecessor version")
	}
	previous, err := history.GetGeneration(ctx, request.Scope, request.SlotID, request.PreviousGenerationID)
	if cause := ctx.Err(); cause != nil {
		return RestartSelection{}, cause
	}
	if err != nil {
		return RestartSelection{}, fmt.Errorf("read restart admission predecessor: %w", err)
	}
	if previous.Scope != request.Scope || previous.SlotID != request.SlotID || previous.ID != request.PreviousGenerationID || previous.Version != request.PreviousVersion {
		return RestartSelection{}, fmt.Errorf("%w: restart admission predecessor differs", state.ErrConflict)
	}
	process, err := history.GetProcess(ctx, request.Scope, request.SlotID, request.PreviousGenerationID)
	if cause := ctx.Err(); cause != nil {
		return RestartSelection{}, cause
	}
	if err != nil {
		return RestartSelection{}, fmt.Errorf("read restart admission process: %w", err)
	}
	if process.Scope != request.Scope || process.SlotID != request.SlotID || process.GenerationID != request.PreviousGenerationID {
		return RestartSelection{}, fmt.Errorf("%w: restart admission process differs", state.ErrConflict)
	}
	ref, err := decodeProcessIdentity(process.Identity)
	if err != nil {
		return RestartSelection{}, err
	}
	intent, err := PrepareRestartOperationIntent(ProcessRestartRequest{Process: process, Instance: ref, Previous: request.Previous, Next: request.Next}, request.GenerationID, request.PreviousVersion)
	if err != nil {
		return RestartSelection{}, err
	}
	op, _, err := history.Accept(ctx, state.Submission{Scope: request.Scope, IdempotencyKey: request.IdempotencyKey, Kind: state.RestartOperationKind, Intent: intent})
	if cause := ctx.Err(); cause != nil {
		return RestartSelection{}, cause
	}
	if err != nil {
		return RestartSelection{}, fmt.Errorf("acknowledge restart acceptance: %w", err)
	}
	if !matchesRestartAdmissionOperation(op, request.Scope, intent) {
		return RestartSelection{}, fmt.Errorf("%w: restart acceptance acknowledgement differs", state.ErrConflict)
	}
	switch op.Status {
	case state.OperationPending:
		if len(op.Result) != 0 {
			return RestartSelection{}, fmt.Errorf("%w: pending restart has a terminal result", state.ErrConflict)
		}
		started, _, err := history.Update(ctx, state.OperationUpdate{Scope: request.Scope, OperationID: op.ID, ExpectedVersion: op.Version,
			IdempotencyKey: "restart-start-v1", Status: state.OperationRunning})
		if cause := ctx.Err(); cause != nil {
			return RestartSelection{}, cause
		}
		if err != nil {
			return RestartSelection{}, fmt.Errorf("acknowledge restart start: %w", err)
		}
		if !matchesRestartAdmissionOperation(started, request.Scope, intent) || started.ID != op.ID || started.Version != op.Version+1 || started.Status != state.OperationRunning || len(started.Result) != 0 {
			return RestartSelection{}, fmt.Errorf("%w: restart start acknowledgement differs", state.ErrConflict)
		}
		op = started
	case state.OperationRunning:
		if len(op.Result) != 0 {
			return RestartSelection{}, fmt.Errorf("%w: running restart has a terminal result", state.ErrConflict)
		}
	case state.OperationSucceeded:
		// Only historical admission can acknowledge a completed operation.
		// AdmitRestart rejects a fresh admission from terminal progress.
		decoded, err := decodeRestartOperationIntent(intent)
		if err != nil {
			return RestartSelection{}, err
		}
		if _, err := decodeRestartResult(op.Result, intent, decoded); err != nil {
			return RestartSelection{}, err
		}
	default:
		return RestartSelection{}, fmt.Errorf("%w: restart operation cannot admit work", state.ErrConflict)
	}
	generation, _, err := history.AdmitRestart(ctx, state.RestartAdmissionRequest{GenerationRequest: state.GenerationRequest{
		Scope: request.Scope, SlotID: request.SlotID, ID: request.GenerationID, OperationID: op.ID, ExpectedVersion: request.PreviousVersion,
		IdempotencyKey: "restart-admit-v1/" + op.ID, Intent: intent}, PreviousGenerationID: request.PreviousGenerationID})
	if cause := ctx.Err(); cause != nil {
		return RestartSelection{}, cause
	}
	if err != nil {
		return RestartSelection{}, fmt.Errorf("acknowledge restart successor admission: %w", err)
	}
	if generation.Scope != request.Scope || generation.SlotID != request.SlotID || generation.ID != request.GenerationID || generation.OperationID != op.ID ||
		generation.Version != request.PreviousVersion+1 || !bytes.Equal(generation.Intent, intent) {
		return RestartSelection{}, fmt.Errorf("%w: admitted successor is not the selected current generation", state.ErrConflict)
	}
	return RestartSelection{Scope: request.Scope, SlotID: request.SlotID, GenerationID: request.GenerationID, OperationID: op.ID}, nil
}

func matchesRestartAdmissionOperation(op state.Operation, scope string, intent []byte) bool {
	return strings.TrimSpace(op.ID) != "" && op.Scope == scope && op.Kind == state.RestartOperationKind && op.Version > 0 && bytes.Equal(op.Intent, intent)
}
