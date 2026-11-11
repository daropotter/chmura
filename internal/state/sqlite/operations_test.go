package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

func openStore(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func submission() state.Submission {
	return state.Submission{Scope: "project-id-a", IdempotencyKey: "opaque / key?do-not-log-this", Kind: "restart", Intent: []byte(`{"slot":"api-1","generation":"generation-a"}`)}
}

func TestAcceptOperationSurvivesReopenAndRetainsIntent(t *testing.T) {
	ctx := context.Background()
	// Characters meaningful in a SQLite URI must remain literal path data.
	path := filepath.Join(t.TempDir(), "state ?mode=memory#private.db")
	store := openStore(t, path)
	request := submission()
	first, created, err := store.Accept(ctx, request)
	if err != nil || !created || first.ID == "" || first.Scope != request.Scope || first.Kind != request.Kind || !bytes.Equal(first.Intent, request.Intent) {
		t.Fatalf("accept: operation=%#v created=%v error=%v", first, created, err)
	}
	request.Intent[0] = '!'
	first.Intent[0] = '!'
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStore(t, path)
	request = submission()
	replay, created, err := store.Accept(ctx, request)
	if err != nil || created || replay.ID != first.ID || !bytes.Equal(replay.Intent, request.Intent) {
		t.Fatalf("reopen lost receipt: operation=%#v created=%v error=%v", replay, created, err)
	}
	found, err := store.Get(ctx, request.Scope, first.ID)
	if err != nil || found.ID != first.ID || !bytes.Equal(found.Intent, request.Intent) {
		t.Fatalf("read after reopen: %#v %v", found, err)
	}
	if _, err := store.Get(ctx, "other-project", first.ID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("cross-scope lookup exposed operation: %v", err)
	}
	if _, err := store.Get(ctx, request.Scope, "missing"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("missing operation=%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || bytes.Contains(contents, []byte(request.IdempotencyKey)) {
		t.Fatal("raw request key was persisted or database was not created at the literal path")
	}
}

func TestAcceptOperationRejectsKeyReuseAndSeparatesScopes(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	request := submission()
	first, _, err := store.Accept(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"intent", "kind"} {
		t.Run(change, func(t *testing.T) {
			changed := submission()
			if change == "intent" {
				changed.Intent = []byte(`{"slot":"api-2"}`)
			} else {
				changed.Kind = "deploy"
			}
			operation, created, err := store.Accept(ctx, changed)
			if !errors.Is(err, state.ErrConflict) || created || operation.ID != "" {
				t.Fatalf("key reuse accepted: %#v %v %v", operation, created, err)
			}
		})
	}
	replay, created, err := store.Accept(ctx, request)
	if err != nil || created || replay.ID != first.ID || !bytes.Equal(replay.Intent, request.Intent) {
		t.Fatalf("conflict changed original receipt: %#v %v %v", replay, created, err)
	}
	request.Scope = "project-id-b"
	other, created, err := store.Accept(ctx, request)
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("independent scope reused operation: %#v %v %v", other, created, err)
	}
}

func TestAcceptConcurrentHandlesKeepOneOperation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	stores := []*sqlite.Store{openStore(t, path), openStore(t, path)}
	type outcome struct {
		operation state.Operation
		created   bool
		err       error
	}
	const callers = 16
	start, done := make(chan struct{}), make(chan outcome, callers)
	for i := range callers {
		go func() {
			<-start
			op, created, err := stores[i%len(stores)].Accept(context.Background(), submission())
			done <- outcome{op, created, err}
		}()
	}
	close(start)
	var id string
	createdCount := 0
	for range callers {
		got := <-done
		if id == "" {
			id = got.operation.ID
		}
		if got.err != nil || id == "" || got.operation.ID != id {
			t.Fatalf("concurrent acceptance: %#v %v", got.operation, got.err)
		}
		if got.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("accepted %d new operations", createdCount)
	}
}

func TestConcurrentOpenInitializesOneSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	const callers = 4
	start, done := make(chan struct{}), make(chan error, callers)
	for range callers {
		go func() {
			<-start
			store, err := sqlite.Open(context.Background(), path)
			if err == nil {
				defer store.Close()
				_, _, err = store.Accept(context.Background(), submission())
			}
			done <- err
		}()
	}
	close(start)
	for range callers {
		if err := <-done; err != nil {
			t.Fatalf("concurrent schema initialization: %v", err)
		}
	}
}

func TestAcceptRejectsInvalidOrCanceledInput(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	for _, mode := range []string{"scope", "key", "kind", "intent", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			request := submission()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "scope":
				request.Scope = " "
			case "key":
				request.IdempotencyKey = " "
			case "kind":
				request.Kind = " "
			case "intent":
				request.Intent = nil
			case "canceled":
				cancel()
			}
			op, created, err := store.Accept(ctx, request)
			if err == nil || created || op.ID != "" || mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("invalid acceptance: %#v %v %v", op, created, err)
			}
		})
	}
	if _, created, err := store.Accept(context.Background(), submission()); err != nil || !created {
		t.Fatalf("rejected input reserved the key: %v %v", created, err)
	}
}

func TestAcceptCanceledWhileWriteIsLockedDoesNotReserveKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	blocker, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	blocker.SetMaxOpenConns(1)
	if _, err := blocker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer blocker.Exec("ROLLBACK")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	op, created, err := store.Accept(ctx, submission())
	if !errors.Is(err, context.DeadlineExceeded) || created || op.ID != "" {
		t.Fatalf("canceled locked write: %#v %v %v", op, created, err)
	}
	if _, err := blocker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.Accept(context.Background(), submission()); err != nil || !created {
		t.Fatalf("canceled locked writer reserved key: %v %v", created, err)
	}
}

func TestOpenRequiresExplicitDiskPathAndPreservesFutureSchema(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{"", ":memory:", "file:state?mode=memory", filepath.Join(t.TempDir(), "missing", "state.db")} {
		if store, err := sqlite.Open(ctx, path); err == nil {
			_ = store.Close()
			t.Fatalf("accepted missing disk path or created parent: %q", path)
		}
	}
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if store, err := sqlite.Open(ctx, path); err == nil {
		_ = store.Close()
		t.Fatal("future schema was opened")
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("future schema was changed: %d %v", version, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	newPath := filepath.Join(t.TempDir(), "canceled.db")
	if store, err := sqlite.Open(canceled, newPath); !errors.Is(err, context.Canceled) {
		if store != nil {
			_ = store.Close()
		}
		t.Fatalf("canceled open=%v", err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("canceled open created a database: %v", err)
	}
}
