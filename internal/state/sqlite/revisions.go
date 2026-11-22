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

// CreateRevision persists immutable manifest bytes without activating them.
// Keys are scoped to the project; exact retries recover the same snapshot with
// false, while changed bytes conflict. A new key creates a new revision even
// for identical content. A lost/canceled response may follow commit; retry exactly.
func (s *Store) CreateRevision(ctx context.Context, request state.CreateRevisionRequest) (state.Revision, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.Revision{}, false, err
	}
	if strings.TrimSpace(request.SpaceID) == "" || strings.TrimSpace(request.ProjectID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" || len(request.Manifest) == 0 {
		return state.Revision{}, false, errors.New("revision creation requires an explicit space, project, idempotency key and canonical manifest")
	}
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	keyHash := hex.EncodeToString(key[:])
	revision, err := scanRevision(s.db.QueryRowContext(ctx, `INSERT INTO revisions (id, project_id, key_hash, manifest)
		SELECT ?, id, ?, ? FROM projects WHERE space_id = ? AND id = ?
		ON CONFLICT DO NOTHING RETURNING id, project_id, manifest`, "rev_"+rand.Text(), keyHash, request.Manifest, request.SpaceID, request.ProjectID))
	if err == nil {
		return revision, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state.Revision{}, false, fmt.Errorf("create revision: %w", err)
	}
	revision, err = scanRevision(s.db.QueryRowContext(ctx, `SELECT r.id, r.project_id, r.manifest
		FROM revisions r JOIN projects p ON p.id = r.project_id
		WHERE p.space_id = ? AND r.project_id = ? AND r.key_hash = ?`, request.SpaceID, request.ProjectID, keyHash))
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.GetProject(ctx, request.SpaceID, request.ProjectID); err != nil {
			return state.Revision{}, false, fmt.Errorf("read revision project: %w", err)
		}
		return state.Revision{}, false, state.ErrConflict
	}
	if err != nil {
		return state.Revision{}, false, fmt.Errorf("read revision creation: %w", err)
	}
	if !bytes.Equal(revision.Manifest, request.Manifest) {
		return state.Revision{}, false, state.ErrConflict
	}
	return revision, false, nil
}

// GetRevision reads a snapshot only in the already authorized space/project.
func (s *Store) GetRevision(ctx context.Context, spaceID, projectID, id string) (state.Revision, error) {
	revision, err := scanRevision(s.db.QueryRowContext(ctx, `SELECT r.id, r.project_id, r.manifest
		FROM revisions r JOIN projects p ON p.id = r.project_id
		WHERE p.space_id = ? AND r.project_id = ? AND r.id = ?`, spaceID, projectID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return state.Revision{}, state.ErrNotFound
	}
	if err != nil {
		return state.Revision{}, fmt.Errorf("read revision: %w", err)
	}
	return revision, nil
}

func scanRevision(row *sql.Row) (state.Revision, error) {
	var revision state.Revision
	err := row.Scan(&revision.ID, &revision.ProjectID, &revision.Manifest)
	return revision, err
}
