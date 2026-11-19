package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/daropotter/chmura/internal/state"
)

// AdmitRestart atomically checks the predecessor's terminal history, a running
// same-scope restart operation with matching intent and the expected slot head,
// then appends the successor. It creates no operation or observed process.
// Exact historical retries acknowledge the receipt and return the current head,
// even after supersession/completion. They never grant execution permission.
func (s *Store) AdmitRestart(ctx context.Context, request state.RestartAdmissionRequest) (state.Generation, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.Generation{}, false, err
	}
	if strings.TrimSpace(request.PreviousGenerationID) == "" || request.ExpectedVersion <= 0 {
		return state.Generation{}, false, errors.New("restart admission requires a predecessor and positive expected slot version")
	}
	generation, created, err := s.admitGeneration(ctx, request.GenerationRequest, request.PreviousGenerationID)
	if err != nil && ctx.Err() != nil {
		return state.Generation{}, false, ctx.Err()
	}
	return generation, created, err
}

// GetCurrentRestart checks an explicit execution selection in one transaction.
// Completed/canceled operations, superseded generations and an already bound
// successor conflict. Bound-process recovery must not rerun creation. A returned
// snapshot can become stale immediately: external owner serialization is still
// required across this check and runtime I/O. This method acquires no lease.
func (s *Store) GetCurrentRestart(ctx context.Context, scope, slot, generation, operation string) (state.Restart, error) {
	restart, err := s.currentRestart(ctx, scope, slot, generation, operation)
	if err != nil && ctx.Err() != nil {
		return state.Restart{}, ctx.Err()
	}
	return restart, err
}

func (s *Store) currentRestart(ctx context.Context, scope, slot, generation, operation string) (state.Restart, error) {
	if err := ctx.Err(); err != nil {
		return state.Restart{}, err
	}
	if strings.TrimSpace(scope) == "" || strings.TrimSpace(slot) == "" || strings.TrimSpace(generation) == "" || strings.TrimSpace(operation) == "" {
		return state.Restart{}, errors.New("current restart requires scope, slot, generation and operation")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return state.Restart{}, fmt.Errorf("begin current restart read: %w", err)
	}
	defer tx.Rollback()
	head, err := scanGeneration(tx.QueryRowContext(ctx, currentGenerationQuery, scope, slot))
	if errors.Is(err, sql.ErrNoRows) {
		return state.Restart{}, state.ErrNotFound
	}
	if err != nil {
		return state.Restart{}, fmt.Errorf("read restart head: %w", err)
	}
	if head.ID != generation || head.OperationID != operation {
		return state.Restart{}, state.ErrConflict
	}
	restart, err := restartSnapshot(ctx, tx, head, "", true)
	if err != nil {
		return state.Restart{}, err
	}
	if err := tx.Commit(); err != nil {
		return state.Restart{}, fmt.Errorf("confirm current restart read: %w", err)
	}
	return restart, nil
}

// Receipt replay checks immutable facts but permits terminal operations. Fresh
// admission and execution selection additionally require a running operation
// and an unbound successor. A live or closed binding needs reconciliation.
func restartSnapshot(ctx context.Context, tx *sql.Tx, generation state.Generation, previousID string, executable bool) (state.Restart, error) {
	if generation.Version <= 1 {
		return state.Restart{}, state.ErrConflict
	}
	op, err := scanOperation(tx.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM operations WHERE scope = ? AND id = ?`, generation.Scope, generation.OperationID))
	if errors.Is(err, sql.ErrNoRows) {
		return state.Restart{}, state.ErrNotFound
	}
	if err != nil {
		return state.Restart{}, fmt.Errorf("read restart operation: %w", err)
	}
	if op.Kind != state.RestartOperationKind || !bytes.Equal(op.Intent, generation.Intent) || executable && op.Status != state.OperationRunning {
		return state.Restart{}, state.ErrConflict
	}
	previous, err := scanGeneration(tx.QueryRowContext(ctx, `SELECT `+generationColumns+` FROM generations WHERE scope = ? AND slot_id = ? AND version = ?`, generation.Scope, generation.SlotID, generation.Version-1))
	if errors.Is(err, sql.ErrNoRows) {
		return state.Restart{}, state.ErrNotFound
	}
	if err != nil {
		return state.Restart{}, fmt.Errorf("read restart predecessor: %w", err)
	}
	if previousID != "" && previous.ID != previousID {
		return state.Restart{}, state.ErrConflict
	}
	process, err := scanProcess(tx.QueryRowContext(ctx, selectProcess, previous.Scope, previous.SlotID, previous.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return state.Restart{}, state.ErrNotFound
	}
	if err != nil {
		return state.Restart{}, fmt.Errorf("read restart predecessor history: %w", err)
	}
	if process.Terminal == nil || (process.Terminal.Cause != state.ProcessExited && process.Terminal.Cause != state.ProcessUnhealthy) ||
		process.Terminal.Cause == state.ProcessUnhealthy && !process.EverReady {
		return state.Restart{}, state.ErrConflict
	}
	if executable {
		var bound bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM generation_processes WHERE scope = ? AND slot_id = ? AND generation_id = ?)`, generation.Scope, generation.SlotID, generation.ID).Scan(&bound); err != nil {
			return state.Restart{}, fmt.Errorf("read restart successor binding: %w", err)
		}
		if bound {
			return state.Restart{}, state.ErrConflict
		}
	}
	return state.Restart{Generation: generation, Previous: previous, Process: process, Operation: op}, nil
}
