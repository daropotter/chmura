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

// CreateSpace creates an empty space bound to an existing cluster. Matching
// retries return the same record with false; changed name/cluster/key bindings
// conflict. Keys are scoped to space creation within the installation, shared
// by all callers. A canceled/lost response may follow commit: retry exactly.
func (s *Store) CreateSpace(ctx context.Context, request state.CreateSpaceRequest) (state.Space, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.Space{}, false, err
	}
	if strings.TrimSpace(request.Name) == "" || strings.TrimSpace(request.ClusterID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return state.Space{}, false, errors.New("space creation requires an explicit name, cluster ID and idempotency key")
	}
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	keyHash := hex.EncodeToString(key[:])
	// Selecting the parent and inserting are one statement; a missing cluster
	// never leaves a space or reserves a key. The foreign key prevents orphans.
	space, err := scanSpace(s.db.QueryRowContext(ctx, `INSERT INTO spaces (id, name, cluster_id, key_hash)
		SELECT ?, ?, id, ? FROM clusters WHERE id = ?
		ON CONFLICT DO NOTHING RETURNING id, name, cluster_id`, "sp_"+rand.Text(), request.Name, keyHash, request.ClusterID))
	if err == nil {
		return space, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state.Space{}, false, fmt.Errorf("create space: %w", err)
	}
	space, err = scanSpace(s.db.QueryRowContext(ctx, `SELECT id, name, cluster_id FROM spaces WHERE key_hash = ?`, keyHash))
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.GetCluster(ctx, request.ClusterID); err != nil {
			return state.Space{}, false, fmt.Errorf("read space cluster: %w", err)
		}
		return state.Space{}, false, state.ErrConflict
	}
	if err != nil {
		return state.Space{}, false, fmt.Errorf("read space creation: %w", err)
	}
	if space.Name != request.Name || space.ClusterID != request.ClusterID {
		return state.Space{}, false, state.ErrConflict
	}
	return space, false, nil
}

// GetSpace reads a space by its stable ID. The caller authorizes access.
func (s *Store) GetSpace(ctx context.Context, id string) (state.Space, error) {
	return spaceResult(s.db.QueryRowContext(ctx, `SELECT id, name, cluster_id FROM spaces WHERE id = ?`, id))
}

// GetSpaceByName resolves an installation-wide name without creating it. The
// caller must already be authorized to read that space.
func (s *Store) GetSpaceByName(ctx context.Context, name string) (state.Space, error) {
	return spaceResult(s.db.QueryRowContext(ctx, `SELECT id, name, cluster_id FROM spaces WHERE name = ?`, name))
}

func scanSpace(row *sql.Row) (state.Space, error) {
	var space state.Space
	err := row.Scan(&space.ID, &space.Name, &space.ClusterID)
	return space, err
}

func spaceResult(row *sql.Row) (state.Space, error) {
	space, err := scanSpace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Space{}, state.ErrNotFound
	}
	if err != nil {
		return state.Space{}, fmt.Errorf("read space: %w", err)
	}
	return space, nil
}
