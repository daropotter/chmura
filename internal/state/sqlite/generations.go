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

// AdmitGeneration appends immutable desired intent using an explicit slot
// version and a running same-scope operation. One operation admits at most one
// generation per slot. Matching retries return the current head with false,
// even after later admissions or operation completion; they never restore it.
// A canceled/lost response can follow commit; retry the same request to recover.
// Admission is not runtime execution permission, a lease or physical fencing.
func (s *Store) AdmitGeneration(ctx context.Context, request state.GenerationRequest) (state.Generation, bool, error) {
	generation, created, err := s.admitGeneration(ctx, request)
	if err != nil && ctx.Err() != nil {
		return state.Generation{}, false, ctx.Err()
	}
	return generation, created, err
}

func (s *Store) admitGeneration(ctx context.Context, request state.GenerationRequest) (state.Generation, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.Generation{}, false, err
	}
	if strings.TrimSpace(request.Scope) == "" || strings.TrimSpace(request.SlotID) == "" || strings.TrimSpace(request.ID) == "" ||
		strings.TrimSpace(request.OperationID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" ||
		request.ExpectedVersion < 0 || request.ExpectedVersion == math.MaxInt64 || len(request.Intent) == 0 {
		return state.Generation{}, false, errors.New("generation admission requires scope, slot, identity, operation, key, nonnegative version and canonical intent")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return state.Generation{}, false, fmt.Errorf("begin generation admission: %w", err)
	}
	defer tx.Rollback()
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	keyHash := hex.EncodeToString(key[:])
	stored, err := scanGeneration(tx.QueryRowContext(ctx, `SELECT `+generationColumns+` FROM generations WHERE scope = ? AND slot_id = ? AND key_hash = ?`, request.Scope, request.SlotID, keyHash))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state.Generation{}, false, fmt.Errorf("read generation admission: %w", err)
	}
	if err == nil {
		if stored.ID != request.ID || stored.OperationID != request.OperationID || stored.Version-1 != request.ExpectedVersion || !bytes.Equal(stored.Intent, request.Intent) {
			return state.Generation{}, false, state.ErrConflict
		}
		head, err := scanGeneration(tx.QueryRowContext(ctx, currentGenerationQuery, request.Scope, request.SlotID))
		if err != nil {
			return state.Generation{}, false, fmt.Errorf("read current generation: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return state.Generation{}, false, fmt.Errorf("confirm generation admission: %w", err)
		}
		return head, false, nil
	}
	var status state.OperationStatus
	err = tx.QueryRowContext(ctx, `SELECT status FROM operations WHERE scope = ? AND id = ?`, request.Scope, request.OperationID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Generation{}, false, state.ErrNotFound
	}
	if err != nil {
		return state.Generation{}, false, fmt.Errorf("read generation operation: %w", err)
	}
	if status != state.OperationRunning {
		return state.Generation{}, false, state.ErrConflict
	}
	head, err := scanGeneration(tx.QueryRowContext(ctx, currentGenerationQuery, request.Scope, request.SlotID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state.Generation{}, false, fmt.Errorf("read slot head: %w", err)
	}
	if head.Version != request.ExpectedVersion {
		return state.Generation{}, false, state.ErrConflict
	}
	generation, err := scanGeneration(tx.QueryRowContext(ctx, `INSERT INTO generations (scope, slot_id, id, operation_id, key_hash, version, intent)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING RETURNING `+generationColumns,
		request.Scope, request.SlotID, request.ID, request.OperationID, keyHash, request.ExpectedVersion+1, request.Intent))
	if errors.Is(err, sql.ErrNoRows) {
		return state.Generation{}, false, state.ErrConflict
	}
	if err != nil {
		return state.Generation{}, false, fmt.Errorf("admit generation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return state.Generation{}, false, fmt.Errorf("commit generation admission: %w", err)
	}
	return generation, true, nil
}

// CurrentGeneration returns the latest admitted desired generation of a slot.
func (s *Store) CurrentGeneration(ctx context.Context, scope, slot string) (state.Generation, error) {
	return generationResult(s.db.QueryRowContext(ctx, currentGenerationQuery, scope, slot))
}

// GetGeneration reads immutable history within the already authorized scope/slot.
func (s *Store) GetGeneration(ctx context.Context, scope, slot, id string) (state.Generation, error) {
	return generationResult(s.db.QueryRowContext(ctx, `SELECT `+generationColumns+` FROM generations WHERE scope = ? AND slot_id = ? AND id = ?`, scope, slot, id))
}

const generationColumns = "scope, slot_id, id, operation_id, version, intent"
const currentGenerationQuery = `SELECT ` + generationColumns + ` FROM generations WHERE scope = ? AND slot_id = ? ORDER BY version DESC LIMIT 1`

func scanGeneration(row *sql.Row) (state.Generation, error) {
	var generation state.Generation
	err := row.Scan(&generation.Scope, &generation.SlotID, &generation.ID, &generation.OperationID, &generation.Version, &generation.Intent)
	return generation, err
}

func generationResult(row *sql.Row) (state.Generation, error) {
	generation, err := scanGeneration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Generation{}, state.ErrNotFound
	}
	if err != nil {
		return state.Generation{}, fmt.Errorf("read generation: %w", err)
	}
	return generation, nil
}
