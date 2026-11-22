package sqlite

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/daropotter/chmura/internal/state"
)

// AcceptDeploy commits a manifest, pending operation and desired selection
// against the requested base in one transaction. An exact retry returns the
// original acceptance with created=false, even after the head has moved. A
// canceled or lost response can follow commit, so retry the exact request.
func (s *Store) AcceptDeploy(ctx context.Context, request state.DeployRequest) (state.DeployAcceptance, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.DeployAcceptance{}, false, err
	}
	if strings.TrimSpace(request.SpaceID) == "" || strings.TrimSpace(request.ProjectID) == "" ||
		strings.TrimSpace(request.IdempotencyKey) == "" || len(request.Manifest) == 0 ||
		!validRevisionBase(request.ExpectedVersion, request.ExpectedRevisionID) {
		return state.DeployAcceptance{}, false, errors.New("deploy requires space, project, key, manifest and a valid base version/revision pair")
	}
	accepted, created, err := s.acceptDeploy(ctx, request)
	if err != nil && ctx.Err() != nil {
		return state.DeployAcceptance{}, false, ctx.Err()
	}
	return accepted, created, err
}

func (s *Store) acceptDeploy(ctx context.Context, request state.DeployRequest) (state.DeployAcceptance, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return state.DeployAcceptance{}, false, fmt.Errorf("begin deploy acceptance: %w", err)
	}
	defer tx.Rollback()
	if _, err := projectResult(tx.QueryRowContext(ctx, `SELECT id, name, space_id FROM projects WHERE space_id = ? AND id = ?`, request.SpaceID, request.ProjectID)); err != nil {
		return state.DeployAcceptance{}, false, err
	}
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	keyHash := hex.EncodeToString(key[:])
	op, err := scanOperation(tx.QueryRowContext(ctx, operationByKeyQuery, request.ProjectID, keyHash))
	var accepted state.DeployAcceptance
	var created bool
	switch {
	case errors.Is(err, sql.ErrNoRows):
		accepted, err = createDeploy(ctx, tx, request, keyHash)
		created = true
	case err == nil:
		accepted, err = replayDeploy(ctx, tx, request, op)
	default:
		err = fmt.Errorf("read deploy operation: %w", err)
	}
	if err != nil {
		return state.DeployAcceptance{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return state.DeployAcceptance{}, false, fmt.Errorf("commit deploy acceptance: %w", err)
	}
	return accepted, created, nil
}

func createDeploy(ctx context.Context, tx *sql.Tx, request state.DeployRequest, keyHash string) (state.DeployAcceptance, error) {
	head, err := scanRevisionSelection(tx.QueryRowContext(ctx, currentRevisionQuery, request.SpaceID, request.ProjectID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state.DeployAcceptance{}, fmt.Errorf("read deploy base: %w", err)
	}
	if head.Version != request.ExpectedVersion || head.RevisionID != request.ExpectedRevisionID {
		return state.DeployAcceptance{}, state.ErrConflict
	}
	operationID, revisionID := "op_"+rand.Text(), "rev_"+rand.Text()
	receiptKey := deployRecordKey(operationID)
	intent, err := json.Marshal(struct {
		RevisionID string `json:"revision-id"`
	}{RevisionID: revisionID})
	if err != nil {
		return state.DeployAcceptance{}, fmt.Errorf("encode deploy intent: %w", err)
	}
	op, err := scanOperation(tx.QueryRowContext(ctx, `INSERT INTO operations (id, scope, key_hash, kind, intent)
		VALUES (?, ?, ?, 'deploy', ?) RETURNING `+operationColumns, operationID, request.ProjectID, keyHash, intent))
	if err != nil {
		return state.DeployAcceptance{}, fmt.Errorf("record deploy operation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO revisions (id, project_id, key_hash, manifest) VALUES (?, ?, ?, ?)`,
		revisionID, request.ProjectID, receiptKey, request.Manifest); err != nil {
		return state.DeployAcceptance{}, fmt.Errorf("record deploy revision: %w", err)
	}
	selection, err := insertRevisionSelection(ctx, tx, state.RevisionSelection{
		ProjectID: request.ProjectID, RevisionID: revisionID, OperationID: op.ID,
		Version: request.ExpectedVersion + 1, BaseRevisionID: request.ExpectedRevisionID,
	}, receiptKey)
	if err != nil {
		return state.DeployAcceptance{}, err
	}
	return state.DeployAcceptance{Operation: op, Selection: selection}, nil
}

func replayDeploy(ctx context.Context, tx *sql.Tx, request state.DeployRequest, op state.Operation) (state.DeployAcceptance, error) {
	if op.Kind != "deploy" {
		return state.DeployAcceptance{}, state.ErrConflict
	}
	receiptKey := deployRecordKey(op.ID)
	var selection state.RevisionSelection
	var manifest []byte
	err := tx.QueryRowContext(ctx, `SELECT s.project_id, s.revision_id, s.operation_id, s.version, s.base_revision_id, r.manifest
		FROM revision_selections s JOIN revisions r ON r.project_id = s.project_id AND r.id = s.revision_id
		WHERE s.project_id = ? AND s.operation_id = ? AND s.key_hash = ? AND r.key_hash = ?`,
		op.Scope, op.ID, receiptKey, receiptKey).
		Scan(&selection.ProjectID, &selection.RevisionID, &selection.OperationID, &selection.Version, &selection.BaseRevisionID, &manifest)
	if errors.Is(err, sql.ErrNoRows) {
		return state.DeployAcceptance{}, state.ErrConflict
	}
	if err != nil {
		return state.DeployAcceptance{}, fmt.Errorf("read deploy acceptance: %w", err)
	}
	if selection.Version-1 != request.ExpectedVersion || selection.BaseRevisionID != request.ExpectedRevisionID || !bytes.Equal(manifest, request.Manifest) {
		return state.DeployAcceptance{}, state.ErrConflict
	}
	return state.DeployAcceptance{Operation: op, Selection: selection}, nil
}

// The operation owns the client key. The prefix distinguishes the linked
// receipts from the bare hexadecimal hashes used by standalone writes.
func deployRecordKey(operationID string) string {
	return "deploy:" + operationID
}
