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

// CreateCluster explicitly registers an empty cluster. Matching retries recover
// its stable identity with false; a different name/key binding conflicts. Keys
// are scoped to cluster creation within this installation, not to a caller.
// A canceled/lost response may follow commit: retry the exact request to recover.
func (s *Store) CreateCluster(ctx context.Context, request state.CreateClusterRequest) (state.Cluster, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.Cluster{}, false, err
	}
	if strings.TrimSpace(request.Name) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return state.Cluster{}, false, errors.New("cluster creation requires an explicit name and idempotency key")
	}
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	keyHash := hex.EncodeToString(key[:])
	cluster, err := scanCluster(s.db.QueryRowContext(ctx, `INSERT INTO clusters (id, name, key_hash)
		VALUES (?, ?, ?) ON CONFLICT DO NOTHING RETURNING id, name`, "cl_"+rand.Text(), request.Name, keyHash))
	if err == nil {
		return cluster, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state.Cluster{}, false, fmt.Errorf("create cluster: %w", err)
	}
	cluster, err = scanCluster(s.db.QueryRowContext(ctx, `SELECT id, name FROM clusters WHERE key_hash = ?`, keyHash))
	if errors.Is(err, sql.ErrNoRows) {
		return state.Cluster{}, false, state.ErrConflict
	}
	if err != nil {
		return state.Cluster{}, false, fmt.Errorf("read cluster creation: %w", err)
	}
	if cluster.Name != request.Name {
		return state.Cluster{}, false, state.ErrConflict
	}
	return cluster, false, nil
}

// GetCluster reads an installation cluster by its stable ID. The caller must
// already be authorized to read installation inventory.
func (s *Store) GetCluster(ctx context.Context, id string) (state.Cluster, error) {
	return clusterResult(s.db.QueryRowContext(ctx, `SELECT id, name FROM clusters WHERE id = ?`, id))
}

// GetClusterByName resolves an installation-wide name without creating it.
func (s *Store) GetClusterByName(ctx context.Context, name string) (state.Cluster, error) {
	return clusterResult(s.db.QueryRowContext(ctx, `SELECT id, name FROM clusters WHERE name = ?`, name))
}

func scanCluster(row *sql.Row) (state.Cluster, error) {
	var cluster state.Cluster
	err := row.Scan(&cluster.ID, &cluster.Name)
	return cluster, err
}

func clusterResult(row *sql.Row) (state.Cluster, error) {
	cluster, err := scanCluster(row)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Cluster{}, state.ErrNotFound
	}
	if err != nil {
		return state.Cluster{}, fmt.Errorf("read cluster: %w", err)
	}
	return cluster, nil
}
