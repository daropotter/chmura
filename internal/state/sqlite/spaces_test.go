package sqlite_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

func registeredCluster(t *testing.T, store *sqlite.Store, name string) state.Cluster {
	t.Helper()
	cluster, _, err := store.CreateCluster(context.Background(), state.CreateClusterRequest{Name: name, IdempotencyKey: "register-" + name})
	if err != nil {
		t.Fatal(err)
	}
	return cluster
}

func spaceRequest(clusterID string) state.CreateSpaceRequest {
	return state.CreateSpaceRequest{Name: "production", ClusterID: clusterID, IdempotencyKey: "opaque space creation / key?do-not-persist"}
}

func TestSpaceCreationSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	cluster := registeredCluster(t, store, "home")
	request := spaceRequest(cluster.ID)
	first, created, err := store.CreateSpace(ctx, request)
	if err != nil || !created || first.ID == "" || first.Name != request.Name || first.ClusterID != cluster.ID {
		t.Fatalf("create space: %#v %v %v", first, created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || bytes.Contains(contents, []byte(request.IdempotencyKey)) {
		t.Fatal("database unreadable or raw space key persisted")
	}
	store = openStore(t, path)
	if got, created, err := store.CreateSpace(ctx, request); err != nil || created || got != first {
		t.Fatalf("reopen lost space receipt: %#v %v %v", got, created, err)
	}
	if got, err := store.GetSpace(ctx, first.ID); err != nil || got != first {
		t.Fatalf("space by ID: %#v %v", got, err)
	}
	if got, err := store.GetSpaceByName(ctx, first.Name); err != nil || got != first {
		t.Fatalf("space by name: %#v %v", got, err)
	}
	for _, name := range []string{"missing", "default"} {
		if got, err := store.GetSpaceByName(ctx, name); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
			t.Fatalf("missing space lookup: %#v %v", got, err)
		}
	}
	if got, err := store.GetSpace(ctx, "missing"); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("missing space ID: %#v %v", got, err)
	}
	other, created, err := store.CreateSpace(ctx, state.CreateSpaceRequest{Name: "staging", ClusterID: cluster.ID, IdempotencyKey: "another-space"})
	if err != nil || !created || other.ID == "" || other.ID == first.ID || other.ClusterID != cluster.ID {
		t.Fatalf("second space in cluster: %#v %v %v", other, created, err)
	}
}

func TestSpaceCreationRequiresExistingCluster(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	request := spaceRequest("missing-cluster")
	if got, created, err := store.CreateSpace(ctx, request); !errors.Is(err, state.ErrNotFound) || created || got.ID != "" {
		t.Fatalf("missing cluster accepted: %#v %v %v", got, created, err)
	}
	if got, err := store.GetCluster(ctx, request.ClusterID); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("space creation fabricated cluster: %#v %v", got, err)
	}
	if got, err := store.GetSpaceByName(ctx, request.Name); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("missing parent left space: %#v %v", got, err)
	}
	request.ClusterID = registeredCluster(t, store, "home").ID
	if got, created, err := store.CreateSpace(ctx, request); err != nil || !created || got.ClusterID != request.ClusterID {
		t.Fatalf("missing parent reserved name/key: %#v %v %v", got, created, err)
	}
}

func TestSpaceCreationRejectsChangedReceiptsAndGlobalNameConflicts(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	home, work := registeredCluster(t, store, "home"), registeredCluster(t, store, "work")
	request := spaceRequest(home.ID)
	first, _, err := store.CreateSpace(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []state.CreateSpaceRequest{
		{Name: request.Name, ClusterID: home.ID, IdempotencyKey: "another-key"},
		{Name: request.Name, ClusterID: work.ID, IdempotencyKey: "another-key"},
		{Name: "staging", ClusterID: home.ID, IdempotencyKey: request.IdempotencyKey},
		{Name: request.Name, ClusterID: work.ID, IdempotencyKey: request.IdempotencyKey},
		{Name: request.Name, ClusterID: "missing-cluster", IdempotencyKey: request.IdempotencyKey},
	} {
		if got, created, err := store.CreateSpace(ctx, changed); !errors.Is(err, state.ErrConflict) || created || got.ID != "" {
			t.Fatalf("changed space request accepted: %#v %v %v", got, created, err)
		}
	}
	if got, err := store.GetSpace(ctx, first.ID); err != nil || got != first {
		t.Fatalf("conflict changed space binding: %#v %v", got, err)
	}
	if got, created, err := store.CreateSpace(ctx, request); err != nil || created || got != first {
		t.Fatalf("conflict changed receipt: %#v %v %v", got, created, err)
	}
	other, created, err := store.CreateSpace(ctx, state.CreateSpaceRequest{Name: "staging", ClusterID: work.ID, IdempotencyKey: "another-key"})
	if err != nil || !created || other.ID == first.ID || other.ClusterID != work.ID {
		t.Fatalf("conflict reserved unused name/key: %#v %v %v", other, created, err)
	}
}

func TestSpaceCreationSerializesIndependentHandles(t *testing.T) {
	for _, mode := range []string{"same request", "same name across clusters", "same key across clusters"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			stores := []*sqlite.Store{openStore(t, path), openStore(t, path)}
			clusters := []state.Cluster{registeredCluster(t, stores[0], "home"), registeredCluster(t, stores[0], "work")}
			type outcome struct {
				space   state.Space
				created bool
				err     error
			}
			const callers = 12
			start, done := make(chan struct{}), make(chan outcome, callers)
			for i := range callers {
				go func() {
					request := spaceRequest(clusters[0].ID)
					if mode == "same name across clusters" {
						request.ClusterID = clusters[i%len(clusters)].ID
						request.IdempotencyKey = fmt.Sprintf("key-%d", i)
					} else if mode == "same key across clusters" {
						request.ClusterID = clusters[i%len(clusters)].ID
						request.Name = fmt.Sprintf("space-%d", i)
					}
					<-start
					space, created, err := stores[i%len(stores)].CreateSpace(context.Background(), request)
					done <- outcome{space, created, err}
				}()
			}
			close(start)
			var first state.Space
			createdCount, conflicts := 0, 0
			for range callers {
				got := <-done
				if errors.Is(got.err, state.ErrConflict) && !got.created && got.space.ID == "" {
					conflicts++
					continue
				}
				if first.ID == "" {
					first = got.space
				}
				if got.err != nil || first.ID == "" || got.space != first {
					t.Errorf("concurrent space creation: %#v %v", got.space, got.err)
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

func TestSpaceCreationRejectsInvalidAndCanceledRequests(t *testing.T) {
	for _, mode := range []string{"empty name", "blank name", "empty cluster", "blank cluster", "empty key", "blank key", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
			cluster := registeredCluster(t, store, "home")
			request := spaceRequest(cluster.ID)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "empty name":
				request.Name = ""
			case "blank name":
				request.Name = " \t"
			case "empty cluster":
				request.ClusterID = ""
			case "blank cluster":
				request.ClusterID = " \t"
			case "empty key":
				request.IdempotencyKey = ""
			case "blank key":
				request.IdempotencyKey = " \t"
			case "canceled":
				cancel()
			}
			got, created, err := store.CreateSpace(ctx, request)
			if err == nil || got.ID != "" || created || mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("invalid space creation: %#v %v %v", got, created, err)
			}
			if got, created, err := store.CreateSpace(context.Background(), spaceRequest(cluster.ID)); err != nil || !created || got.ID == "" {
				t.Fatalf("invalid request reserved name/key: %#v %v %v", got, created, err)
			}
		})
	}
}

func TestSpaceCreationStorageFailureDoesNotReserveNameOrKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	cluster := registeredCluster(t, store, "home")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_space BEFORE INSERT ON spaces BEGIN SELECT RAISE(ABORT, 'storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := spaceRequest(cluster.ID)
	if got, created, err := store.CreateSpace(ctx, request); err == nil || errors.Is(err, state.ErrConflict) || errors.Is(err, state.ErrNotFound) || created || got.ID != "" {
		t.Fatalf("failed space write acknowledged or misclassified: %#v %v %v", got, created, err)
	}
	if got, err := store.GetSpaceByName(ctx, request.Name); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("failed write left space: %#v %v", got, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_space"); err != nil {
		t.Fatal(err)
	}
	if got, created, err := store.CreateSpace(ctx, request); err != nil || !created || got.ID == "" {
		t.Fatalf("failed write reserved name/key: %#v %v %v", got, created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got, created, err := store.CreateSpace(ctx, request); err == nil || created || got.ID != "" {
		t.Fatalf("closed storage acknowledged space: %#v %v %v", got, created, err)
	}
	if got, err := store.GetSpaceByName(ctx, request.Name); err == nil || errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("closed storage hid read error: %#v %v", got, err)
	}
}

func TestSpaceBindingIsEnforcedByDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	cluster := registeredCluster(t, store, "home")
	space, _, err := store.CreateSpace(context.Background(), spaceRequest(cluster.ID))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM clusters WHERE id = ?", cluster.ID); err == nil {
		t.Fatal("database allowed a space's cluster to disappear")
	}
	if _, err := db.Exec(`INSERT INTO spaces (id, name, cluster_id, key_hash) VALUES ('orphan', 'orphan', 'missing', 'unused')`); err == nil {
		t.Fatal("database accepted an orphan space")
	}
	if got, err := store.GetSpace(context.Background(), space.ID); err != nil || got != space {
		t.Fatalf("rejected deletion changed space: %#v %v", got, err)
	}
}

func TestSpaceMigrationPreservesClusterReceiptsAndRollsBackFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "rollback chain"}[fail], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(versionOneSchema + versionTwoProgressSchema + versionThreeSchema + versionFourSchema + versionFiveTerminalSchema); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO operations VALUES ('op_legacy','scope','hash','deploy',X'7B7D','succeeded',3,X'7B7D');`); err != nil {
				t.Fatal(err)
			}
			if fail {
				if _, err := db.Exec("CREATE TABLE spaces (unexpected TEXT)"); err != nil {
					t.Fatal(err)
				}
				if store, err := sqlite.Open(context.Background(), path); err == nil {
					_ = store.Close()
					t.Fatal("failed space migration accepted")
				}
				var version, count int
				if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 5 {
					t.Fatalf("failed migration changed version: %d %v", version, err)
				}
				if err := db.QueryRow("SELECT count(*) FROM clusters").Scan(&count); err == nil {
					t.Fatal("failed migration chain left cluster table")
				}
				if err := db.QueryRow("SELECT count(*) FROM operations WHERE id='op_legacy'").Scan(&count); err != nil || count != 1 {
					t.Fatalf("failed migration lost operation: %d %v", count, err)
				}
				return
			}
			if _, err := db.Exec(versionSixClusterSchema); err != nil {
				t.Fatal(err)
			}
			request := clusterRequest()
			key := sha256.Sum256([]byte(request.IdempotencyKey))
			if _, err := db.Exec("INSERT INTO clusters VALUES ('cl_legacy', ?, ?)", request.Name, hex.EncodeToString(key[:])); err != nil {
				t.Fatal(err)
			}
			store := openStore(t, path)
			ctx := context.Background()
			cluster, created, err := store.CreateCluster(ctx, request)
			if err != nil || created || cluster.ID != "cl_legacy" || cluster.Name != request.Name {
				t.Fatalf("migration lost cluster receipt: %#v %v %v", cluster, created, err)
			}
			op, err := store.Get(ctx, "scope", "op_legacy")
			if err != nil || op.Status != state.OperationSucceeded || op.Version != 3 || !bytes.Equal(op.Result, []byte(`{}`)) {
				t.Fatalf("space migration lost operation: %#v %v", op, err)
			}
			var count int
			if err := db.QueryRow("SELECT count(*) FROM spaces").Scan(&count); err != nil || count != 0 {
				t.Fatalf("migration implicitly created space: %d %v", count, err)
			}
			if got, created, err := store.CreateSpace(ctx, spaceRequest(cluster.ID)); err != nil || !created || got.ClusterID != cluster.ID {
				t.Fatalf("migrated space creation: %#v %v %v", got, created, err)
			}
		})
	}
}

// Frozen version-6 cluster table from #73.
const versionSixClusterSchema = `CREATE TABLE clusters (
id TEXT NOT NULL PRIMARY KEY, name TEXT NOT NULL UNIQUE CHECK (length(name) > 0), key_hash TEXT NOT NULL UNIQUE) STRICT;
PRAGMA user_version = 6;`
