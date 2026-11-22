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

func registeredProject(t *testing.T, store *sqlite.Store, spaceName, name string) state.Project {
	t.Helper()
	space := registeredSpace(t, store, spaceName)
	project, _, err := store.CreateProject(context.Background(), state.CreateProjectRequest{Name: name, SpaceID: space.ID, IdempotencyKey: "register-" + name})
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func revisionRequest(project state.Project) state.CreateRevisionRequest {
	return state.CreateRevisionRequest{SpaceID: project.SpaceID, ProjectID: project.ID, IdempotencyKey: "opaque revision creation / key?do-not-persist",
		Manifest: []byte(fmt.Sprintf(`{"version":1,"name":%q,"applications":{"api":{"source":{"image":"example.com/app:v1"}}}}`, project.Name))}
}

func TestRevisionSnapshotsSurviveReopenAndRetainIndependentBytes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	project := registeredProject(t, store, "production", "shop")
	request := revisionRequest(project)
	first, created, err := store.CreateRevision(ctx, request)
	if err != nil || !created || first.ID == "" || first.ProjectID != project.ID || !bytes.Equal(first.Manifest, request.Manifest) {
		t.Fatalf("create revision: %#v %v %v", first, created, err)
	}
	request.Manifest[0], first.Manifest[0] = '!', '!'
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || bytes.Contains(contents, []byte(request.IdempotencyKey)) {
		t.Fatal("database unreadable or raw revision key persisted")
	}
	store = openStore(t, path)
	request = revisionRequest(project)
	replay, created, err := store.CreateRevision(ctx, request)
	if err != nil || created || replay.ID != first.ID || !bytes.Equal(replay.Manifest, request.Manifest) {
		t.Fatalf("reopen lost immutable revision receipt: %#v %v %v", replay, created, err)
	}
	got, err := store.GetRevision(ctx, project.SpaceID, project.ID, first.ID)
	if err != nil || got.ID != first.ID || got.ProjectID != project.ID || !bytes.Equal(got.Manifest, request.Manifest) {
		t.Fatalf("read revision: %#v %v", got, err)
	}
	got.Manifest[0] = '!'
	if got, err := store.GetRevision(ctx, project.SpaceID, project.ID, first.ID); err != nil || !bytes.Equal(got.Manifest, request.Manifest) {
		t.Fatalf("returned bytes changed stored revision: %#v %v", got, err)
	}
	changed := revisionRequest(project)
	changed.IdempotencyKey = "second-revision"
	changed.Manifest = bytes.ReplaceAll(changed.Manifest, []byte(":v1"), []byte(":v2"))
	second, created, err := store.CreateRevision(ctx, changed)
	if err != nil || !created || second.ID == "" || second.ID == first.ID || !bytes.Equal(second.Manifest, changed.Manifest) {
		t.Fatalf("second revision: %#v %v %v", second, created, err)
	}
	if got, created, err := store.CreateRevision(ctx, request); err != nil || created || got.ID != first.ID || !bytes.Equal(got.Manifest, request.Manifest) {
		t.Fatalf("later revision changed historical receipt: %#v %v %v", got, created, err)
	}
	// Each new logical request can create a revision even when content is unchanged.
	request.IdempotencyKey = "third-revision-same-content"
	third, created, err := store.CreateRevision(ctx, request)
	if err != nil || !created || third.ID == "" || third.ID == first.ID || third.ID == second.ID || !bytes.Equal(third.Manifest, request.Manifest) {
		t.Fatalf("new request was content-deduplicated: %#v %v %v", third, created, err)
	}
}

func TestRevisionCreationAndReadsStayWithinSpaceAndProject(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	project := registeredProject(t, store, "production", "shop")
	otherProject := registeredProject(t, store, "production", "billing")
	otherSpace := registeredProject(t, store, "staging", "shop")
	first, _, err := store.CreateRevision(ctx, revisionRequest(project))
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range [][2]string{{project.SpaceID, otherProject.ID}, {otherSpace.SpaceID, project.ID}, {project.SpaceID, "missing"}, {"", project.ID}} {
		if got, err := store.GetRevision(ctx, scope[0], scope[1], first.ID); !errors.Is(err, state.ErrNotFound) || got.ID != "" || len(got.Manifest) != 0 {
			t.Fatalf("foreign revision lookup exposed bytes: %#v %v", got, err)
		}
	}
	if got, err := store.GetRevision(ctx, project.SpaceID, project.ID, "missing"); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("missing revision: %#v %v", got, err)
	}
	for _, foreign := range []state.Project{{ID: project.ID, SpaceID: otherSpace.SpaceID, Name: project.Name}, {ID: "missing-project", SpaceID: project.SpaceID, Name: project.Name}} {
		if got, created, err := store.CreateRevision(ctx, revisionRequest(foreign)); !errors.Is(err, state.ErrNotFound) || created || got.ID != "" || len(got.Manifest) != 0 {
			t.Fatalf("foreign/missing parent accepted: %#v %v %v", got, created, err)
		}
	}
	if got, err := store.GetProject(ctx, project.SpaceID, "missing-project"); !errors.Is(err, state.ErrNotFound) || got.ID != "" {
		t.Fatalf("revision creation fabricated project: %#v %v", got, err)
	}
	for _, independent := range []state.Project{otherProject, otherSpace} {
		request := revisionRequest(independent)
		got, created, err := store.CreateRevision(ctx, request)
		if err != nil || !created || got.ID == "" || got.ID == first.ID || got.ProjectID != independent.ID {
			t.Fatalf("another project reused revision: %#v %v %v", got, created, err)
		}
		if replay, created, err := store.CreateRevision(ctx, request); err != nil || created || replay.ID != got.ID || !bytes.Equal(replay.Manifest, request.Manifest) {
			t.Fatalf("independent receipt was lost: %#v %v %v", replay, created, err)
		}
	}
}

func TestRevisionCreationRejectsChangedManifestUnderAcceptedKey(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	project := registeredProject(t, store, "production", "shop")
	request := revisionRequest(project)
	first, _, err := store.CreateRevision(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	changed := revisionRequest(project)
	changed.Manifest = bytes.ReplaceAll(changed.Manifest, []byte(":v1"), []byte(":v2"))
	if got, created, err := store.CreateRevision(ctx, changed); !errors.Is(err, state.ErrConflict) || created || got.ID != "" || len(got.Manifest) != 0 {
		t.Fatalf("accepted key changed manifest: %#v %v %v", got, created, err)
	}
	if got, created, err := store.CreateRevision(ctx, request); err != nil || created || got.ID != first.ID || !bytes.Equal(got.Manifest, request.Manifest) {
		t.Fatalf("conflict changed original revision: %#v %v %v", got, created, err)
	}
	changed.IdempotencyKey = "new-request"
	if got, created, err := store.CreateRevision(ctx, changed); err != nil || !created || got.ID == first.ID || !bytes.Equal(got.Manifest, changed.Manifest) {
		t.Fatalf("conflict reserved a future request: %#v %v %v", got, created, err)
	}
}

func TestRevisionCreationSerializesIndependentHandles(t *testing.T) {
	for _, mode := range []string{"same request", "changed manifest", "new keys", "different projects"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			stores := []*sqlite.Store{openStore(t, path), openStore(t, path)}
			projects := []state.Project{registeredProject(t, stores[0], "production", "shop"), registeredProject(t, stores[0], "production", "billing")}
			type outcome struct {
				revision state.Revision
				project  string
				created  bool
				err      error
			}
			const callers = 12
			start, done := make(chan struct{}), make(chan outcome, callers)
			for i := range callers {
				go func() {
					project := projects[0]
					if mode == "different projects" {
						project = projects[i%len(projects)]
					}
					request := revisionRequest(project)
					if mode == "new keys" {
						request.IdempotencyKey = fmt.Sprintf("key-%d", i)
					} else if mode == "changed manifest" {
						request.Manifest = bytes.ReplaceAll(request.Manifest, []byte(":v1"), []byte(fmt.Sprintf(":v%d", i+2)))
					}
					<-start
					revision, created, err := stores[i%len(stores)].CreateRevision(context.Background(), request)
					done <- outcome{revision, project.ID, created, err}
				}()
			}
			close(start)
			ids, byProject := map[string]bool{}, map[string]string{}
			createdCount, conflicts := 0, 0
			for range callers {
				got := <-done
				if errors.Is(got.err, state.ErrConflict) && !got.created && got.revision.ID == "" && len(got.revision.Manifest) == 0 {
					conflicts++
					continue
				}
				if got.err != nil || got.revision.ID == "" || got.revision.ProjectID != got.project || len(got.revision.Manifest) == 0 {
					t.Errorf("concurrent revision creation: %#v %v", got.revision, got.err)
				}
				if previous, exists := byProject[got.project]; mode != "new keys" && exists && previous != got.revision.ID {
					t.Errorf("retry created distinct revisions: %q %q", previous, got.revision.ID)
				}
				byProject[got.project], ids[got.revision.ID] = got.revision.ID, true
				if got.created {
					createdCount++
				}
			}
			wantCreated, wantConflicts := 1, 0
			switch mode {
			case "changed manifest":
				wantConflicts = callers - 1
			case "new keys":
				wantCreated = callers
			case "different projects":
				wantCreated = len(projects)
			}
			if createdCount != wantCreated || len(ids) != wantCreated || conflicts != wantConflicts {
				t.Fatalf("concurrent creation: created=%d identities=%d conflicts=%d", createdCount, len(ids), conflicts)
			}
		})
	}
}

func TestRevisionCreationRejectsInvalidAndCanceledRequests(t *testing.T) {
	for _, mode := range []string{"empty space", "blank space", "empty project", "blank project", "empty key", "blank key", "empty manifest", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
			project := registeredProject(t, store, "production", "shop")
			request := revisionRequest(project)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "empty space":
				request.SpaceID = ""
			case "blank space":
				request.SpaceID = " \t"
			case "empty project":
				request.ProjectID = ""
			case "blank project":
				request.ProjectID = " \t"
			case "empty key":
				request.IdempotencyKey = ""
			case "blank key":
				request.IdempotencyKey = " \t"
			case "empty manifest":
				request.Manifest = nil
			case "canceled":
				cancel()
			}
			got, created, err := store.CreateRevision(ctx, request)
			if err == nil || got.ID != "" || len(got.Manifest) != 0 || created || mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("invalid revision creation: %#v %v %v", got, created, err)
			}
			if got, created, err := store.CreateRevision(context.Background(), revisionRequest(project)); err != nil || !created || got.ID == "" {
				t.Fatalf("invalid request reserved a key: %#v %v %v", got, created, err)
			}
		})
	}
}

func TestRevisionStorageErrorsAndProjectBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	project := registeredProject(t, store, "production", "shop")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_revision BEFORE INSERT ON revisions BEGIN SELECT RAISE(ABORT, 'storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := revisionRequest(project)
	if got, created, err := store.CreateRevision(ctx, request); err == nil || errors.Is(err, state.ErrConflict) || errors.Is(err, state.ErrNotFound) || created || got.ID != "" || len(got.Manifest) != 0 {
		t.Fatalf("failed revision write acknowledged or misclassified: %#v %v %v", got, created, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_revision"); err != nil {
		t.Fatal(err)
	}
	revision, created, err := store.CreateRevision(ctx, request)
	if err != nil || !created || revision.ID == "" {
		t.Fatalf("failed write reserved key: %#v %v %v", revision, created, err)
	}
	if _, err := db.Exec("DELETE FROM projects WHERE id = ?", project.ID); err == nil {
		t.Fatal("database allowed a revision's project to disappear")
	}
	if _, err := db.Exec(`INSERT INTO revisions (id, project_id, key_hash, manifest) VALUES ('orphan', 'missing', 'unused', X'7B7D')`); err == nil {
		t.Fatal("database accepted an orphan revision")
	}
	if got, err := store.GetRevision(ctx, project.SpaceID, project.ID, revision.ID); err != nil || got.ID != revision.ID || !bytes.Equal(got.Manifest, request.Manifest) {
		t.Fatalf("rejected deletion changed revision: %#v %v", got, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got, created, err := store.CreateRevision(ctx, request); err == nil || created || got.ID != "" || len(got.Manifest) != 0 {
		t.Fatalf("closed storage acknowledged revision: %#v %v %v", got, created, err)
	}
	if got, err := store.GetRevision(ctx, project.SpaceID, project.ID, revision.ID); err == nil || errors.Is(err, state.ErrNotFound) || got.ID != "" || len(got.Manifest) != 0 {
		t.Fatalf("closed storage hid read error: %#v %v", got, err)
	}
}

func TestRevisionMigrationPreservesProjectReceiptsAndRollsBackFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "rollback chain"}[fail], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(versionOneSchema + versionTwoProgressSchema + versionThreeSchema + versionFourSchema + versionFiveTerminalSchema + versionSixClusterSchema + versionSevenSpaceSchema); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO clusters VALUES ('cl_legacy','home','cluster-hash');
				INSERT INTO spaces VALUES ('sp_legacy','production','cl_legacy','space-hash');
				INSERT INTO operations VALUES ('op_legacy','scope','hash','deploy',X'7B7D','succeeded',3,X'7B7D');`); err != nil {
				t.Fatal(err)
			}
			if fail {
				if _, err := db.Exec("CREATE TABLE revisions (unexpected TEXT)"); err != nil {
					t.Fatal(err)
				}
				if store, err := sqlite.Open(context.Background(), path); err == nil {
					_ = store.Close()
					t.Fatal("failed revision migration accepted")
				}
				var version, count int
				if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 7 {
					t.Fatalf("failed migration changed version: %d %v", version, err)
				}
				if err := db.QueryRow("SELECT count(*) FROM projects").Scan(&count); err == nil {
					t.Fatal("failed migration chain left project table")
				}
				if err := db.QueryRow("SELECT count(*) FROM spaces WHERE id='sp_legacy'").Scan(&count); err != nil || count != 1 {
					t.Fatalf("failed migration lost space: %d %v", count, err)
				}
				return
			}
			if _, err := db.Exec(versionEightProjectSchema); err != nil {
				t.Fatal(err)
			}
			request := projectRequest("sp_legacy")
			key := sha256.Sum256([]byte(request.IdempotencyKey))
			if _, err := db.Exec("INSERT INTO projects VALUES ('prj_legacy', ?, ?, ?)", request.Name, request.SpaceID, hex.EncodeToString(key[:])); err != nil {
				t.Fatal(err)
			}
			store := openStore(t, path)
			ctx := context.Background()
			project, created, err := store.CreateProject(ctx, request)
			if err != nil || created || project.ID != "prj_legacy" || project.SpaceID != request.SpaceID {
				t.Fatalf("migration lost project receipt: %#v %v %v", project, created, err)
			}
			op, err := store.Get(ctx, "scope", "op_legacy")
			if err != nil || op.Status != state.OperationSucceeded || op.Version != 3 || !bytes.Equal(op.Result, []byte(`{}`)) {
				t.Fatalf("revision migration lost operation: %#v %v", op, err)
			}
			var count int
			if err := db.QueryRow("SELECT count(*) FROM revisions").Scan(&count); err != nil || count != 0 {
				t.Fatalf("migration fabricated manifest history: %d %v", count, err)
			}
			if got, created, err := store.CreateRevision(ctx, revisionRequest(project)); err != nil || !created || got.ProjectID != project.ID {
				t.Fatalf("migrated revision creation: %#v %v %v", got, created, err)
			}
		})
	}
}

// Frozen version-8 project table from #75.
const versionEightProjectSchema = `CREATE TABLE projects (
id TEXT NOT NULL PRIMARY KEY, name TEXT NOT NULL CHECK (length(name) > 0),
space_id TEXT NOT NULL REFERENCES spaces(id), key_hash TEXT NOT NULL,
UNIQUE (space_id, name), UNIQUE (space_id, key_hash)) STRICT;
PRAGMA user_version = 8;`
