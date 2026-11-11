package sqlite

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/daropotter/chmura/internal/state"
)

// Accept persists an immutable operation before returning its stable ID. The
// boolean reports a new acceptance; matching retries return the existing record.
// A canceled/lost response can follow a committed insert: retry the same key.
// The scope must already be authorized; acceptance creates no domain objects.
func (s *Store) Accept(ctx context.Context, request state.Submission) (state.Operation, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.Operation{}, false, err
	}
	if strings.TrimSpace(request.Scope) == "" || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.Kind) == "" || len(request.Intent) == 0 {
		return state.Operation{}, false, errors.New("operation requires an explicit scope, key, kind and canonical intent")
	}
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	keyHash := hex.EncodeToString(key[:])
	operation, err := scanOperation(s.db.QueryRowContext(ctx, `INSERT INTO operations (id, scope, key_hash, kind, intent)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (scope, key_hash) DO NOTHING
		RETURNING `+operationColumns, "op_"+rand.Text(), request.Scope, keyHash, request.Kind, request.Intent))
	if err == nil {
		return operation, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state.Operation{}, false, fmt.Errorf("accept operation: %w", err)
	}
	operation, err = scanOperation(s.db.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM operations WHERE scope = ? AND key_hash = ?`, request.Scope, keyHash))
	if err != nil {
		return state.Operation{}, false, fmt.Errorf("read operation acceptance: %w", err)
	}
	if operation.Kind != request.Kind || !bytes.Equal(operation.Intent, request.Intent) {
		return state.Operation{}, false, state.ErrConflict
	}
	return operation, false, nil
}

// Get reads an operation only within the caller's already authorized scope.
func (s *Store) Get(ctx context.Context, scope, id string) (state.Operation, error) {
	operation, err := scanOperation(s.db.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM operations WHERE scope = ? AND id = ?`, scope, id))
	if errors.Is(err, sql.ErrNoRows) {
		return state.Operation{}, state.ErrNotFound
	}
	if err != nil {
		return state.Operation{}, fmt.Errorf("read operation: %w", err)
	}
	return operation, nil
}

const operationColumns = "id, scope, kind, intent, status, version, result"

func scanOperation(row *sql.Row) (state.Operation, error) {
	var op state.Operation
	err := row.Scan(&op.ID, &op.Scope, &op.Kind, &op.Intent, &op.Status, &op.Version, &op.Result)
	return op, err
}
