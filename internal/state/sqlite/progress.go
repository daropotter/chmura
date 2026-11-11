package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/daropotter/chmura/internal/state"
)

// Update atomically advances progress and retains the update's key/intent.
// A matching retry returns the current snapshot without changing it, including
// after later transitions. The boolean reports a newly committed transition.
// This records the owner's decision; it neither authorizes runtime I/O nor
// establishes worker ownership. A terminal result never changes or resumes.
func (s *Store) Update(ctx context.Context, request state.OperationUpdate) (state.Operation, bool, error) {
	op, changed, err := s.update(ctx, request)
	// SQLite may return SQLITE_BUSY rather than the expired context when
	// BEGIN IMMEDIATE finishes waiting. Keep cancellation's caller meaning.
	if err != nil && ctx.Err() != nil {
		return state.Operation{}, false, ctx.Err()
	}
	return op, changed, err
}

func (s *Store) update(ctx context.Context, request state.OperationUpdate) (state.Operation, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.Operation{}, false, err
	}
	terminal := request.Status == state.OperationSucceeded || request.Status == state.OperationFailed || request.Status == state.OperationCanceled
	if strings.TrimSpace(request.Scope) == "" || strings.TrimSpace(request.OperationID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" ||
		request.ExpectedVersion <= 0 || request.ExpectedVersion == math.MaxInt64 ||
		(!terminal && request.Status != state.OperationRunning) || terminal != (len(request.Result) > 0) {
		return state.Operation{}, false, errors.New("operation update requires scope, identity, key, positive version and a valid status/result")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return state.Operation{}, false, fmt.Errorf("begin operation update: %w", err)
	}
	defer tx.Rollback()
	op, err := scanOperation(tx.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM operations WHERE scope = ? AND id = ?`, request.Scope, request.OperationID))
	if errors.Is(err, sql.ErrNoRows) {
		return state.Operation{}, false, state.ErrNotFound
	}
	if err != nil {
		return state.Operation{}, false, fmt.Errorf("read operation progress: %w", err)
	}
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	keyHash := hex.EncodeToString(key[:])
	var version int64
	var status state.OperationStatus
	var result []byte
	err = tx.QueryRowContext(ctx, `SELECT expected_version, status, result FROM operation_updates WHERE operation_id = ? AND key_hash = ?`, op.ID, keyHash).
		Scan(&version, &status, &result)
	if err == nil {
		if version != request.ExpectedVersion || status != request.Status || !bytes.Equal(result, request.Result) {
			return state.Operation{}, false, state.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return state.Operation{}, false, fmt.Errorf("confirm operation update: %w", err)
		}
		return op, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state.Operation{}, false, fmt.Errorf("read operation update receipt: %w", err)
	}
	if op.Version != request.ExpectedVersion || !canAdvance(op.Status, request.Status) {
		return state.Operation{}, false, state.ErrConflict
	}
	result = bytes.Clone(request.Result)
	if result == nil {
		result = []byte{}
	}
	changed, err := tx.ExecContext(ctx, `UPDATE operations SET status = ?, version = version + 1, result = ? WHERE scope = ? AND id = ? AND version = ?`, request.Status, result, op.Scope, op.ID, request.ExpectedVersion)
	if err != nil {
		return state.Operation{}, false, fmt.Errorf("advance operation: %w", err)
	}
	count, err := changed.RowsAffected()
	if err != nil {
		return state.Operation{}, false, fmt.Errorf("confirm operation progress: %w", err)
	}
	if count != 1 {
		return state.Operation{}, false, state.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO operation_updates (operation_id, key_hash, expected_version, status, result) VALUES (?, ?, ?, ?, ?)`, op.ID, keyHash, request.ExpectedVersion, request.Status, result); err != nil {
		return state.Operation{}, false, fmt.Errorf("record operation update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return state.Operation{}, false, fmt.Errorf("commit operation progress: %w", err)
	}
	op.Status, op.Version, op.Result = request.Status, op.Version+1, result
	return op, true, nil
}

func canAdvance(previous, next state.OperationStatus) bool {
	switch previous {
	case state.OperationPending:
		return next == state.OperationRunning || next == state.OperationFailed || next == state.OperationCanceled
	case state.OperationRunning:
		return next == state.OperationSucceeded || next == state.OperationFailed || next == state.OperationCanceled
	default:
		return false
	}
}
