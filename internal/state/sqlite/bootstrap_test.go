package sqlite_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

func TestBootstrapAdminSerializesIndependentHandles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	stores := []*sqlite.Store{openStore(t, path), openStore(t, path)}
	requests := []state.BootstrapAdminRequest{
		{Name: "alice", IdempotencyKey: "bootstrap-a", TokenDigest: sha256.Sum256([]byte("fixture-a"))},
		{Name: "bob", IdempotencyKey: "bootstrap-b", TokenDigest: sha256.Sum256([]byte("fixture-b"))},
	}
	type outcome struct {
		id      int
		admin   state.Admin
		created bool
		err     error
	}
	start, done := make(chan struct{}), make(chan outcome, 2)
	for i, store := range stores {
		go func() {
			<-start
			admin, created, err := store.BootstrapAdmin(ctx, requests[i])
			done <- outcome{i, admin, created, err}
		}()
	}
	close(start)
	first, second := <-done, <-done
	if second.created {
		first, second = second, first
	}
	if first.err != nil || !first.created || first.admin.ID == "" || first.admin.Name != requests[first.id].Name ||
		second.created || second.admin != (state.Admin{}) || !errors.Is(second.err, state.ErrConflict) {
		t.Fatalf("concurrent bootstrap results: %+v %+v", first, second)
	}

	reopened := openStore(t, path)
	if admin, created, err := reopened.BootstrapAdmin(ctx, requests[first.id]); err != nil || created || admin != first.admin {
		t.Fatalf("bootstrap replay after reopen: %+v created=%v error=%v", admin, created, err)
	}
}

func TestBootstrapAdminUpgradesExistingState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	project := registeredProject(t, store, "production", "shop")
	revision, _, err := store.CreateRevision(ctx, revisionRequest(project))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Recreate the v10 layout, which is identical apart from the bootstrap table.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP TABLE bootstrap_admin; PRAGMA user_version = 10;"); err != nil {
		t.Fatal(err)
	}
	store = openStore(t, path)
	var count int
	if err := db.QueryRow("SELECT count(*) FROM bootstrap_admin").Scan(&count); err != nil || count != 0 {
		t.Fatalf("migration unexpectedly bootstrapped the installation: count=%d error=%v", count, err)
	}
	if _, created, err := store.BootstrapAdmin(ctx, state.BootstrapAdminRequest{
		Name: "alice", IdempotencyKey: "bootstrap", TokenDigest: sha256.Sum256([]byte("fixture")),
	}); err != nil || !created {
		t.Fatalf("bootstrap after upgrade: created=%v error=%v", created, err)
	}
	if got, err := store.GetRevision(ctx, project.SpaceID, project.ID, revision.ID); err != nil || got.ID != revision.ID {
		t.Fatalf("upgrade lost the existing revision: %+v error=%v", got, err)
	}
}
