package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

func selectionOperation(t *testing.T, store *sqlite.Store, projectID, key, revisionID string) state.Operation {
	t.Helper()
	ctx := context.Background()
	op, _, err := store.Accept(ctx, state.Submission{Scope: projectID, IdempotencyKey: "operation-" + key, Kind: "deploy", Intent: []byte(fmt.Sprintf(`{"revision-id":%q}`, revisionID))})
	if err != nil {
		t.Fatal(err)
	}
	op, _, err = store.Update(ctx, progressUpdate(op, "start", state.OperationRunning, ""))
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func revisionAdmission(t *testing.T, store *sqlite.Store, project state.Project, key string) state.RevisionAdmission {
	t.Helper()
	request := revisionRequest(project)
	request.IdempotencyKey = "snapshot-" + key
	revision, _, err := store.CreateRevision(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	op := selectionOperation(t, store, project.ID, key, revision.ID)
	return state.RevisionAdmission{SpaceID: project.SpaceID, ProjectID: project.ID, RevisionID: revision.ID, OperationID: op.ID, IdempotencyKey: "admission-" + key}
}

func TestRevisionAdmissionSurvivesReopenAndHistoricalReplay(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	project := registeredProject(t, store, "production", "shop")
	first := revisionAdmission(t, store, project, "first")
	if got, err := store.CurrentRevision(ctx, project.SpaceID, project.ID); !errors.Is(err, state.ErrNotFound) || got.Version != 0 {
		t.Fatalf("snapshot implicitly selected a revision: %#v %v", got, err)
	}
	head, admitted, err := store.AdmitRevision(ctx, first)
	if err != nil || !admitted || head.ProjectID != project.ID || head.RevisionID != first.RevisionID || head.OperationID != first.OperationID || head.Version != 1 || head.BaseRevisionID != "" {
		t.Fatalf("first revision admission: %#v %v %v", head, admitted, err)
	}
	op, err := store.Get(ctx, project.ID, first.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, progressUpdate(op, "failed-rollout", state.OperationFailed, `{}`)); err != nil {
		t.Fatal(err)
	}
	second := revisionAdmission(t, store, project, "second")
	second.ExpectedVersion, second.ExpectedRevisionID = head.Version, head.RevisionID
	head, admitted, err = store.AdmitRevision(ctx, second)
	if err != nil || !admitted || head.Version != 2 || head.RevisionID != second.RevisionID || head.BaseRevisionID != first.RevisionID {
		t.Fatalf("successor admission: %#v %v %v", head, admitted, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || bytes.Contains(contents, []byte(first.IdempotencyKey)) || bytes.Contains(contents, []byte(second.IdempotencyKey)) {
		t.Fatal("database unreadable or raw admission key persisted")
	}
	store = openStore(t, path)
	for _, request := range []state.RevisionAdmission{first, second} {
		if got, admitted, err := store.AdmitRevision(ctx, request); err != nil || admitted || got != head {
			t.Fatalf("historical retry restored an old selection: %#v %v %v", got, admitted, err)
		}
	}
	if got, err := store.CurrentRevision(ctx, project.SpaceID, project.ID); err != nil || got != head {
		t.Fatalf("reopened selection: %#v %v", got, err)
	}
	if got, err := store.GetRevision(ctx, project.SpaceID, project.ID, first.RevisionID); err != nil || len(got.Manifest) == 0 {
		t.Fatalf("failed rollout lost revision: %#v %v", got, err)
	}
	if got, err := store.Get(ctx, project.ID, first.OperationID); err != nil || got.Status != state.OperationFailed {
		t.Fatalf("replay resumed terminal operation: %#v %v", got, err)
	}
	if got, err := store.Get(ctx, project.ID, second.OperationID); err != nil || got.Status != state.OperationRunning || got.Version != 2 {
		t.Fatalf("admission changed operation progress: %#v %v", got, err)
	}
}

func TestRevisionAdmissionChecksBaseIdentityAndVersion(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	project := registeredProject(t, store, "production", "shop")
	first := revisionAdmission(t, store, project, "first")
	head, _, err := store.AdmitRevision(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	second := revisionAdmission(t, store, project, "second")
	if got, admitted, err := store.AdmitRevision(ctx, second); !errors.Is(err, state.ErrConflict) || admitted || got != (state.RevisionSelection{}) {
		t.Fatalf("stale empty base advanced project: %#v %v %v", got, admitted, err)
	}
	second.ExpectedVersion, second.ExpectedRevisionID = head.Version, second.RevisionID
	if got, admitted, err := store.AdmitRevision(ctx, second); !errors.Is(err, state.ErrConflict) || admitted || got != (state.RevisionSelection{}) {
		t.Fatalf("wrong base identity advanced project: %#v %v %v", got, admitted, err)
	}
	second.ExpectedRevisionID = head.RevisionID
	head, admitted, err := store.AdmitRevision(ctx, second)
	if err != nil || !admitted || head.Version != 2 {
		t.Fatalf("base conflict reserved receipt: %#v %v %v", head, admitted, err)
	}
	// Selecting a retained declaration must still advance the project's version.
	op := selectionOperation(t, store, project.ID, "return-to-first", first.RevisionID)
	third := state.RevisionAdmission{SpaceID: project.SpaceID, ProjectID: project.ID, RevisionID: first.RevisionID, OperationID: op.ID,
		IdempotencyKey: "return-to-first", ExpectedVersion: head.Version, ExpectedRevisionID: head.RevisionID}
	head, admitted, err = store.AdmitRevision(ctx, third)
	if err != nil || !admitted || head.Version != 3 || head.RevisionID != first.RevisionID {
		t.Fatalf("retained selection: %#v %v %v", head, admitted, err)
	}
	late := second
	late.OperationID = selectionOperation(t, store, project.ID, "late", second.RevisionID).ID
	late.IdempotencyKey = "late"
	if got, admitted, err := store.AdmitRevision(ctx, late); !errors.Is(err, state.ErrConflict) || admitted || got != (state.RevisionSelection{}) {
		t.Fatalf("matching revision ID bypassed stale version: %#v %v %v", got, admitted, err)
	}
	late.ExpectedVersion, late.ExpectedRevisionID = head.Version, head.RevisionID
	if got, admitted, err := store.AdmitRevision(ctx, late); err != nil || !admitted || got.Version != 4 || got.RevisionID != second.RevisionID {
		t.Fatalf("stale rejection reserved key: %#v %v %v", got, admitted, err)
	}
}

func TestRevisionAdmissionBindsReceiptAndOneSelectionPerOperation(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	project := registeredProject(t, store, "production", "shop")
	first := revisionAdmission(t, store, project, "first")
	head, _, err := store.AdmitRevision(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	second := revisionAdmission(t, store, project, "second")
	second.ExpectedVersion, second.ExpectedRevisionID = head.Version, head.RevisionID
	head, _, err = store.AdmitRevision(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"revision", "operation", "base version", "base identity"} {
		t.Run(mode, func(t *testing.T) {
			changed := second
			switch mode {
			case "revision":
				changed.RevisionID = first.RevisionID
			case "operation":
				changed.OperationID = first.OperationID
			case "base version":
				changed.ExpectedVersion = 2
			case "base identity":
				changed.ExpectedRevisionID = second.RevisionID
			}
			if got, admitted, err := store.AdmitRevision(ctx, changed); !errors.Is(err, state.ErrConflict) || admitted || got != (state.RevisionSelection{}) {
				t.Fatalf("accepted receipt changed: %#v %v %v", got, admitted, err)
			}
		})
	}
	reused := second
	reused.IdempotencyKey, reused.RevisionID = "reuse-operation", first.RevisionID
	reused.ExpectedVersion, reused.ExpectedRevisionID = head.Version, head.RevisionID
	if got, admitted, err := store.AdmitRevision(ctx, reused); !errors.Is(err, state.ErrConflict) || admitted || got != (state.RevisionSelection{}) {
		t.Fatalf("one operation selected twice: %#v %v %v", got, admitted, err)
	}
	reused.OperationID = selectionOperation(t, store, project.ID, "new-operation", reused.RevisionID).ID
	if got, admitted, err := store.AdmitRevision(ctx, reused); err != nil || !admitted || got.Version != 3 {
		t.Fatalf("operation conflict reserved key: %#v %v %v", got, admitted, err)
	}
}

func TestRevisionAdmissionRequiresScopedRecordsAndRunningOperation(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	project := registeredProject(t, store, "production", "shop")
	other := registeredProject(t, store, "staging", "shop")
	request := revisionAdmission(t, store, project, "first")
	foreign := revisionAdmission(t, store, other, "foreign")
	sameSpace := revisionAdmission(t, store, registeredProject(t, store, "production", "billing"), "same-space")
	pending, _, err := store.Accept(ctx, state.Submission{Scope: project.ID, IdempotencyKey: "pending", Kind: "deploy", Intent: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	terminal := selectionOperation(t, store, project.ID, "terminal", request.RevisionID)
	if _, _, err := store.Update(ctx, progressUpdate(terminal, "fail", state.OperationFailed, `{}`)); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"missing project", "foreign space", "missing revision", "foreign revision", "same-space foreign revision", "missing operation", "foreign operation", "pending operation", "terminal operation"} {
		t.Run(mode, func(t *testing.T) {
			changed, want := request, state.ErrNotFound
			switch mode {
			case "missing project":
				changed.ProjectID = "missing"
			case "foreign space":
				changed.SpaceID = other.SpaceID
			case "missing revision":
				changed.RevisionID = "missing"
			case "foreign revision":
				changed.RevisionID = foreign.RevisionID
			case "same-space foreign revision":
				changed.RevisionID = sameSpace.RevisionID
			case "missing operation":
				changed.OperationID = "missing"
			case "foreign operation":
				changed.OperationID = foreign.OperationID
			case "pending operation":
				changed.OperationID, want = pending.ID, state.ErrConflict
			case "terminal operation":
				changed.OperationID, want = terminal.ID, state.ErrConflict
			}
			if got, admitted, err := store.AdmitRevision(ctx, changed); !errors.Is(err, want) || admitted || got != (state.RevisionSelection{}) {
				t.Fatalf("invalid admission: %#v %v %v", got, admitted, err)
			}
		})
	}
	if got, err := store.CurrentRevision(ctx, project.SpaceID, project.ID); !errors.Is(err, state.ErrNotFound) || got.Version != 0 {
		t.Fatalf("rejected request selected a revision: %#v %v", got, err)
	}
	if _, admitted, err := store.AdmitRevision(ctx, request); err != nil || !admitted {
		t.Fatalf("rejection reserved receipt: %v %v", admitted, err)
	}
	if got, err := store.CurrentRevision(ctx, other.SpaceID, project.ID); !errors.Is(err, state.ErrNotFound) || got.Version != 0 {
		t.Fatalf("foreign head read exposed selection: %#v %v", got, err)
	}
	foreign.IdempotencyKey = request.IdempotencyKey
	if got, admitted, err := store.AdmitRevision(ctx, foreign); err != nil || !admitted || got.ProjectID != other.ID || got.Version != 1 {
		t.Fatalf("another project could not reuse admission key: %#v %v %v", got, admitted, err)
	}
}

func TestRevisionAdmissionSerializesIndependentHandles(t *testing.T) {
	for _, mode := range []string{"same request", "competing requests"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			stores := []*sqlite.Store{openStore(t, path), openStore(t, path)}
			project := registeredProject(t, stores[0], "production", "shop")
			const callers = 12
			requests := make([]state.RevisionAdmission, callers)
			for i := range callers {
				if i == 0 || mode == "competing requests" {
					requests[i] = revisionAdmission(t, stores[0], project, fmt.Sprintf("request-%d", i))
				} else {
					requests[i] = requests[0]
				}
			}
			type outcome struct {
				head     state.RevisionSelection
				admitted bool
				err      error
			}
			start, done := make(chan struct{}), make(chan outcome, callers)
			for i := range callers {
				go func() {
					<-start
					head, admitted, err := stores[i%len(stores)].AdmitRevision(context.Background(), requests[i])
					done <- outcome{head, admitted, err}
				}()
			}
			close(start)
			var selected state.RevisionSelection
			admissions, conflicts := 0, 0
			for range callers {
				got := <-done
				if errors.Is(got.err, state.ErrConflict) && !got.admitted && got.head == (state.RevisionSelection{}) {
					conflicts++
					continue
				}
				if selected.Version == 0 {
					selected = got.head
				}
				if got.err != nil || got.head.Version != 1 || got.head != selected {
					t.Errorf("concurrent selection: %#v %v", got.head, got.err)
				}
				if got.admitted {
					admissions++
				}
			}
			wantConflicts := 0
			if mode == "competing requests" {
				wantConflicts = callers - 1
			}
			if admissions != 1 || conflicts != wantConflicts {
				t.Fatalf("concurrent admission: admissions=%d conflicts=%d", admissions, conflicts)
			}
			if got, err := stores[0].CurrentRevision(context.Background(), project.SpaceID, project.ID); err != nil || got != selected {
				t.Fatalf("concurrent head lost: %#v %v", got, err)
			}
		})
	}
}

func TestRevisionAdmissionRejectsInvalidAndCanceledRequests(t *testing.T) {
	for _, mode := range []string{"space", "project", "revision", "operation", "key", "negative version", "overflow", "base without version", "version without base", "blank base", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
			project := registeredProject(t, store, "production", "shop")
			request := revisionAdmission(t, store, project, "first")
			changed := request
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "space":
				changed.SpaceID = " \t"
			case "project":
				changed.ProjectID = " \t"
			case "revision":
				changed.RevisionID = " \t"
			case "operation":
				changed.OperationID = " \t"
			case "key":
				changed.IdempotencyKey = " \t"
			case "negative version":
				changed.ExpectedVersion = -1
			case "overflow":
				changed.ExpectedVersion, changed.ExpectedRevisionID = math.MaxInt64, request.RevisionID
			case "base without version":
				changed.ExpectedRevisionID = request.RevisionID
			case "version without base":
				changed.ExpectedVersion = 1
			case "blank base":
				changed.ExpectedVersion, changed.ExpectedRevisionID = 1, " \t"
			case "canceled":
				cancel()
			}
			got, admitted, err := store.AdmitRevision(ctx, changed)
			if err == nil || admitted || got != (state.RevisionSelection{}) || mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("invalid admission: %#v %v %v", got, admitted, err)
			}
			if _, admitted, err := store.AdmitRevision(context.Background(), request); err != nil || !admitted {
				t.Fatalf("invalid request reserved receipt: %v %v", admitted, err)
			}
		})
	}
}

func TestRevisionAdmissionFailureAndLockedCancellationRetainHead(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	project := registeredProject(t, store, "production", "shop")
	request := revisionAdmission(t, store, project, "first")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_selection BEFORE INSERT ON revision_selections BEGIN SELECT RAISE(ABORT, 'storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	if got, admitted, err := store.AdmitRevision(ctx, request); err == nil || errors.Is(err, state.ErrConflict) || errors.Is(err, state.ErrNotFound) || admitted || got != (state.RevisionSelection{}) {
		t.Fatalf("failed write acknowledged/misclassified: %#v %v %v", got, admitted, err)
	}
	if got, err := store.CurrentRevision(ctx, project.SpaceID, project.ID); !errors.Is(err, state.ErrNotFound) || got.Version != 0 {
		t.Fatalf("failed write left a head: %#v %v", got, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_selection"); err != nil {
		t.Fatal(err)
	}
	head, admitted, err := store.AdmitRevision(ctx, request)
	if err != nil || !admitted || head.Version != 1 {
		t.Fatalf("failed write reserved receipt: %#v %v %v", head, admitted, err)
	}
	if _, err := db.Exec("DELETE FROM revisions WHERE id = ?", request.RevisionID); err == nil {
		t.Fatal("admission lost its snapshot relationship")
	}
	second := revisionAdmission(t, store, project, "second")
	second.ExpectedVersion, second.ExpectedRevisionID = head.Version, head.RevisionID
	if _, err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer db.Exec("ROLLBACK")
	limited, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if got, admitted, err := store.AdmitRevision(limited, second); !errors.Is(err, context.DeadlineExceeded) || admitted || got != (state.RevisionSelection{}) {
		t.Fatalf("locked cancellation: %#v %v %v", got, admitted, err)
	}
	if _, err := db.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.CurrentRevision(ctx, project.SpaceID, project.ID); err != nil || got != head {
		t.Fatalf("canceled write changed head: %#v %v", got, err)
	}
	if got, admitted, err := store.AdmitRevision(ctx, second); err != nil || !admitted || got.Version != 2 {
		t.Fatalf("canceled write reserved receipt: %#v %v %v", got, admitted, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got, admitted, err := store.AdmitRevision(ctx, second); err == nil || admitted || got != (state.RevisionSelection{}) {
		t.Fatalf("closed store acknowledged admission: %#v %v %v", got, admitted, err)
	}
	if got, err := store.CurrentRevision(ctx, project.SpaceID, project.ID); err == nil || errors.Is(err, state.ErrNotFound) || got.Version != 0 {
		t.Fatalf("closed store hid read error: %#v %v", got, err)
	}
}

func TestRevisionAdmissionMigrationRetainsSnapshotsAndRollsBackChain(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "rollback chain"}[fail], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(versionOneSchema + versionTwoProgressSchema + versionThreeSchema + versionFourSchema + versionFiveTerminalSchema + versionSixClusterSchema + versionSevenSpaceSchema + versionEightProjectSchema); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO clusters VALUES ('cl_legacy','home','cluster-hash');
				INSERT INTO spaces VALUES ('sp_legacy','production','cl_legacy','space-hash');
				INSERT INTO projects VALUES ('prj_legacy','shop','sp_legacy','project-hash');
				INSERT INTO operations VALUES ('op_legacy','prj_legacy','hash','deploy',X'7B7D','running',2,X'');`); err != nil {
				t.Fatal(err)
			}
			if fail {
				if _, err := db.Exec("CREATE TABLE revision_selections (unexpected TEXT)"); err != nil {
					t.Fatal(err)
				}
				if store, err := sqlite.Open(context.Background(), path); err == nil {
					_ = store.Close()
					t.Fatal("failed admission migration accepted")
				}
				var version, count int
				if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 8 {
					t.Fatalf("failed migration changed version: %d %v", version, err)
				}
				if err := db.QueryRow("SELECT count(*) FROM revisions").Scan(&count); err == nil {
					t.Fatal("failed migration chain left snapshot table")
				}
				if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='revisions_project_identity'").Scan(&count); err != nil || count != 0 {
					t.Fatalf("failed migration left snapshot index: %d %v", count, err)
				}
				if err := db.QueryRow("SELECT count(*) FROM projects WHERE id='prj_legacy'").Scan(&count); err != nil || count != 1 {
					t.Fatalf("failed migration lost project: %d %v", count, err)
				}
				return
			}
			if _, err := db.Exec(versionNineRevisionSchema + `INSERT INTO revisions VALUES ('rev_legacy','prj_legacy','revision-hash',X'7B7D');`); err != nil {
				t.Fatal(err)
			}
			store := openStore(t, path)
			ctx := context.Background()
			if got, err := store.GetRevision(ctx, "sp_legacy", "prj_legacy", "rev_legacy"); err != nil || !bytes.Equal(got.Manifest, []byte(`{}`)) {
				t.Fatalf("migration lost snapshot: %#v %v", got, err)
			}
			if got, err := store.Get(ctx, "prj_legacy", "op_legacy"); err != nil || got.Status != state.OperationRunning || got.Version != 2 {
				t.Fatalf("migration lost operation: %#v %v", got, err)
			}
			if got, err := store.CurrentRevision(ctx, "sp_legacy", "prj_legacy"); !errors.Is(err, state.ErrNotFound) || got.Version != 0 {
				t.Fatalf("migration inferred a selection: %#v %v", got, err)
			}
			request := state.RevisionAdmission{SpaceID: "sp_legacy", ProjectID: "prj_legacy", RevisionID: "rev_legacy", OperationID: "op_legacy", IdempotencyKey: "admit-legacy"}
			if got, admitted, err := store.AdmitRevision(ctx, request); err != nil || !admitted || got.Version != 1 || got.RevisionID != request.RevisionID {
				t.Fatalf("migrated admission: %#v %v %v", got, admitted, err)
			}
		})
	}
}

// Frozen version-9 snapshot table from #76.
const versionNineRevisionSchema = `CREATE TABLE revisions (
id TEXT NOT NULL PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id), key_hash TEXT NOT NULL,
manifest BLOB NOT NULL CHECK (length(manifest) > 0), UNIQUE (project_id, key_hash)) STRICT;
PRAGMA user_version = 9;`
