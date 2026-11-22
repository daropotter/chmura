package sqlite

import (
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

// AdmitRevision atomically compares the base pair and appends a desired-revision
// selection/receipt. One running same-project operation selects at most once.
// Matching retries return the current head with false, including after later
// selections or operation completion; they never restore an old selection.
// Lost/canceled replies can follow commit: retry exactly. This grants no lease.
func (s *Store) AdmitRevision(ctx context.Context, request state.RevisionAdmission) (state.RevisionSelection, bool, error) {
	head, admitted, err := s.admitRevision(ctx, request)
	if err != nil && ctx.Err() != nil {
		return state.RevisionSelection{}, false, ctx.Err()
	}
	return head, admitted, err
}

func (s *Store) admitRevision(ctx context.Context, request state.RevisionAdmission) (state.RevisionSelection, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.RevisionSelection{}, false, err
	}
	if strings.TrimSpace(request.SpaceID) == "" || strings.TrimSpace(request.ProjectID) == "" || strings.TrimSpace(request.RevisionID) == "" ||
		strings.TrimSpace(request.OperationID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" ||
		request.ExpectedVersion < 0 || request.ExpectedVersion == math.MaxInt64 ||
		request.ExpectedVersion == 0 && request.ExpectedRevisionID != "" || request.ExpectedVersion > 0 && strings.TrimSpace(request.ExpectedRevisionID) == "" {
		return state.RevisionSelection{}, false, errors.New("revision admission requires space, project, snapshot, operation, key and a valid base version/revision pair")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return state.RevisionSelection{}, false, fmt.Errorf("begin revision admission: %w", err)
	}
	defer tx.Rollback()
	var projectID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE space_id = ? AND id = ?`, request.SpaceID, request.ProjectID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return state.RevisionSelection{}, false, state.ErrNotFound
	}
	if err != nil {
		return state.RevisionSelection{}, false, fmt.Errorf("read admission project: %w", err)
	}
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	keyHash := hex.EncodeToString(key[:])
	stored, err := scanRevisionSelection(tx.QueryRowContext(ctx, `SELECT project_id, revision_id, operation_id, version, base_revision_id
		FROM revision_selections WHERE project_id = ? AND key_hash = ?`, projectID, keyHash))
	if err == nil {
		if stored.RevisionID != request.RevisionID || stored.OperationID != request.OperationID || stored.Version-1 != request.ExpectedVersion || stored.BaseRevisionID != request.ExpectedRevisionID {
			return state.RevisionSelection{}, false, state.ErrConflict
		}
		head, err := scanRevisionSelection(tx.QueryRowContext(ctx, currentRevisionQuery, request.SpaceID, projectID))
		if err != nil {
			return state.RevisionSelection{}, false, fmt.Errorf("read current revision: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return state.RevisionSelection{}, false, fmt.Errorf("confirm revision admission: %w", err)
		}
		return head, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state.RevisionSelection{}, false, fmt.Errorf("read revision admission receipt: %w", err)
	}
	var revisionID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM revisions WHERE project_id = ? AND id = ?`, projectID, request.RevisionID).Scan(&revisionID)
	if errors.Is(err, sql.ErrNoRows) {
		return state.RevisionSelection{}, false, state.ErrNotFound
	}
	if err != nil {
		return state.RevisionSelection{}, false, fmt.Errorf("read admission snapshot: %w", err)
	}
	var status state.OperationStatus
	err = tx.QueryRowContext(ctx, `SELECT status FROM operations WHERE scope = ? AND id = ?`, projectID, request.OperationID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return state.RevisionSelection{}, false, state.ErrNotFound
	}
	if err != nil {
		return state.RevisionSelection{}, false, fmt.Errorf("read admission operation: %w", err)
	}
	if status != state.OperationRunning {
		return state.RevisionSelection{}, false, state.ErrConflict
	}
	head, err := scanRevisionSelection(tx.QueryRowContext(ctx, currentRevisionQuery, request.SpaceID, projectID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state.RevisionSelection{}, false, fmt.Errorf("read revision base: %w", err)
	}
	if head.Version != request.ExpectedVersion || head.RevisionID != request.ExpectedRevisionID {
		return state.RevisionSelection{}, false, state.ErrConflict
	}
	selected, err := scanRevisionSelection(tx.QueryRowContext(ctx, `INSERT INTO revision_selections (project_id, version, revision_id, base_revision_id, operation_id, key_hash)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING RETURNING project_id, revision_id, operation_id, version, base_revision_id`,
		projectID, request.ExpectedVersion+1, revisionID, request.ExpectedRevisionID, request.OperationID, keyHash))
	if errors.Is(err, sql.ErrNoRows) {
		return state.RevisionSelection{}, false, state.ErrConflict
	}
	if err != nil {
		return state.RevisionSelection{}, false, fmt.Errorf("record revision selection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return state.RevisionSelection{}, false, fmt.Errorf("commit revision admission: %w", err)
	}
	return selected, true, nil
}

// CurrentRevision reads the latest admitted desired selection, not observed
// rollout state. A project without admissions returns ErrNotFound.
func (s *Store) CurrentRevision(ctx context.Context, spaceID, projectID string) (state.RevisionSelection, error) {
	head, err := scanRevisionSelection(s.db.QueryRowContext(ctx, currentRevisionQuery, spaceID, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return state.RevisionSelection{}, state.ErrNotFound
	}
	if err != nil {
		return state.RevisionSelection{}, fmt.Errorf("read current revision: %w", err)
	}
	return head, nil
}

const currentRevisionQuery = `SELECT s.project_id, s.revision_id, s.operation_id, s.version, s.base_revision_id
	FROM revision_selections s JOIN projects p ON p.id = s.project_id
	WHERE p.space_id = ? AND s.project_id = ? ORDER BY s.version DESC LIMIT 1`

func scanRevisionSelection(row *sql.Row) (state.RevisionSelection, error) {
	var selection state.RevisionSelection
	err := row.Scan(&selection.ProjectID, &selection.RevisionID, &selection.OperationID, &selection.Version, &selection.BaseRevisionID)
	return selection, err
}
