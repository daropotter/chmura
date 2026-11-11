package sqlite_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

func progressUpdate(op state.Operation, key string, status state.OperationStatus, result string) state.OperationUpdate {
	return state.OperationUpdate{Scope: op.Scope, OperationID: op.ID, IdempotencyKey: key, ExpectedVersion: op.Version, Status: status, Result: []byte(result)}
}

func TestOperationProgressAndTerminalReplaySurviveReopen(t *testing.T) {
	for _, terminal := range []state.OperationStatus{state.OperationSucceeded, state.OperationFailed, state.OperationCanceled} {
		t.Run(string(terminal), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "state.db")
			store := openStore(t, path)
			op, _, err := store.Accept(ctx, submission())
			if err != nil || op.Status != state.OperationPending || op.Version != 1 || len(op.Result) != 0 {
				t.Fatalf("new progress: %#v %v", op, err)
			}
			start := progressUpdate(op, "start", state.OperationRunning, "")
			running, changed, err := store.Update(ctx, start)
			if err != nil || !changed || running.Version != 2 || running.Status != state.OperationRunning {
				t.Fatalf("start: %#v %v %v", running, changed, err)
			}
			finish := progressUpdate(running, "finish", terminal, `{"outcome":"recorded"}`)
			final, changed, err := store.Update(ctx, finish)
			if err != nil || !changed || final.Version != 3 || final.Status != terminal || !bytes.Equal(final.Result, finish.Result) || !bytes.Equal(final.Intent, submission().Intent) {
				t.Fatalf("finish: %#v %v %v", final, changed, err)
			}
			finish.Result[0], final.Result[0] = '!', '!'
			finish.Result = []byte(`{"outcome":"recorded"}`)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openStore(t, path)
			for _, update := range []state.OperationUpdate{start, finish} {
				current, changed, err := store.Update(ctx, update)
				if err != nil || changed || current.Version != 3 || current.Status != terminal || !bytes.Equal(current.Result, finish.Result) {
					t.Fatalf("historical replay revived older state: %#v %v %v", current, changed, err)
				}
			}
			replayed, created, err := store.Accept(ctx, submission())
			if err != nil || created || replayed.ID != op.ID || replayed.Status != terminal || replayed.Version != 3 || !bytes.Equal(replayed.Result, finish.Result) {
				t.Fatalf("acceptance replay lost completion: %#v %v %v", replayed, created, err)
			}
			stored, err := store.Get(ctx, op.Scope, op.ID)
			if err != nil || stored.Status != terminal || !bytes.Equal(stored.Result, finish.Result) {
				t.Fatalf("terminal read: %#v %v", stored, err)
			}
		})
	}
}

func TestOperationUpdateRejectsStaleKeysAndTerminalChanges(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	op, _, err := store.Accept(ctx, submission())
	if err != nil {
		t.Fatal(err)
	}
	start := progressUpdate(op, "start", state.OperationRunning, "")
	running, _, err := store.Update(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"stale version", "reused key", "repeat phase"} {
		t.Run(mode, func(t *testing.T) {
			update := progressUpdate(running, "finish", state.OperationSucceeded, `{}`)
			switch mode {
			case "stale version":
				update.ExpectedVersion = op.Version
			case "reused key":
				update.IdempotencyKey = start.IdempotencyKey
			case "repeat phase":
				update.Status, update.Result = state.OperationRunning, nil
			}
			if got, changed, err := store.Update(ctx, update); !errors.Is(err, state.ErrConflict) || changed || got.ID != "" {
				t.Fatalf("conflicting update: %#v %v %v", got, changed, err)
			}
		})
	}
	// Rejected requests must not reserve the key or alter the current version.
	finish := progressUpdate(running, "finish", state.OperationSucceeded, `{}`)
	final, _, err := store.Update(ctx, finish)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"change result under same key", "new terminal write", "resume", "historical key rebound"} {
		t.Run(mode, func(t *testing.T) {
			update := progressUpdate(final, "other", state.OperationFailed, `{"error":"other"}`)
			switch mode {
			case "change result under same key":
				update = finish
				update.Result = []byte(`{"different":true}`)
			case "resume":
				update.Status, update.Result = state.OperationRunning, nil
			case "historical key rebound":
				update.IdempotencyKey = start.IdempotencyKey
			}
			if _, changed, err := store.Update(ctx, update); !errors.Is(err, state.ErrConflict) || changed {
				t.Fatalf("terminal receipt changed: %v %v", changed, err)
			}
		})
	}
}

func TestOperationCanFailOrCancelBeforeRunning(t *testing.T) {
	for _, status := range []state.OperationStatus{state.OperationFailed, state.OperationCanceled} {
		t.Run(string(status), func(t *testing.T) {
			store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
			op, _, err := store.Accept(context.Background(), submission())
			if err != nil {
				t.Fatal(err)
			}
			got, changed, err := store.Update(context.Background(), progressUpdate(op, "preflight", status, `{"phase":"preflight"}`))
			if err != nil || !changed || got.Status != status || got.Version != 2 {
				t.Fatalf("pre-execution termination: %#v %v %v", got, changed, err)
			}
		})
	}
}

func TestOperationUpdateValidatesInputAndScope(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	op, _, err := store.Accept(context.Background(), submission())
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"scope", "id", "key", "version", "status", "running result", "missing terminal result", "pending", "early success", "foreign scope", "missing operation", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			update := progressUpdate(op, "start", state.OperationRunning, "")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "scope":
				update.Scope = " "
			case "id":
				update.OperationID = ""
			case "key":
				update.IdempotencyKey = " "
			case "version":
				update.ExpectedVersion = 0
			case "status":
				update.Status = "unknown"
			case "running result":
				update.Result = []byte(`{}`)
			case "missing terminal result":
				update.Status = state.OperationFailed
			case "pending":
				update.Status = state.OperationPending
			case "early success":
				update.Status, update.Result = state.OperationSucceeded, []byte(`{}`)
			case "foreign scope":
				update.Scope = "other"
			case "missing operation":
				update.OperationID = "missing"
			case "canceled":
				cancel()
			}
			got, changed, err := store.Update(ctx, update)
			if err == nil || changed || got.ID != "" {
				t.Fatalf("invalid update: %#v %v %v", got, changed, err)
			}
			if mode == "canceled" && !errors.Is(err, context.Canceled) || (mode == "foreign scope" || mode == "missing operation") && !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("update lost cause: %v", err)
			}
		})
	}
	if got, changed, err := store.Update(context.Background(), progressUpdate(op, "start", state.OperationRunning, "")); err != nil || !changed || got.Version != 2 {
		t.Fatalf("rejected update mutated state or reserved key: %#v %v %v", got, changed, err)
	}
}

func TestOperationProgressMigratesVersionOneWithoutLosingAcceptance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(versionOneSchema)
	if err != nil {
		t.Fatal(err)
	}
	request := submission()
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	if _, err := db.Exec(`INSERT INTO operations VALUES (?, ?, ?, ?, ?)`, "op_legacy", request.Scope, hex.EncodeToString(key[:]), request.Kind, request.Intent); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, path)
	op, created, err := store.Accept(context.Background(), request)
	if err != nil || created || op.ID != "op_legacy" || op.Status != state.OperationPending || op.Version != 1 || !bytes.Equal(op.Intent, request.Intent) {
		t.Fatalf("migrated acceptance: %#v %v %v", op, created, err)
	}
	if _, _, err := store.Update(context.Background(), progressUpdate(op, "start", state.OperationRunning, "")); err != nil {
		t.Fatalf("migrated operation cannot advance: %v", err)
	}
}

// Frozen schema from #54: migration must retain the original acceptance.
const versionOneSchema = `CREATE TABLE operations (
	id TEXT NOT NULL PRIMARY KEY, scope TEXT NOT NULL, key_hash TEXT NOT NULL,
	kind TEXT NOT NULL, intent BLOB NOT NULL, UNIQUE(scope, key_hash)) STRICT;
	PRAGMA user_version = 1;`

func TestOperationMigrationFailureLeavesVersionOneUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(versionOneSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO operations VALUES ('legacy', 'scope', 'hash', 'restart', X'7B7D');
		CREATE TABLE operation_updates (unexpected TEXT);`); err != nil {
		t.Fatal(err)
	}
	if store, err := sqlite.Open(context.Background(), path); err == nil {
		_ = store.Close()
		t.Fatal("failed migration was acknowledged")
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("failed migration changed version: %d %v", version, err)
	}
	var intent []byte
	if err := db.QueryRow("SELECT intent FROM operations WHERE id = 'legacy'").Scan(&intent); err != nil || !bytes.Equal(intent, []byte(`{}`)) {
		t.Fatalf("failed migration lost acceptance: %s %v", intent, err)
	}
	var status string
	if err := db.QueryRow("SELECT status FROM operations LIMIT 1").Scan(&status); err == nil {
		t.Fatal("failed migration left a partially added column")
	}
}

func TestOperationProgressConcurrentKeysAdvanceOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	stores := []*sqlite.Store{openStore(t, path), openStore(t, path)}
	ctx := context.Background()
	op, _, err := stores[0].Accept(ctx, submission())
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		key     string
		op      state.Operation
		changed bool
		err     error
	}
	start, done := make(chan struct{}), make(chan outcome, 2)
	for i, key := range []string{"worker-a", "worker-b"} {
		go func() {
			<-start
			got, changed, err := stores[i].Update(ctx, progressUpdate(op, key, state.OperationRunning, ""))
			done <- outcome{key, got, changed, err}
		}()
	}
	close(start)
	var winner string
	for range 2 {
		got := <-done
		if got.err == nil {
			if !got.changed || got.op.Version != 2 || winner != "" {
				t.Fatalf("multiple workers advanced: %#v", got)
			}
			winner = got.key
		} else if !errors.Is(got.err, state.ErrConflict) || got.changed {
			t.Fatalf("competing progress lost conflict: %#v", got)
		}
	}
	if winner == "" {
		t.Fatal("no progress committed")
	}
	running, changed, err := stores[0].Update(ctx, progressUpdate(op, winner, state.OperationRunning, ""))
	if err != nil || changed || running.Version != 2 {
		t.Fatalf("winner replay: %#v %v %v", running, changed, err)
	}
	// Concurrent identical retries share the receipt; only one new write occurs.
	finish := progressUpdate(running, "finish", state.OperationSucceeded, `{}`)
	for _, store := range stores {
		go func() {
			got, changed, err := store.Update(ctx, finish)
			done <- outcome{"finish", got, changed, err}
		}()
	}
	changes := 0
	for range 2 {
		got := <-done
		if got.err != nil || got.op.Version != 3 || got.op.Status != state.OperationSucceeded {
			t.Fatalf("concurrent completion replay: %#v", got)
		}
		if got.changed {
			changes++
		}
	}
	if changes != 1 {
		t.Fatalf("completion changed %d times", changes)
	}
}

func TestOperationUpdateRollsBackProgressWhenReceiptCannotBeStored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	op, _, err := store.Accept(context.Background(), submission())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_receipt BEFORE INSERT ON operation_updates BEGIN SELECT RAISE(ABORT, 'receipt rejected'); END;`); err != nil {
		t.Fatal(err)
	}
	update := progressUpdate(op, "start", state.OperationRunning, "")
	if got, changed, err := store.Update(context.Background(), update); err == nil || changed || got.ID != "" {
		t.Fatalf("receipt storage failure acknowledged: %#v %v %v", got, changed, err)
	}
	current, err := store.Get(context.Background(), op.Scope, op.ID)
	if err != nil || current.Version != 1 || current.Status != state.OperationPending {
		t.Fatalf("progress escaped failed transaction: %#v %v", current, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_receipt"); err != nil {
		t.Fatal(err)
	}
	if got, changed, err := store.Update(context.Background(), update); err != nil || !changed || got.Version != 2 {
		t.Fatalf("failed transaction reserved update key: %#v %v %v", got, changed, err)
	}
}

func TestOperationUpdateCanceledWhileLockedRetainsStateAndKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	op, _, err := store.Accept(context.Background(), submission())
	if err != nil {
		t.Fatal(err)
	}
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
	update := progressUpdate(op, "start", state.OperationRunning, "")
	if got, changed, err := store.Update(ctx, update); !errors.Is(err, context.DeadlineExceeded) || changed || got.ID != "" {
		t.Fatalf("canceled update lost cause: %#v %v %v", got, changed, err)
	}
	if _, err := blocker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if got, changed, err := store.Update(context.Background(), update); err != nil || !changed || got.Version != 2 {
		t.Fatalf("canceled update changed state or reserved key: %#v %v %v", got, changed, err)
	}
}
