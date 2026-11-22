package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

func deployRequest(project state.Project, key string) state.DeployRequest {
	return state.DeployRequest{SpaceID: project.SpaceID, ProjectID: project.ID, IdempotencyKey: key, Manifest: revisionRequest(project).Manifest}
}

func TestAcceptDeployReplaysOriginalAfterNewerDeploy(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	project := registeredProject(t, store, "production", "shop")
	request := deployRequest(project, "first")
	first, created, err := store.AcceptDeploy(ctx, request)
	if err != nil || !created || first.Operation.ID == "" || first.Operation.Scope != project.ID ||
		first.Operation.Kind != "deploy" || first.Operation.Status != state.OperationPending || first.Operation.Version != 1 ||
		first.Selection.OperationID != first.Operation.ID || first.Selection.RevisionID == "" || first.Selection.Version != 1 {
		t.Fatalf("first deploy: %+v, created=%v, error=%v", first, created, err)
	}
	if got, err := store.GetRevision(ctx, project.SpaceID, project.ID, first.Selection.RevisionID); err != nil || !bytes.Equal(got.Manifest, request.Manifest) {
		t.Fatalf("snapshot: %+v, error=%v", got, err)
	}
	var intent map[string]string
	if err := json.Unmarshal(first.Operation.Intent, &intent); err != nil || intent["revision-id"] != first.Selection.RevisionID {
		t.Fatalf("operation intent does not identify its snapshot: %s, error=%v", first.Operation.Intent, err)
	}
	if _, _, err := store.Update(ctx, progressUpdate(first.Operation, "fail", state.OperationFailed, "{}")); err != nil {
		t.Fatal(err)
	}

	secondRequest := request
	secondRequest.IdempotencyKey = "second"
	secondRequest.ExpectedVersion, secondRequest.ExpectedRevisionID = first.Selection.Version, first.Selection.RevisionID
	second, created, err := store.AcceptDeploy(ctx, secondRequest)
	if err != nil || !created || second.Operation.ID == first.Operation.ID ||
		second.Selection.RevisionID == first.Selection.RevisionID || second.Selection.Version != 2 ||
		second.Selection.BaseRevisionID != first.Selection.RevisionID {
		t.Fatalf("second deploy with identical manifest: %+v, created=%v, error=%v", second, created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStore(t, path)
	replay, created, err := store.AcceptDeploy(ctx, request)
	if err != nil || created || replay.Operation.ID != first.Operation.ID || replay.Selection != first.Selection ||
		replay.Operation.Status != state.OperationFailed || !bytes.Equal(replay.Operation.Result, []byte("{}")) {
		t.Fatalf("historical retry: %+v, created=%v, error=%v", replay, created, err)
	}
	if got, err := store.CurrentRevision(ctx, project.SpaceID, project.ID); err != nil || got != second.Selection {
		t.Fatalf("historical retry changed current revision: %+v, error=%v", got, err)
	}

	changed := request
	changed.Manifest = bytes.ReplaceAll(changed.Manifest, []byte(":v1"), []byte(":v2"))
	if got, created, err := store.AcceptDeploy(ctx, changed); !errors.Is(err, state.ErrConflict) || created || got.Operation.ID != "" {
		t.Fatalf("accepted key allowed changed manifest: %+v, created=%v, error=%v", got, created, err)
	}
	changed = request
	changed.ExpectedVersion, changed.ExpectedRevisionID = secondRequest.ExpectedVersion, secondRequest.ExpectedRevisionID
	if got, created, err := store.AcceptDeploy(ctx, changed); !errors.Is(err, state.ErrConflict) || created || got.Operation.ID != "" {
		t.Fatalf("accepted key allowed changed base: %+v, created=%v, error=%v", got, created, err)
	}

	foreign := request
	other := registeredProject(t, store, "staging", "shop")
	foreign.SpaceID = other.SpaceID
	if got, created, err := store.AcceptDeploy(ctx, foreign); !errors.Is(err, state.ErrNotFound) || created || got.Operation.ID != "" {
		t.Fatalf("wrong space recovered an operation: %+v, created=%v, error=%v", got, created, err)
	}
	if got, created, err := store.AcceptDeploy(ctx, deployRequest(other, request.IdempotencyKey)); err != nil || !created || got.Operation.ID == first.Operation.ID || got.Selection.ProjectID != other.ID {
		t.Fatalf("another project could not reuse the key: %+v, created=%v, error=%v", got, created, err)
	}
}

func TestAcceptDeployRollsBackOnSelectionFailureAndStaleBase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	project := registeredProject(t, store, "production", "shop")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertCounts := func(want int) {
		t.Helper()
		var snapshots, operations, selections int
		err := db.QueryRow(`SELECT (SELECT count(*) FROM revisions), (SELECT count(*) FROM operations), (SELECT count(*) FROM revision_selections)`).Scan(&snapshots, &operations, &selections)
		if err != nil || snapshots != want || operations != want || selections != want {
			t.Fatalf("deploy records: snapshots=%d operations=%d selections=%d want=%d each, error=%v", snapshots, operations, selections, want, err)
		}
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_selection BEFORE INSERT ON revision_selections
		BEGIN SELECT RAISE(ABORT, 'fixture selection failure'); END`); err != nil {
		t.Fatal(err)
	}
	request := deployRequest(project, "retry")
	if got, created, err := store.AcceptDeploy(ctx, request); err == nil || errors.Is(err, state.ErrConflict) || created || got.Operation.ID != "" {
		t.Fatalf("storage failure: %+v, created=%v, error=%v", got, created, err)
	}
	assertCounts(0)
	if _, err := db.Exec("DROP TRIGGER reject_selection"); err != nil {
		t.Fatal(err)
	}
	first, created, err := store.AcceptDeploy(ctx, request)
	if err != nil || !created {
		t.Fatalf("storage failure reserved key: %+v, created=%v, error=%v", first, created, err)
	}
	request.IdempotencyKey = "stale"
	if got, created, err := store.AcceptDeploy(ctx, request); !errors.Is(err, state.ErrConflict) || created || got.Operation.ID != "" {
		t.Fatalf("stale base: %+v, created=%v, error=%v", got, created, err)
	}
	request.ExpectedVersion, request.ExpectedRevisionID = first.Selection.Version, "wrong-revision"
	if got, created, err := store.AcceptDeploy(ctx, request); !errors.Is(err, state.ErrConflict) || created || got.Operation.ID != "" {
		t.Fatalf("wrong base ID: %+v, created=%v, error=%v", got, created, err)
	}
	assertCounts(1)
	request.ExpectedRevisionID = first.Selection.RevisionID
	if got, created, err := store.AcceptDeploy(ctx, request); err != nil || !created || got.Selection.Version != 2 {
		t.Fatalf("base conflict reserved key: %+v, created=%v, error=%v", got, created, err)
	}
	assertCounts(2)
}

func TestAcceptDeployConcurrentRequests(t *testing.T) {
	for _, sameKey := range []bool{true, false} {
		t.Run(fmt.Sprintf("same-key=%t", sameKey), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "state.db")
			first, second := openStore(t, path), openStore(t, path)
			project := registeredProject(t, first, "production", "shop")
			type result struct {
				deploy  state.DeployAcceptance
				created bool
				err     error
			}
			start, done := make(chan struct{}), make(chan result, 2)
			for i, store := range []*sqlite.Store{first, second} {
				go func() {
					key := "same"
					if !sameKey {
						key = fmt.Sprintf("deploy-%d", i)
					}
					<-start
					got, created, err := store.AcceptDeploy(ctx, deployRequest(project, key))
					done <- result{got, created, err}
				}()
			}
			close(start)
			a, b := <-done, <-done
			if b.created {
				a, b = b, a
			}
			if a.err != nil || !a.created || a.deploy.Operation.ID == "" || b.created {
				t.Fatalf("expected exactly one accepted deploy: %+v %+v", a, b)
			}
			if sameKey {
				if b.err != nil || b.deploy.Operation.ID != a.deploy.Operation.ID || b.deploy.Selection != a.deploy.Selection {
					t.Fatalf("duplicate did not recover acceptance: %+v %+v", a, b)
				}
			} else if !errors.Is(b.err, state.ErrConflict) || b.deploy.Operation.ID != "" {
				t.Fatalf("stale contender did not conflict: %+v", b)
			}
			if head, err := first.CurrentRevision(ctx, project.SpaceID, project.ID); err != nil || head != a.deploy.Selection {
				t.Fatalf("current revision: %+v, error=%v", head, err)
			}
		})
	}
}

func TestAcceptDeployRejectsStandaloneOperationKey(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	project := registeredProject(t, store, "production", "shop")
	request := deployRequest(project, "already-used")
	op, _, err := store.Accept(ctx, state.Submission{Scope: project.ID, IdempotencyKey: request.IdempotencyKey, Kind: "deploy", Intent: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	if got, created, err := store.AcceptDeploy(ctx, request); !errors.Is(err, state.ErrConflict) || created || got.Operation.ID != "" {
		t.Fatalf("deploy adopted a preexisting operation: %+v, created=%v, error=%v", got, created, err)
	}
	if got, err := store.Get(ctx, project.ID, op.ID); err != nil || got.Status != state.OperationPending || !bytes.Equal(got.Intent, op.Intent) {
		t.Fatalf("old operation changed: %+v, error=%v", got, err)
	}
	if got, err := store.CurrentRevision(ctx, project.SpaceID, project.ID); !errors.Is(err, state.ErrNotFound) || got.Version != 0 {
		t.Fatalf("unbound operation became current: %+v, error=%v", got, err)
	}

	// Even a standalone operation that selected a snapshot retains its own
	// receipt and cannot satisfy the atomic deploy request.
	revision, _, err := store.CreateRevision(ctx, state.CreateRevisionRequest{
		SpaceID: project.SpaceID, ProjectID: project.ID, Manifest: request.Manifest, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, progressUpdate(op, "start", state.OperationRunning, "")); err != nil {
		t.Fatal(err)
	}
	selection, _, err := store.AdmitRevision(ctx, state.RevisionAdmission{
		SpaceID: project.SpaceID, ProjectID: project.ID, RevisionID: revision.ID, OperationID: op.ID, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, created, err := store.AcceptDeploy(ctx, request); !errors.Is(err, state.ErrConflict) || created || got.Operation.ID != "" {
		t.Fatalf("deploy adopted a standalone selection: %+v, created=%v, error=%v", got, created, err)
	}
	if got, err := store.CurrentRevision(ctx, project.SpaceID, project.ID); err != nil || got != selection {
		t.Fatalf("standalone selection changed: %+v, error=%v", got, err)
	}
	request.IdempotencyKey = "new-deploy"
	request.ExpectedVersion, request.ExpectedRevisionID = selection.Version, selection.RevisionID
	if got, created, err := store.AcceptDeploy(ctx, request); err != nil || !created || got.Selection.Version != selection.Version+1 || got.Selection.BaseRevisionID != selection.RevisionID {
		t.Fatalf("deploy could not follow a standalone selection: %+v, created=%v, error=%v", got, created, err)
	}
}
