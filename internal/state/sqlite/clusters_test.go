package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

func clusterRequest() state.CreateClusterRequest {
	return state.CreateClusterRequest{Name: "home", IdempotencyKey: "opaque cluster creation / key?do-not-persist"}
}

func TestClusterCreationSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	if got, err := store.GetClusterByName(ctx, "home"); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("opening state created a cluster: %#v %v", got, err)
	}
	request := clusterRequest()
	first, created, err := store.CreateCluster(ctx, request)
	if err != nil || !created || first.ID == "" || first.Name != request.Name {
		t.Fatalf("create cluster: %#v %v %v", first, created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || bytes.Contains(contents, []byte(request.IdempotencyKey)) {
		t.Fatal("database unreadable or raw cluster key persisted")
	}
	store = openStore(t, path)
	replay, created, err := store.CreateCluster(ctx, request)
	if err != nil || created || replay != first {
		t.Fatalf("reopen lost cluster receipt: %#v %v %v", replay, created, err)
	}
	byID, err := store.GetCluster(ctx, first.ID)
	if err != nil || byID != first {
		t.Fatalf("read cluster by ID: %#v %v", byID, err)
	}
	byName, err := store.GetClusterByName(ctx, request.Name)
	if err != nil || byName != first {
		t.Fatalf("read cluster by name: %#v %v", byName, err)
	}
	for _, name := range []string{"missing", "default"} {
		if got, err := store.GetClusterByName(ctx, name); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
			t.Fatalf("missing cluster lookup: %#v %v", got, err)
		}
	}
	if got, err := store.GetCluster(ctx, "missing"); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("missing cluster ID: %#v %v", got, err)
	}
	other, created, err := store.CreateCluster(ctx, state.CreateClusterRequest{Name: "work", IdempotencyKey: "second-cluster"})
	if err != nil || !created || other.ID == "" || other.ID == first.ID || other.Name != "work" {
		t.Fatalf("independent cluster: %#v %v %v", other, created, err)
	}
}

func TestClusterCreationRejectsNameAndKeyConflicts(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	request := clusterRequest()
	first, _, err := store.CreateCluster(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []state.CreateClusterRequest{
		{Name: request.Name, IdempotencyKey: "different-key"},
		{Name: "different-name", IdempotencyKey: request.IdempotencyKey},
	} {
		if got, created, err := store.CreateCluster(ctx, changed); !errors.Is(err, state.ErrConflict) || created || got.ID != "" {
			t.Fatalf("conflicting cluster creation: %#v %v %v", got, created, err)
		}
	}
	if got, created, err := store.CreateCluster(ctx, request); err != nil || created || got != first {
		t.Fatalf("conflict changed original receipt: %#v %v %v", got, created, err)
	}
	// Neither conflict reserves the other request's otherwise unused name/key.
	other, created, err := store.CreateCluster(ctx, state.CreateClusterRequest{Name: "different-name", IdempotencyKey: "different-key"})
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("conflict reserved unused name/key: %#v %v %v", other, created, err)
	}
}

func TestClusterCreationSerializesIndependentHandles(t *testing.T) {
	for _, mode := range []string{"same request", "same name", "same key"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			stores := []*sqlite.Store{openStore(t, path), openStore(t, path)}
			type outcome struct {
				cluster state.Cluster
				created bool
				err     error
			}
			const callers = 12
			start, done := make(chan struct{}), make(chan outcome, callers)
			for i := range callers {
				go func() {
					request := clusterRequest()
					if mode == "same name" {
						request.IdempotencyKey = fmt.Sprintf("key-%d", i)
					} else if mode == "same key" {
						request.Name = fmt.Sprintf("cluster-%d", i)
					}
					<-start
					cluster, created, err := stores[i%len(stores)].CreateCluster(context.Background(), request)
					done <- outcome{cluster, created, err}
				}()
			}
			close(start)
			var id string
			createdCount, conflicts := 0, 0
			for range callers {
				got := <-done
				if errors.Is(got.err, state.ErrConflict) && !got.created && got.cluster.ID == "" {
					conflicts++
					continue
				}
				if id == "" {
					id = got.cluster.ID
				}
				if got.err != nil || id == "" || got.cluster.ID != id {
					t.Errorf("concurrent cluster creation: %#v %v", got.cluster, got.err)
				}
				if got.created {
					createdCount++
				}
			}
			wantConflicts := callers - 1
			if mode == "same request" {
				wantConflicts = 0
			}
			if createdCount != 1 || conflicts != wantConflicts {
				t.Fatalf("concurrent creation: created=%d conflicts=%d want=%d", createdCount, conflicts, wantConflicts)
			}
		})
	}
}

func TestClusterCreationRejectsInvalidAndCanceledRequests(t *testing.T) {
	for _, mode := range []string{"empty name", "blank name", "empty key", "blank key", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
			request := clusterRequest()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "empty name":
				request.Name = ""
			case "blank name":
				request.Name = " \t"
			case "empty key":
				request.IdempotencyKey = ""
			case "blank key":
				request.IdempotencyKey = " \t"
			case "canceled":
				cancel()
			}
			got, created, err := store.CreateCluster(ctx, request)
			if err == nil || got.ID != "" || created || mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("invalid creation: %#v %v %v", got, created, err)
			}
			if got, created, err := store.CreateCluster(context.Background(), clusterRequest()); err != nil || !created || got.ID == "" {
				t.Fatalf("invalid creation reserved name/key: %#v %v %v", got, created, err)
			}
		})
	}
}

func TestClusterCreationStorageFailureDoesNotReserveNameOrKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_cluster BEFORE INSERT ON clusters BEGIN SELECT RAISE(ABORT, 'storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := clusterRequest()
	if got, created, err := store.CreateCluster(ctx, request); err == nil || errors.Is(err, state.ErrConflict) || created || got.ID != "" {
		t.Fatalf("failed write acknowledged: %#v %v %v", got, created, err)
	}
	if got, err := store.GetClusterByName(ctx, request.Name); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("failed write left cluster: %#v %v", got, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_cluster"); err != nil {
		t.Fatal(err)
	}
	if got, created, err := store.CreateCluster(ctx, request); err != nil || !created || got.ID == "" {
		t.Fatalf("failed write reserved name/key: %#v %v %v", got, created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got, created, err := store.CreateCluster(ctx, request); err == nil || created || got.ID != "" {
		t.Fatalf("closed storage acknowledged: %#v %v %v", got, created, err)
	}
	if got, err := store.GetClusterByName(ctx, request.Name); err == nil || errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("closed storage hid read error: %#v %v", got, err)
	}
}

func TestClusterMigrationPreservesHistoryAndRollsBackFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "rollback chain"}[fail], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(versionOneSchema + versionTwoProgressSchema + versionThreeSchema + versionFourSchema); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO operations VALUES ('op_legacy','scope','hash','deploy',X'7B7D','succeeded',3,X'7B7D');`); err != nil {
				t.Fatal(err)
			}
			if fail {
				if _, err := db.Exec("CREATE TABLE clusters (unexpected TEXT)"); err != nil {
					t.Fatal(err)
				}
				if store, err := sqlite.Open(context.Background(), path); err == nil {
					_ = store.Close()
					t.Fatal("failed cluster migration accepted")
				}
				var version, count int
				if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 4 {
					t.Fatalf("failed migration changed version: %d %v", version, err)
				}
				if err := db.QueryRow("SELECT count(*) FROM process_terminals").Scan(&count); err == nil {
					t.Fatal("failed migration chain left terminal table")
				}
				if err := db.QueryRow("SELECT count(*) FROM operations WHERE id='op_legacy'").Scan(&count); err != nil || count != 1 {
					t.Fatalf("failed migration lost history: %d %v", count, err)
				}
				return
			}
			if _, err := db.Exec(versionFiveTerminalSchema); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO generations VALUES ('scope','api/0','generation-a','op_legacy','generation-key',1,X'7B7D');
				INSERT INTO generation_processes VALUES ('scope','api/0','generation-a',X'72756E74696D65','identity-hash','2026-11-11T12:00:00Z',1);
				INSERT INTO process_terminals VALUES ('scope','api/0','generation-a','terminal-key','runtime-exit','2026-11-11T12:01:00Z',X'7B7D');`); err != nil {
				t.Fatal(err)
			}
			store := openStore(t, path)
			op, err := store.Get(context.Background(), "scope", "op_legacy")
			if err != nil || op.Status != state.OperationSucceeded || op.Version != 3 || !bytes.Equal(op.Result, []byte(`{}`)) {
				t.Fatalf("cluster migration lost operation: %#v %v", op, err)
			}
			process, err := store.GetProcess(context.Background(), "scope", "api/0", "generation-a")
			if err != nil || !process.EverReady || process.Terminal == nil || process.Terminal.Cause != state.ProcessExited || !bytes.Equal(process.Terminal.Evidence, []byte(`{}`)) {
				t.Fatalf("cluster migration lost terminal history: %#v %v", process, err)
			}
			var count int
			if err := db.QueryRow("SELECT count(*) FROM clusters").Scan(&count); err != nil || count != 0 {
				t.Fatalf("migration implicitly registered cluster: %d %v", count, err)
			}
			if got, created, err := store.CreateCluster(context.Background(), clusterRequest()); err != nil || !created || got.ID == "" {
				t.Fatalf("migrated cluster creation: %#v %v %v", got, created, err)
			}
		})
	}
}

// Frozen version-5 terminal table.
const versionFiveTerminalSchema = `CREATE TABLE process_terminals (
scope TEXT NOT NULL, slot_id TEXT NOT NULL, generation_id TEXT NOT NULL, key_hash TEXT NOT NULL,
cause TEXT NOT NULL CHECK (cause IN ('runtime-exit', 'startup-timeout', 'unhealthy')),
occurred_at TEXT NOT NULL, evidence BLOB NOT NULL CHECK (length(evidence) > 0),
PRIMARY KEY (scope, slot_id, generation_id),
FOREIGN KEY (scope, slot_id, generation_id) REFERENCES generation_processes(scope, slot_id, generation_id)) STRICT;
PRAGMA user_version = 5;`
