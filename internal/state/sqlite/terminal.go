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

	"github.com/daropotter/chmura/internal/state"
)

// RecordProcessTerminal permanently closes a bound current generation. The
// first decision wins; only its exact key/facts may replay, even after a newer
// generation is admitted. The boolean reports a new terminal record/receipt.
// This is retained evidence, never worker ownership or restart permission.
// A canceled/lost response may follow commit; retry the identical request.
func (s *Store) RecordProcessTerminal(ctx context.Context, request state.ProcessTerminalObservation) (state.Process, bool, error) {
	process, recorded, err := s.recordProcessTerminal(ctx, request)
	if err != nil && ctx.Err() != nil {
		return state.Process{}, false, ctx.Err()
	}
	return process, recorded, err
}

func (s *Store) recordProcessTerminal(ctx context.Context, request state.ProcessTerminalObservation) (state.Process, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.Process{}, false, err
	}
	terminal := request.Terminal
	if strings.TrimSpace(request.Scope) == "" || strings.TrimSpace(request.SlotID) == "" || strings.TrimSpace(request.GenerationID) == "" ||
		strings.TrimSpace(request.IdempotencyKey) == "" || len(request.Identity) == 0 || request.StartedAt.IsZero() ||
		terminal.OccurredAt.IsZero() || terminal.OccurredAt.Before(request.StartedAt) || len(terminal.Evidence) == 0 ||
		(terminal.Cause != state.ProcessExited && terminal.Cause != state.ProcessStartupTimeout && terminal.Cause != state.ProcessUnhealthy) {
		return state.Process{}, false, errors.New("process terminal requires scope, slot, generation, key, exact process/start and valid cause, time and evidence")
	}
	occurred, err := terminal.OccurredAt.UTC().MarshalText()
	if err != nil {
		return state.Process{}, false, fmt.Errorf("encode process terminal time: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return state.Process{}, false, fmt.Errorf("begin process terminal: %w", err)
	}
	defer tx.Rollback()
	process, err := scanProcess(tx.QueryRowContext(ctx, selectProcess, request.Scope, request.SlotID, request.GenerationID))
	if errors.Is(err, sql.ErrNoRows) {
		return state.Process{}, false, state.ErrNotFound
	}
	if err != nil {
		return state.Process{}, false, fmt.Errorf("read terminal process binding: %w", err)
	}
	if !bytes.Equal(process.Identity, request.Identity) || !process.StartedAt.Equal(request.StartedAt) {
		return state.Process{}, false, state.ErrConflict
	}
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	keyHash := hex.EncodeToString(key[:])
	if previous := process.Terminal; previous != nil {
		var previousKey string
		if err := tx.QueryRowContext(ctx, `SELECT key_hash FROM process_terminals WHERE scope = ? AND slot_id = ? AND generation_id = ?`, request.Scope, request.SlotID, request.GenerationID).Scan(&previousKey); err != nil {
			return state.Process{}, false, fmt.Errorf("read process terminal receipt: %w", err)
		}
		if previousKey != keyHash || previous.Cause != terminal.Cause || !previous.OccurredAt.Equal(terminal.OccurredAt) || !bytes.Equal(previous.Evidence, terminal.Evidence) {
			return state.Process{}, false, state.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return state.Process{}, false, fmt.Errorf("confirm process terminal: %w", err)
		}
		return process, false, nil
	}
	head, err := scanGeneration(tx.QueryRowContext(ctx, currentGenerationQuery, request.Scope, request.SlotID))
	if err != nil {
		return state.Process{}, false, fmt.Errorf("read terminal slot head: %w", err)
	}
	if head.ID != request.GenerationID || (terminal.Cause == state.ProcessStartupTimeout && process.EverReady) ||
		(terminal.Cause == state.ProcessUnhealthy && !process.EverReady) {
		return state.Process{}, false, state.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO process_terminals (scope, slot_id, generation_id, key_hash, cause, occurred_at, evidence) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		request.Scope, request.SlotID, request.GenerationID, keyHash, terminal.Cause, string(occurred), terminal.Evidence); err != nil {
		return state.Process{}, false, fmt.Errorf("retain process terminal and receipt: %w", err)
	}
	process, err = scanProcess(tx.QueryRowContext(ctx, selectProcess, request.Scope, request.SlotID, request.GenerationID))
	if err != nil {
		return state.Process{}, false, fmt.Errorf("read retained process terminal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return state.Process{}, false, fmt.Errorf("commit process terminal: %w", err)
	}
	return process, true, nil
}
