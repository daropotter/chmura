package sqlite

import (
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

// CreateProject registers an identity within an existing space. Matching retries
// return the same record with false; a changed name/key binding conflicts.
// Creation keys are scoped to the space, shared by all authorized callers.
// A canceled/lost response may follow commit: retry the exact request to recover.
func (s *Store) CreateProject(ctx context.Context, request state.CreateProjectRequest) (state.Project, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.Project{}, false, err
	}
	if strings.TrimSpace(request.Name) == "" || strings.TrimSpace(request.SpaceID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return state.Project{}, false, errors.New("project creation requires an explicit name, space ID and idempotency key")
	}
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	keyHash := hex.EncodeToString(key[:])
	// Parent selection and insertion are atomic; no missing space is created
	// and the foreign key prevents orphan project identities.
	project, err := scanProject(s.db.QueryRowContext(ctx, `INSERT INTO projects (id, name, space_id, key_hash)
		SELECT ?, ?, id, ? FROM spaces WHERE id = ?
		ON CONFLICT DO NOTHING RETURNING id, name, space_id`, "prj_"+rand.Text(), request.Name, keyHash, request.SpaceID))
	if err == nil {
		return project, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state.Project{}, false, fmt.Errorf("create project: %w", err)
	}
	project, err = scanProject(s.db.QueryRowContext(ctx, `SELECT id, name, space_id FROM projects WHERE space_id = ? AND key_hash = ?`, request.SpaceID, keyHash))
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.GetSpace(ctx, request.SpaceID); err != nil {
			return state.Project{}, false, fmt.Errorf("read project space: %w", err)
		}
		return state.Project{}, false, state.ErrConflict
	}
	if err != nil {
		return state.Project{}, false, fmt.Errorf("read project creation: %w", err)
	}
	if project.Name != request.Name {
		return state.Project{}, false, state.ErrConflict
	}
	return project, false, nil
}

// GetProject reads a stable project ID only in the already authorized space.
func (s *Store) GetProject(ctx context.Context, spaceID, id string) (state.Project, error) {
	return projectResult(s.db.QueryRowContext(ctx, `SELECT id, name, space_id FROM projects WHERE space_id = ? AND id = ?`, spaceID, id))
}

// GetProjectByName resolves a recognition key in the already authorized space
// without creating a project. Another space's names and keys are independent.
func (s *Store) GetProjectByName(ctx context.Context, spaceID, name string) (state.Project, error) {
	return projectResult(s.db.QueryRowContext(ctx, `SELECT id, name, space_id FROM projects WHERE space_id = ? AND name = ?`, spaceID, name))
}

func scanProject(row *sql.Row) (state.Project, error) {
	var project state.Project
	err := row.Scan(&project.ID, &project.Name, &project.SpaceID)
	return project, err
}

func projectResult(row *sql.Row) (state.Project, error) {
	project, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Project{}, state.ErrNotFound
	}
	if err != nil {
		return state.Project{}, fmt.Errorf("read project: %w", err)
	}
	return project, nil
}
