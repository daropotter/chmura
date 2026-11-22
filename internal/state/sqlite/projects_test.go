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

func registeredSpace(t *testing.T, store *sqlite.Store, name string) state.Space {
	t.Helper()
	cluster := registeredCluster(t, store, "home")
	space, _, err := store.CreateSpace(context.Background(), state.CreateSpaceRequest{Name: name, ClusterID: cluster.ID, IdempotencyKey: "register-" + name})
	if err != nil {
		t.Fatal(err)
	}
	return space
}

func projectRequest(spaceID string) state.CreateProjectRequest {
	return state.CreateProjectRequest{Name: "shop", SpaceID: spaceID, IdempotencyKey: "opaque project creation / key?do-not-persist"}
}

func TestProjectCreationSurvivesReopenAndSeparatesSpaces(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	production, staging := registeredSpace(t, store, "production"), registeredSpace(t, store, "staging")
	request := projectRequest(production.ID)
	first, created, err := store.CreateProject(ctx, request)
	if err != nil || !created || first.ID == "" || first.Name != request.Name || first.SpaceID != production.ID {
		t.Fatalf("create project: %#v %v %v", first, created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || bytes.Contains(contents, []byte(request.IdempotencyKey)) {
		t.Fatal("database unreadable or raw project key persisted")
	}
	store = openStore(t, path)
	if got, created, err := store.CreateProject(ctx, request); err != nil || created || got != first {
		t.Fatalf("reopen lost project receipt: %#v %v %v", got, created, err)
	}
	if got, err := store.GetProject(ctx, production.ID, first.ID); err != nil || got != first {
		t.Fatalf("project by ID: %#v %v", got, err)
	}
	if got, err := store.GetProjectByName(ctx, production.ID, first.Name); err != nil || got != first {
		t.Fatalf("project by name: %#v %v", got, err)
	}
	for _, spaceID := range []string{staging.ID, "missing", ""} {
		if got, err := store.GetProject(ctx, spaceID, first.ID); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
			t.Fatalf("foreign ID lookup exposed project: %#v %v", got, err)
		}
		if got, err := store.GetProjectByName(ctx, spaceID, first.Name); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
			t.Fatalf("foreign name lookup exposed project: %#v %v", got, err)
		}
	}
	if got, err := store.GetProject(ctx, production.ID, "missing"); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("missing project ID: %#v %v", got, err)
	}
	if got, err := store.GetProjectByName(ctx, production.ID, "default"); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("opening state created a default project: %#v %v", got, err)
	}
	// The same name and creation key address a distinct project in another space.
	otherRequest := projectRequest(staging.ID)
	other, created, err := store.CreateProject(ctx, otherRequest)
	if err != nil || !created || other.ID == "" || other.ID == first.ID || other.Name != first.Name || other.SpaceID != staging.ID {
		t.Fatalf("independent space reused project: %#v %v %v", other, created, err)
	}
	if got, err := store.GetProjectByName(ctx, staging.ID, first.Name); err != nil || got != other {
		t.Fatalf("space/name resolved foreign project: %#v %v", got, err)
	}
	if got, created, err := store.CreateProject(ctx, otherRequest); err != nil || created || got != other {
		t.Fatalf("other-space receipt changed: %#v %v %v", got, created, err)
	}
	if got, created, err := store.CreateProject(ctx, request); err != nil || created || got != first {
		t.Fatalf("other-space creation changed original receipt: %#v %v %v", got, created, err)
	}
}

func TestProjectCreationRequiresExistingSpace(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	request := projectRequest("missing-space")
	if got, created, err := store.CreateProject(ctx, request); !errors.Is(err, state.ErrNotFound) || created || got.ID != "" {
		t.Fatalf("missing space accepted: %#v %v %v", got, created, err)
	}
	if got, err := store.GetSpace(ctx, request.SpaceID); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("project creation fabricated space: %#v %v", got, err)
	}
	if got, err := store.GetProjectByName(ctx, request.SpaceID, request.Name); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("missing parent left project: %#v %v", got, err)
	}
	request.SpaceID = registeredSpace(t, store, "production").ID
	if got, created, err := store.CreateProject(ctx, request); err != nil || !created || got.SpaceID != request.SpaceID {
		t.Fatalf("missing parent reserved name/key: %#v %v %v", got, created, err)
	}
	request.SpaceID = "another-missing-space"
	if got, created, err := store.CreateProject(ctx, request); !errors.Is(err, state.ErrNotFound) || created || got.ID != "" {
		t.Fatalf("receipt in another scope hid missing space: %#v %v %v", got, created, err)
	}
}

func TestProjectCreationRejectsNameAndKeyConflictsWithinSpace(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	space := registeredSpace(t, store, "production")
	request := projectRequest(space.ID)
	first, _, err := store.CreateProject(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []state.CreateProjectRequest{
		{Name: request.Name, SpaceID: space.ID, IdempotencyKey: "another-key"},
		{Name: "billing", SpaceID: space.ID, IdempotencyKey: request.IdempotencyKey},
	} {
		if got, created, err := store.CreateProject(ctx, changed); !errors.Is(err, state.ErrConflict) || created || got.ID != "" {
			t.Fatalf("conflicting project request accepted: %#v %v %v", got, created, err)
		}
	}
	if got, created, err := store.CreateProject(ctx, request); err != nil || created || got != first {
		t.Fatalf("conflict changed project receipt: %#v %v %v", got, created, err)
	}
	other, created, err := store.CreateProject(ctx, state.CreateProjectRequest{Name: "billing", SpaceID: space.ID, IdempotencyKey: "another-key"})
	if err != nil || !created || other.ID == first.ID || other.Name != "billing" || other.SpaceID != space.ID {
		t.Fatalf("conflict reserved unused name/key: %#v %v %v", other, created, err)
	}
}

func TestProjectCreationSerializesIndependentHandles(t *testing.T) {
	for _, mode := range []string{"same request", "same name", "same key", "same request in different spaces"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			stores := []*sqlite.Store{openStore(t, path), openStore(t, path)}
			spaces := []state.Space{registeredSpace(t, stores[0], "production"), registeredSpace(t, stores[0], "staging")}
			type outcome struct {
				project state.Project
				created bool
				err     error
			}
			const callers = 12
			start, done := make(chan struct{}), make(chan outcome, callers)
			for i := range callers {
				go func() {
					request := projectRequest(spaces[0].ID)
					switch mode {
					case "same name":
						request.IdempotencyKey = fmt.Sprintf("key-%d", i)
					case "same key":
						request.Name = fmt.Sprintf("project-%d", i)
					case "same request in different spaces":
						request.SpaceID = spaces[i%len(spaces)].ID
					}
					<-start
					project, created, err := stores[i%len(stores)].CreateProject(context.Background(), request)
					done <- outcome{project, created, err}
				}()
			}
			close(start)
			bySpace := map[string]state.Project{}
			createdCount, conflicts := 0, 0
			for range callers {
				got := <-done
				if errors.Is(got.err, state.ErrConflict) && !got.created && got.project.ID == "" {
					conflicts++
					continue
				}
				if got.err != nil || got.project.ID == "" || got.project.SpaceID != spaces[0].ID && got.project.SpaceID != spaces[1].ID {
					t.Errorf("concurrent project creation: %#v %v", got.project, got.err)
				}
				if previous, exists := bySpace[got.project.SpaceID]; exists && previous != got.project {
					t.Errorf("same-space callers returned distinct projects: %#v %#v", previous, got.project)
				}
				bySpace[got.project.SpaceID] = got.project
				if got.created {
					createdCount++
				}
			}
			wantCreated, wantConflicts := 1, 0
			if mode == "same name" || mode == "same key" {
				wantConflicts = callers - 1
			} else if mode == "same request in different spaces" {
				wantCreated = 2
				if bySpace[spaces[0].ID].ID == bySpace[spaces[1].ID].ID {
					t.Error("different spaces shared a project identity")
				}
			}
			if createdCount != wantCreated || len(bySpace) != wantCreated || conflicts != wantConflicts {
				t.Fatalf("concurrent creation: created=%d scopes=%d conflicts=%d", createdCount, len(bySpace), conflicts)
			}
		})
	}
}

func TestProjectCreationRejectsInvalidAndCanceledRequests(t *testing.T) {
	for _, mode := range []string{"empty name", "blank name", "empty space", "blank space", "empty key", "blank key", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
			space := registeredSpace(t, store, "production")
			request := projectRequest(space.ID)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "empty name":
				request.Name = ""
			case "blank name":
				request.Name = " \t"
			case "empty space":
				request.SpaceID = ""
			case "blank space":
				request.SpaceID = " \t"
			case "empty key":
				request.IdempotencyKey = ""
			case "blank key":
				request.IdempotencyKey = " \t"
			case "canceled":
				cancel()
			}
			got, created, err := store.CreateProject(ctx, request)
			if err == nil || got.ID != "" || created || mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("invalid project creation: %#v %v %v", got, created, err)
			}
			if got, created, err := store.CreateProject(context.Background(), projectRequest(space.ID)); err != nil || !created || got.ID == "" {
				t.Fatalf("invalid request reserved name/key: %#v %v %v", got, created, err)
			}
		})
	}
}

func TestProjectCreationStorageErrorsAndSpaceBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	space := registeredSpace(t, store, "production")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_project BEFORE INSERT ON projects BEGIN SELECT RAISE(ABORT, 'storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := projectRequest(space.ID)
	if got, created, err := store.CreateProject(ctx, request); err == nil || errors.Is(err, state.ErrConflict) || errors.Is(err, state.ErrNotFound) || created || got.ID != "" {
		t.Fatalf("failed project write acknowledged or misclassified: %#v %v %v", got, created, err)
	}
	if got, err := store.GetProjectByName(ctx, space.ID, request.Name); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("failed write left project: %#v %v", got, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_project"); err != nil {
		t.Fatal(err)
	}
	project, created, err := store.CreateProject(ctx, request)
	if err != nil || !created || project.ID == "" {
		t.Fatalf("failed write reserved name/key: %#v %v %v", project, created, err)
	}
	if _, err := db.Exec("DELETE FROM spaces WHERE id = ?", space.ID); err == nil {
		t.Fatal("database allowed a project's space to disappear")
	}
	if _, err := db.Exec(`INSERT INTO projects (id, name, space_id, key_hash) VALUES ('orphan', 'orphan', 'missing', 'unused')`); err == nil {
		t.Fatal("database accepted an orphan project")
	}
	if got, err := store.GetProject(ctx, space.ID, project.ID); err != nil || got != project {
		t.Fatalf("rejected deletion changed project: %#v %v", got, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got, created, err := store.CreateProject(ctx, request); err == nil || created || got.ID != "" {
		t.Fatalf("closed storage acknowledged project: %#v %v %v", got, created, err)
	}
	if got, err := store.GetProjectByName(ctx, space.ID, request.Name); err == nil || errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("closed storage hid read error: %#v %v", got, err)
	}
}

func TestProjectMigrationPreservesSpaceReceiptsAndRollsBackFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "rollback chain"}[fail], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(versionOneSchema + versionTwoProgressSchema + versionThreeSchema + versionFourSchema + versionFiveTerminalSchema + versionSixClusterSchema); err != nil {
				t.Fatal(err)
			}
			legacyCluster := clusterRequest()
			clusterKey := sha256.Sum256([]byte(legacyCluster.IdempotencyKey))
			if _, err := db.Exec("INSERT INTO clusters VALUES ('cl_legacy', ?, ?)", legacyCluster.Name, hex.EncodeToString(clusterKey[:])); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO operations VALUES ('op_legacy','scope','hash','deploy',X'7B7D','succeeded',3,X'7B7D');`); err != nil {
				t.Fatal(err)
			}
			if fail {
				if _, err := db.Exec("CREATE TABLE projects (unexpected TEXT)"); err != nil {
					t.Fatal(err)
				}
				if store, err := sqlite.Open(context.Background(), path); err == nil {
					_ = store.Close()
					t.Fatal("failed project migration accepted")
				}
				var version, count int
				if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 6 {
					t.Fatalf("failed migration changed version: %d %v", version, err)
				}
				if err := db.QueryRow("SELECT count(*) FROM spaces").Scan(&count); err == nil {
					t.Fatal("failed migration chain left space table")
				}
				if err := db.QueryRow("SELECT count(*) FROM clusters WHERE id='cl_legacy'").Scan(&count); err != nil || count != 1 {
					t.Fatalf("failed migration lost cluster: %d %v", count, err)
				}
				return
			}
			if _, err := db.Exec(versionSevenSpaceSchema); err != nil {
				t.Fatal(err)
			}
			request := spaceRequest("cl_legacy")
			key := sha256.Sum256([]byte(request.IdempotencyKey))
			if _, err := db.Exec("INSERT INTO spaces VALUES ('sp_legacy', ?, ?, ?)", request.Name, request.ClusterID, hex.EncodeToString(key[:])); err != nil {
				t.Fatal(err)
			}
			store := openStore(t, path)
			ctx := context.Background()
			space, created, err := store.CreateSpace(ctx, request)
			if err != nil || created || space.ID != "sp_legacy" || space.ClusterID != request.ClusterID {
				t.Fatalf("migration lost space receipt: %#v %v %v", space, created, err)
			}
			if got, created, err := store.CreateCluster(ctx, legacyCluster); err != nil || created || got.ID != request.ClusterID {
				t.Fatalf("migration lost cluster receipt: %#v %v %v", got, created, err)
			}
			op, err := store.Get(ctx, "scope", "op_legacy")
			if err != nil || op.Status != state.OperationSucceeded || op.Version != 3 || !bytes.Equal(op.Result, []byte(`{}`)) {
				t.Fatalf("project migration lost operation: %#v %v", op, err)
			}
			var count int
			if err := db.QueryRow("SELECT count(*) FROM projects").Scan(&count); err != nil || count != 0 {
				t.Fatalf("migration implicitly created project: %d %v", count, err)
			}
			if got, created, err := store.CreateProject(ctx, projectRequest(space.ID)); err != nil || !created || got.SpaceID != space.ID {
				t.Fatalf("migrated project creation: %#v %v %v", got, created, err)
			}
		})
	}
}

// Frozen version-7 space table from #74.
const versionSevenSpaceSchema = `CREATE TABLE spaces (
id TEXT NOT NULL PRIMARY KEY, name TEXT NOT NULL UNIQUE CHECK (length(name) > 0),
cluster_id TEXT NOT NULL REFERENCES clusters(id), key_hash TEXT NOT NULL UNIQUE) STRICT;
PRAGMA user_version = 7;`
