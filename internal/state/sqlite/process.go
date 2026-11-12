package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/daropotter/chmura/internal/state"
)

// RecordProcess binds immutable runtime identity/start and monotonically retains
// first readiness. The boolean means a new observation receipt was committed,
// even when its facts were already known. Matching retries return stored facts.
// Fresh writes require the current desired generation; historical replays grant
// no right to resume a superseded process. A canceled/lost response may follow
// commit; repeat the same request to recover. Call outside probe callbacks.
func (s *Store) RecordProcess(ctx context.Context, request state.ProcessObservation) (state.Process, bool, error) {
	process, recorded, err := s.recordProcess(ctx, request)
	if err != nil && ctx.Err() != nil {
		return state.Process{}, false, ctx.Err()
	}
	return process, recorded, err
}

func (s *Store) recordProcess(ctx context.Context, request state.ProcessObservation) (state.Process, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.Process{}, false, err
	}
	if strings.TrimSpace(request.Scope) == "" || strings.TrimSpace(request.SlotID) == "" || strings.TrimSpace(request.GenerationID) == "" ||
		strings.TrimSpace(request.IdempotencyKey) == "" || len(request.Identity) == 0 || request.StartedAt.IsZero() {
		return state.Process{}, false, errors.New("process observation requires scope, slot, generation, key, complete identity and actual start time")
	}
	started, err := request.StartedAt.UTC().MarshalText()
	if err != nil {
		return state.Process{}, false, fmt.Errorf("encode process start time: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return state.Process{}, false, fmt.Errorf("begin process observation: %w", err)
	}
	defer tx.Rollback()
	process, err := scanProcess(tx.QueryRowContext(ctx, selectProcess, request.Scope, request.SlotID, request.GenerationID))
	bound := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state.Process{}, false, fmt.Errorf("read process binding: %w", err)
	}
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	keyHash := hex.EncodeToString(key[:])
	if bound {
		if !bytes.Equal(process.Identity, request.Identity) || !process.StartedAt.Equal(request.StartedAt) {
			return state.Process{}, false, state.ErrConflict
		}
		var ready bool
		err = tx.QueryRowContext(ctx, `SELECT ever_ready FROM process_observations WHERE scope = ? AND slot_id = ? AND generation_id = ? AND key_hash = ?`, request.Scope, request.SlotID, request.GenerationID, keyHash).Scan(&ready)
		if err == nil {
			if ready != request.EverReady {
				return state.Process{}, false, state.ErrConflict
			}
			if err := tx.Commit(); err != nil {
				return state.Process{}, false, fmt.Errorf("confirm process observation: %w", err)
			}
			return process, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return state.Process{}, false, fmt.Errorf("read process receipt: %w", err)
		}
	}
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM generations WHERE scope = ? AND slot_id = ? AND id = ?`, request.Scope, request.SlotID, request.GenerationID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Process{}, false, state.ErrNotFound
	}
	if err != nil {
		return state.Process{}, false, fmt.Errorf("read process generation: %w", err)
	}
	head, err := scanGeneration(tx.QueryRowContext(ctx, currentGenerationQuery, request.Scope, request.SlotID))
	if err != nil {
		return state.Process{}, false, fmt.Errorf("read process slot head: %w", err)
	}
	if head.ID != request.GenerationID {
		return state.Process{}, false, state.ErrConflict
	}
	if !bound {
		identityHash := sha256.Sum256(request.Identity)
		process, err = scanProcess(tx.QueryRowContext(ctx, `INSERT INTO generation_processes (scope, slot_id, generation_id, identity, identity_hash, started_at, ever_ready)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING RETURNING `+processColumns,
			request.Scope, request.SlotID, request.GenerationID, request.Identity, hex.EncodeToString(identityHash[:]), string(started), request.EverReady))
		if errors.Is(err, sql.ErrNoRows) {
			return state.Process{}, false, state.ErrConflict
		}
		if err != nil {
			return state.Process{}, false, fmt.Errorf("bind generation process: %w", err)
		}
	} else if request.EverReady && !process.EverReady {
		process, err = scanProcess(tx.QueryRowContext(ctx, `UPDATE generation_processes SET ever_ready = 1 WHERE scope = ? AND slot_id = ? AND generation_id = ? RETURNING `+processColumns, request.Scope, request.SlotID, request.GenerationID))
		if err != nil {
			return state.Process{}, false, fmt.Errorf("retain first readiness: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO process_observations (scope, slot_id, generation_id, key_hash, ever_ready) VALUES (?, ?, ?, ?, ?)`, request.Scope, request.SlotID, request.GenerationID, keyHash, request.EverReady); err != nil {
		return state.Process{}, false, fmt.Errorf("record process receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return state.Process{}, false, fmt.Errorf("commit process observation: %w", err)
	}
	return process, true, nil
}

// GetProcess reads retained facts for the exact generation in an authorized scope.
// No row means unbound/missing, never assumed current readiness or failed startup.
func (s *Store) GetProcess(ctx context.Context, scope, slot, generation string) (state.Process, error) {
	process, err := scanProcess(s.db.QueryRowContext(ctx, selectProcess, scope, slot, generation))
	if errors.Is(err, sql.ErrNoRows) {
		return state.Process{}, state.ErrNotFound
	}
	if err != nil {
		return state.Process{}, fmt.Errorf("read retained process: %w", err)
	}
	return process, nil
}

const processColumns = "scope, slot_id, generation_id, identity, started_at, ever_ready"
const selectProcess = `SELECT ` + processColumns + ` FROM generation_processes WHERE scope = ? AND slot_id = ? AND generation_id = ?`

func scanProcess(row *sql.Row) (state.Process, error) {
	var process state.Process
	var started string
	if err := row.Scan(&process.Scope, &process.SlotID, &process.GenerationID, &process.Identity, &started, &process.EverReady); err != nil {
		return state.Process{}, err
	}
	var err error
	process.StartedAt, err = time.Parse(time.RFC3339Nano, started)
	return process, err
}
