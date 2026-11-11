package sqlite_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

func generationRequest(t *testing.T, store *sqlite.Store, id string) state.GenerationRequest {
	t.Helper()
	ctx := context.Background()
	op, _, err := store.Accept(ctx, state.Submission{Scope: "project-a", IdempotencyKey: "operation-" + id, Kind: "deploy", Intent: []byte(`{"slot":"api/0"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, progressUpdate(op, "start", state.OperationRunning, "")); err != nil {
		t.Fatal(err)
	}
	return state.GenerationRequest{Scope: op.Scope, SlotID: "api/0", ID: id, OperationID: op.ID, IdempotencyKey: "generation-" + id, Intent: []byte(`{"image":"resolved-image-reference"}`)}
}

func TestGenerationHeadAndHistorySurviveReopenAndOldReplay(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	first := generationRequest(t, store, "generation-a")
	op, err := store.Get(ctx, first.Scope, first.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	head, created, err := store.AdmitGeneration(ctx, first)
	if err != nil || !created || head.ID != first.ID || head.Version != 1 || head.OperationID != first.OperationID || !bytes.Equal(head.Intent, first.Intent) {
		t.Fatalf("first generation: %#v %v %v", head, created, err)
	}
	if _, _, err := store.Update(ctx, progressUpdate(op, "finish", state.OperationSucceeded, `{}`)); err != nil {
		t.Fatal(err)
	}
	second := generationRequest(t, store, "generation-b")
	second.ExpectedVersion = head.Version
	head, created, err = store.AdmitGeneration(ctx, second)
	if err != nil || !created || head.ID != second.ID || head.Version != 2 {
		t.Fatalf("successor generation: %#v %v %v", head, created, err)
	}
	head.Intent[0], second.Intent[0] = '!', '!'
	second.Intent = []byte(`{"image":"resolved-image-reference"}`)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || bytes.Contains(contents, []byte(first.IdempotencyKey)) || bytes.Contains(contents, []byte(second.IdempotencyKey)) {
		t.Fatal("generation key was stored raw or database could not be read")
	}
	store = openStore(t, path)
	for _, request := range []state.GenerationRequest{first, second} {
		head, created, err = store.AdmitGeneration(ctx, request)
		if err != nil || created || head.ID != second.ID || head.Version != 2 || !bytes.Equal(head.Intent, second.Intent) {
			t.Fatalf("old replay revived a generation: %#v %v %v", head, created, err)
		}
	}
	old, err := store.GetGeneration(ctx, first.Scope, first.SlotID, first.ID)
	if err != nil || old.Version != 1 || old.OperationID != first.OperationID || !bytes.Equal(old.Intent, first.Intent) {
		t.Fatalf("history lost: %#v %v", old, err)
	}
	head, err = store.CurrentGeneration(ctx, first.Scope, first.SlotID)
	if err != nil || head.ID != second.ID || head.Version != 2 {
		t.Fatalf("current generation after reopen: %#v %v", head, err)
	}
	for _, mode := range []string{"scope", "slot", "generation"} {
		scope, slot, id := first.Scope, first.SlotID, first.ID
		switch mode {
		case "scope":
			scope = "other"
		case "slot":
			slot = "api/1"
		case "generation":
			id = "missing"
		}
		if _, err := store.GetGeneration(ctx, scope, slot, id); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("missing/foreign generation: %s %v", mode, err)
		}
	}
	if _, err := store.CurrentGeneration(ctx, "other", first.SlotID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("foreign head exposed: %v", err)
	}
}

func TestGenerationAdmissionRejectsRebindingAndObsoleteOperations(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	first := generationRequest(t, store, "generation-a")
	second := generationRequest(t, store, "generation-b")
	if _, _, err := store.AdmitGeneration(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"changed intent", "changed identity", "changed operation", "changed base", "same operation new key", "stale base", "reused generation", "another slot same identity"} {
		t.Run(mode, func(t *testing.T) {
			request := first
			switch mode {
			case "changed intent":
				request.Intent = []byte(`{"different":true}`)
			case "changed identity":
				request.ID = "other"
			case "changed operation":
				request.OperationID = second.OperationID
			case "changed base":
				request.ExpectedVersion = 1
			case "same operation new key":
				request.ID, request.IdempotencyKey, request.ExpectedVersion = "other", "other", 1
			case "stale base":
				request = second
			case "reused generation":
				request = second
				request.ID, request.ExpectedVersion = first.ID, 1
			case "another slot same identity":
				request = second
				request.ID, request.SlotID = first.ID, "api/1"
			}
			got, created, err := store.AdmitGeneration(context.Background(), request)
			if !errors.Is(err, state.ErrConflict) || created || got.ID != "" {
				t.Fatalf("generation conflict hidden: %#v %v %v", got, created, err)
			}
		})
	}
	second.ExpectedVersion = 1
	if got, created, err := store.AdmitGeneration(context.Background(), second); err != nil || !created || got.Version != 2 {
		t.Fatalf("rejected admission reserved key or changed head: %#v %v %v", got, created, err)
	}
}

func TestGenerationAdmissionValidatesOperationAndInput(t *testing.T) {
	for _, mode := range []string{"scope", "slot", "id", "operation", "key", "intent", "negative version", "missing slot version", "missing operation", "foreign operation", "pending operation", "terminal operation", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
			request := generationRequest(t, store, "generation-a")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "scope":
				request.Scope = " "
			case "slot":
				request.SlotID = " "
			case "id":
				request.ID = ""
			case "operation":
				request.OperationID = ""
			case "key":
				request.IdempotencyKey = " "
			case "intent":
				request.Intent = nil
			case "negative version":
				request.ExpectedVersion = -1
			case "missing slot version":
				request.ExpectedVersion = 1
			case "missing operation":
				request.OperationID = "missing"
			case "foreign operation":
				request.Scope = "other"
			case "pending operation":
				op, _, err := store.Accept(ctx, state.Submission{Scope: request.Scope, IdempotencyKey: "pending", Kind: "deploy", Intent: []byte(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				request.OperationID = op.ID
			case "terminal operation":
				op, err := store.Get(ctx, request.Scope, request.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.Update(ctx, progressUpdate(op, "finish", state.OperationFailed, `{}`)); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			if got, created, err := store.AdmitGeneration(ctx, request); err == nil || created || got.ID != "" {
				t.Fatalf("invalid admission: %#v %v %v", got, created, err)
			} else if mode == "canceled" && !errors.Is(err, context.Canceled) || (mode == "missing operation" || mode == "foreign operation") && !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("admission lost cause: %v", err)
			}
			if _, err := store.CurrentGeneration(context.Background(), "project-a", "api/0"); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("rejected admission created a slot: %v", err)
			}
		})
	}
}

func TestGenerationAdmissionConcurrentHandlesSerializeHead(t *testing.T) {
	for _, identical := range []bool{false, true} {
		t.Run(map[bool]string{false: "competing operations", true: "identical retry"}[identical], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			stores := []*sqlite.Store{openStore(t, path), openStore(t, path)}
			first := generationRequest(t, stores[0], "generation-a")
			second := first
			if !identical {
				seed := generationRequest(t, stores[0], "generation-seed")
				if _, _, err := stores[0].AdmitGeneration(context.Background(), seed); err != nil {
					t.Fatal(err)
				}
				second = generationRequest(t, stores[0], "generation-b")
				first.ExpectedVersion, second.ExpectedVersion = 1, 1
			}
			type outcome struct {
				generation state.Generation
				created    bool
				err        error
			}
			start, done := make(chan struct{}), make(chan outcome, 2)
			for i, request := range []state.GenerationRequest{first, second} {
				go func() {
					<-start
					got, created, err := stores[i].AdmitGeneration(context.Background(), request)
					done <- outcome{got, created, err}
				}()
			}
			close(start)
			createdCount, conflicts := 0, 0
			for range 2 {
				got := <-done
				if got.err != nil {
					if identical || !errors.Is(got.err, state.ErrConflict) {
						t.Fatalf("concurrent admission: %#v", got)
					}
					conflicts++
				} else if got.created {
					createdCount++
				}
			}
			if createdCount != 1 || !identical && conflicts != 1 {
				t.Fatalf("head admitted twice: creates=%d conflicts=%d", createdCount, conflicts)
			}
			head, err := stores[0].CurrentGeneration(context.Background(), first.Scope, first.SlotID)
			if err != nil || head.Version != first.ExpectedVersion+1 {
				t.Fatalf("concurrent head: %#v %v", head, err)
			}
		})
	}
}

func TestGenerationCanceledWhileLockedDoesNotReserveAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	request := generationRequest(t, store, "generation-a")
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
	if got, created, err := store.AdmitGeneration(ctx, request); !errors.Is(err, context.DeadlineExceeded) || created || got.ID != "" {
		t.Fatalf("canceled admission lost cause: %#v %v %v", got, created, err)
	}
	if _, err := blocker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if got, created, err := store.AdmitGeneration(context.Background(), request); err != nil || !created || got.Version != 1 {
		t.Fatalf("canceled admission reserved key or advanced slot: %#v %v %v", got, created, err)
	}
}

func TestGenerationMigrationRetainsVersionTwoProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Frozen version-2 tables from #55.
	if _, err := db.Exec(versionOneSchema + versionTwoProgressSchema); err != nil {
		t.Fatal(err)
	}
	request := submission()
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	if _, err := db.Exec(`INSERT INTO operations (id, scope, key_hash, kind, intent, status, version, result) VALUES (?, ?, ?, ?, ?, 'running', 2, X'')`, "op_legacy", request.Scope, hex.EncodeToString(key[:]), request.Kind, request.Intent); err != nil {
		t.Fatal(err)
	}
	startKey := sha256.Sum256([]byte("start"))
	if _, err := db.Exec(`INSERT INTO operation_updates VALUES (?, ?, 1, 'running', X'')`, "op_legacy", hex.EncodeToString(startKey[:])); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, path)
	op, created, err := store.Accept(context.Background(), request)
	if err != nil || created || op.ID != "op_legacy" || op.Status != state.OperationRunning || op.Version != 2 {
		t.Fatalf("migrated progress: %#v %v %v", op, created, err)
	}
	start := progressUpdate(op, "start", state.OperationRunning, "")
	start.ExpectedVersion = 1
	if _, changed, err := store.Update(context.Background(), start); err != nil || changed {
		t.Fatalf("migration lost receipt: %v %v", changed, err)
	}
	admission := state.GenerationRequest{Scope: op.Scope, SlotID: "api/0", ID: "generation-a", OperationID: op.ID, IdempotencyKey: "admit", Intent: []byte(`{}`)}
	if got, created, err := store.AdmitGeneration(context.Background(), admission); err != nil || !created || got.Version != 1 {
		t.Fatalf("migrated generation: %#v %v %v", got, created, err)
	}
}

const versionTwoProgressSchema = `
ALTER TABLE operations ADD COLUMN status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'canceled'));
ALTER TABLE operations ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0);
ALTER TABLE operations ADD COLUMN result BLOB NOT NULL DEFAULT X'' CHECK ((status IN ('pending', 'running') AND length(result) = 0) OR (status IN ('succeeded', 'failed', 'canceled') AND length(result) > 0));
CREATE TABLE operation_updates (
operation_id TEXT NOT NULL REFERENCES operations(id), key_hash TEXT NOT NULL,
expected_version INTEGER NOT NULL CHECK (expected_version > 0), status TEXT NOT NULL, result BLOB NOT NULL,
PRIMARY KEY (operation_id, key_hash), UNIQUE (operation_id, expected_version)) STRICT;
PRAGMA user_version = 2;`
