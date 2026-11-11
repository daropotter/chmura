// Package sqlite implements the single-mode control-plane state database.
package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// Store owns one database handle. Independent handles share database uniqueness
// constraints; the connection limit is not a distributed ownership mechanism.
type Store struct{ db *sql.DB }

// Open explicitly opens/creates a database at an absolute disk path. It creates
// no parent directories or domain objects. The caller owns its location, file
// permissions and backups. No configuration or kubeconfig is loaded implicitly.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("state database requires an absolute disk path")
	}
	u := url.URL{Scheme: "file", Path: filepath.Clean(path)}
	q := url.Values{"_pragma": {"busy_timeout(5000)", "synchronous(FULL)", "foreign_keys(1)"}, "_txlock": {"immediate"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := initialize(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize state database: %w", err)
	}
	return &Store{db: db}, nil
}

func initialize(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	switch version {
	case 0:
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return err
		}
	case 1:
	default:
		return fmt.Errorf("unsupported state schema version %d", version)
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }
