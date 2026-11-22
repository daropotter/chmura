package sqlite

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/daropotter/chmura/internal/state"
)

// BootstrapAdmin creates the first administrator once per installation. Exact
// retries return its identity, including after a lost reply or database reopen.
func (s *Store) BootstrapAdmin(ctx context.Context, request state.BootstrapAdminRequest) (state.Admin, bool, error) {
	if err := ctx.Err(); err != nil {
		return state.Admin{}, false, err
	}
	if strings.TrimSpace(request.Name) == "" || strings.TrimSpace(request.IdempotencyKey) == "" || request.TokenDigest == ([32]byte{}) {
		return state.Admin{}, false, errors.New("bootstrap requires an administrator name, idempotency key and token digest")
	}
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	var admin state.Admin
	err := s.db.QueryRowContext(ctx, `INSERT INTO bootstrap_admin (singleton, id, name, key_hash, token_hash)
		VALUES (1, ?, ?, ?, ?) ON CONFLICT (singleton) DO NOTHING RETURNING id, name`,
		"admin_"+rand.Text(), request.Name, key[:], request.TokenDigest[:]).Scan(&admin.ID, &admin.Name)
	if err == nil {
		return admin, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return state.Admin{}, false, fmt.Errorf("create first administrator: %w", err)
	}
	var storedKey, storedDigest []byte
	if err := s.db.QueryRowContext(ctx, `SELECT id, name, key_hash, token_hash FROM bootstrap_admin WHERE singleton = 1`).
		Scan(&admin.ID, &admin.Name, &storedKey, &storedDigest); err != nil {
		return state.Admin{}, false, fmt.Errorf("read bootstrap receipt: %w", err)
	}
	if admin.Name != request.Name || subtle.ConstantTimeCompare(storedKey, key[:]) != 1 ||
		subtle.ConstantTimeCompare(storedDigest, request.TokenDigest[:]) != 1 {
		return state.Admin{}, false, state.ErrConflict
	}
	return admin, false, nil
}
