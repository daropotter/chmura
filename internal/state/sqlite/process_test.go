package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

func processObservation(t *testing.T, store *sqlite.Store, id string) state.ProcessObservation {
	t.Helper()
	request := generationRequest(t, store, id)
	if _, _, err := store.AdmitGeneration(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	return state.ProcessObservation{Scope: request.Scope, SlotID: request.SlotID, GenerationID: request.ID,
		IdempotencyKey: "bind-process-" + id, Identity: []byte("exact-runtime-identity-" + id),
		StartedAt: time.Date(2026, 11, 11, 12, 0, 0, 123456789, time.FixedZone("offset", 3600))}
}

func TestProcessBindingAndFirstReadinessSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	initial := processObservation(t, store, "generation-a")
	generation, err := store.GetGeneration(ctx, initial.Scope, initial.SlotID, initial.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	op, err := store.Get(ctx, initial.Scope, generation.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, progressUpdate(op, "finish", state.OperationSucceeded, `{}`)); err != nil {
		t.Fatal(err)
	}
	bound, recorded, err := store.RecordProcess(ctx, initial)
	if err != nil || !recorded || bound.EverReady || !bytes.Equal(bound.Identity, initial.Identity) || !bound.StartedAt.Equal(initial.StartedAt) || bound.GenerationID != initial.GenerationID {
		t.Fatalf("bind process: %#v %v %v", bound, recorded, err)
	}
	ready := initial
	ready.IdempotencyKey, ready.EverReady = "first-ready", true
	bound, recorded, err = store.RecordProcess(ctx, ready)
	if err != nil || !recorded || !bound.EverReady {
		t.Fatalf("first readiness: %#v %v %v", bound, recorded, err)
	}
	bound.Identity[0] = '!'
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || bytes.Contains(contents, []byte(initial.IdempotencyKey)) || bytes.Contains(contents, []byte(ready.IdempotencyKey)) {
		t.Fatal("raw observation key was stored or database could not be read")
	}
	store = openStore(t, path)
	// Time-zone presentation is not a different process fact.
	initial.StartedAt = initial.StartedAt.UTC()
	for _, request := range []state.ProcessObservation{initial, ready} {
		bound, recorded, err = store.RecordProcess(ctx, request)
		if err != nil || recorded || !bound.EverReady || !bytes.Equal(bound.Identity, initial.Identity) || !bound.StartedAt.Equal(initial.StartedAt) {
			t.Fatalf("replay lost process history: %#v %v %v", bound, recorded, err)
		}
	}
	falseFact := initial
	falseFact.IdempotencyKey = "later-unready"
	if bound, recorded, err := store.RecordProcess(ctx, falseFact); err != nil || !recorded || !bound.EverReady {
		t.Fatalf("later observation cleared first readiness: %#v %v %v", bound, recorded, err)
	}
	stored, err := store.GetProcess(ctx, initial.Scope, initial.SlotID, initial.GenerationID)
	if err != nil || !stored.EverReady || !stored.StartedAt.Equal(initial.StartedAt) {
		t.Fatalf("read persisted process: %#v %v", stored, err)
	}
}

func TestProcessFactsRejectRebindingAndIsolateSuccessors(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	initial := processObservation(t, store, "generation-a")
	if _, _, err := store.RecordProcess(ctx, initial); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"identity", "start", "key reused for readiness"} {
		t.Run(mode, func(t *testing.T) {
			request := initial
			switch mode {
			case "identity":
				request.Identity = []byte("another-process")
			case "start":
				request.StartedAt = request.StartedAt.Add(time.Nanosecond)
			case "key reused for readiness":
				request.EverReady = true
			}
			if got, recorded, err := store.RecordProcess(ctx, request); !errors.Is(err, state.ErrConflict) || recorded || got.GenerationID != "" {
				t.Fatalf("process facts rebound: %#v %v %v", got, recorded, err)
			}
		})
	}
	ready := initial
	ready.IdempotencyKey, ready.EverReady = "first-ready", true
	if _, _, err := store.RecordProcess(ctx, ready); err != nil {
		t.Fatal(err)
	}
	next := generationRequest(t, store, "generation-b")
	next.ExpectedVersion = 1
	if _, _, err := store.AdmitGeneration(ctx, next); err != nil {
		t.Fatal(err)
	}
	// A historical receipt is still acknowledged, but fresh obsolete writes stop.
	if got, recorded, err := store.RecordProcess(ctx, ready); err != nil || recorded || !got.EverReady {
		t.Fatalf("historical replay: %#v %v %v", got, recorded, err)
	}
	late := ready
	late.IdempotencyKey = "late-observation"
	if _, _, err := store.RecordProcess(ctx, late); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("obsolete writer accepted: %v", err)
	}
	successor := initial
	successor.GenerationID, successor.IdempotencyKey = next.ID, "successor-binding"
	if _, _, err := store.RecordProcess(ctx, successor); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("process reused by another generation: %v", err)
	}
	successor.Identity = []byte("fresh-runtime-identity")
	if got, recorded, err := store.RecordProcess(ctx, successor); err != nil || !recorded || got.EverReady {
		t.Fatalf("successor inherited history or key was reserved: %#v %v %v", got, recorded, err)
	}
	old, err := store.GetProcess(ctx, initial.Scope, initial.SlotID, initial.GenerationID)
	if err != nil || !old.EverReady {
		t.Fatalf("successor erased history: %#v %v", old, err)
	}
}

func TestProcessObservationValidatesScopeAndFacts(t *testing.T) {
	for _, mode := range []string{"scope", "slot", "generation", "key", "identity", "start", "invalid time", "missing generation", "foreign scope", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
			request := processObservation(t, store, "generation-a")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "scope":
				request.Scope = " "
			case "slot":
				request.SlotID = ""
			case "generation":
				request.GenerationID = ""
			case "key":
				request.IdempotencyKey = " "
			case "identity":
				request.Identity = nil
			case "start":
				request.StartedAt = time.Time{}
			case "invalid time":
				request.StartedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "missing generation":
				request.GenerationID = "missing"
			case "foreign scope":
				request.Scope = "other"
			case "canceled":
				cancel()
			}
			if got, recorded, err := store.RecordProcess(ctx, request); err == nil || recorded || got.GenerationID != "" {
				t.Fatalf("invalid process observation: %#v %v %v", got, recorded, err)
			} else if mode == "canceled" && !errors.Is(err, context.Canceled) || (mode == "missing generation" || mode == "foreign scope") && !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("observation lost cause: %v", err)
			}
			if _, err := store.GetProcess(context.Background(), "project-a", "api/0", "generation-a"); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("rejected observation bound a process: %v", err)
			}
		})
	}
}

func TestProcessObservationsMergeConcurrentFirstReadiness(t *testing.T) {
	for _, sameProcess := range []bool{true, false} {
		t.Run(map[bool]string{true: "same process", false: "competing identities"}[sameProcess], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			stores := []*sqlite.Store{openStore(t, path), openStore(t, path)}
			initial := processObservation(t, stores[0], "generation-a")
			ready := initial
			ready.IdempotencyKey, ready.EverReady = "first-ready", true
			if !sameProcess {
				ready.Identity = []byte("another-runtime-identity")
			}
			start, done := make(chan struct{}), make(chan error, 2)
			for i, request := range []state.ProcessObservation{initial, ready} {
				go func() { <-start; _, _, err := stores[i].RecordProcess(context.Background(), request); done <- err }()
			}
			close(start)
			conflicts := 0
			for range 2 {
				if err := <-done; err != nil {
					if sameProcess || !errors.Is(err, state.ErrConflict) {
						t.Fatal(err)
					}
					conflicts++
				}
			}
			if !sameProcess && conflicts != 1 {
				t.Fatalf("competing identities accepted: conflicts=%d", conflicts)
			}
			stored, err := stores[0].GetProcess(context.Background(), initial.Scope, initial.SlotID, initial.GenerationID)
			wantReady := sameProcess || bytes.Equal(stored.Identity, ready.Identity)
			if err != nil || stored.EverReady != wantReady {
				t.Fatalf("concurrent facts lost first readiness: %#v %v", stored, err)
			}
		})
	}
}

func TestRestoredProcessHistoryRechecksCurrentReadiness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	request := processObservation(t, store, "generation-a")
	request.StartedAt, request.EverReady = time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), true
	if _, _, err := store.RecordProcess(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStore(t, path)
	retained, err := store.GetProcess(context.Background(), request.Scope, request.SlotID, request.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		config := &manifest.Health{Check: &manifest.Probe{TCP: &manifest.TCPCheck{Port: "web"}, Interval: manifest.Duration(time.Second)}, Ready: manifest.ReadyRule{Successes: 2}}
		var attempts atomic.Int32
		var updates []probe.Readiness
		err := probe.MonitorReadinessWithHistory(ctx, retained.StartedAt, config, retained.EverReady,
			func(context.Context, manifest.Probe) error { attempts.Add(1); return nil },
			func(readiness probe.Readiness) {
				updates = append(updates, readiness)
				if readiness.Ready {
					cancel()
				}
			})
		if !errors.Is(err, context.Canceled) || attempts.Load() != 2 || len(updates) != 2 || updates[0] != (probe.Readiness{EverReady: true}) || !updates[1].Ready {
			t.Fatalf("restored readiness bypassed fresh checks or renewed startup: attempts=%d updates=%#v error=%v", attempts.Load(), updates, err)
		}
	})
}

func TestProcessObservationCancellationWhileLockedReservesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	request := processObservation(t, store, "generation-a")
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
	if got, recorded, err := store.RecordProcess(ctx, request); !errors.Is(err, context.DeadlineExceeded) || recorded || got.GenerationID != "" {
		t.Fatalf("canceled observation: %#v %v %v", got, recorded, err)
	}
	if _, err := blocker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if got, recorded, err := store.RecordProcess(context.Background(), request); err != nil || !recorded || got.EverReady {
		t.Fatalf("canceled observation reserved facts/key: %#v %v %v", got, recorded, err)
	}
}

func TestProcessMigrationPreservesHistoryAndRollsBackFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "rollback"}[fail], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(versionOneSchema + versionTwoProgressSchema + versionThreeSchema); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO operations VALUES ('op_legacy','scope','hash','deploy',X'7B7D','succeeded',3,X'7B7D');
				INSERT INTO generations VALUES ('scope','api/0','generation-a','op_legacy','key-hash',1,X'7B7D');`); err != nil {
				t.Fatal(err)
			}
			if fail {
				if _, err := db.Exec("CREATE TABLE process_observations (unexpected TEXT)"); err != nil {
					t.Fatal(err)
				}
				if store, err := sqlite.Open(context.Background(), path); err == nil {
					_ = store.Close()
					t.Fatal("failed migration accepted")
				}
				var version int
				if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
					t.Fatalf("migration changed version: %d %v", version, err)
				}
				var count int
				if err := db.QueryRow("SELECT count(*) FROM generation_processes").Scan(&count); err == nil {
					t.Fatal("failed migration left process table")
				}
				if err := db.QueryRow("SELECT count(*) FROM generations WHERE id='generation-a'").Scan(&count); err != nil || count != 1 {
					t.Fatalf("migration lost history: %d %v", count, err)
				}
				return
			}
			store := openStore(t, path)
			generation, err := store.GetGeneration(context.Background(), "scope", "api/0", "generation-a")
			if err != nil || generation.Version != 1 || !bytes.Equal(generation.Intent, []byte(`{}`)) {
				t.Fatalf("migrated generation: %#v %v", generation, err)
			}
			op, err := store.Get(context.Background(), "scope", "op_legacy")
			if err != nil || op.Status != state.OperationSucceeded || op.Version != 3 || !bytes.Equal(op.Result, []byte(`{}`)) {
				t.Fatalf("migrated operation: %#v %v", op, err)
			}
			if _, err := store.GetProcess(context.Background(), "scope", "api/0", "generation-a"); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("migration fabricated runtime facts: %v", err)
			}
			request := state.ProcessObservation{Scope: "scope", SlotID: "api/0", GenerationID: "generation-a", IdempotencyKey: "bind", Identity: []byte("runtime-identity"), StartedAt: time.Now()}
			if got, recorded, err := store.RecordProcess(context.Background(), request); err != nil || !recorded || got.EverReady {
				t.Fatalf("migrated binding: %#v %v %v", got, recorded, err)
			}
		})
	}
}

// Frozen version-3 generation table from #56.
const versionThreeSchema = `CREATE TABLE generations (
scope TEXT NOT NULL, slot_id TEXT NOT NULL, id TEXT NOT NULL, operation_id TEXT NOT NULL REFERENCES operations(id),
key_hash TEXT NOT NULL, version INTEGER NOT NULL CHECK (version > 0), intent BLOB NOT NULL CHECK (length(intent) > 0),
PRIMARY KEY (scope, slot_id, id), UNIQUE (scope, id), UNIQUE (scope, slot_id, key_hash),
UNIQUE (scope, slot_id, version), UNIQUE (scope, slot_id, operation_id)) STRICT;
PRAGMA user_version = 3;`

func TestProcessObservationFailureRollsBackBindingAndReadiness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	initial := processObservation(t, store, "generation-a")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reject := func() {
		t.Helper()
		if _, err := db.Exec(`CREATE TRIGGER reject_process_receipt BEFORE INSERT ON process_observations BEGIN SELECT RAISE(ABORT, 'receipt rejected'); END;`); err != nil {
			t.Fatal(err)
		}
	}
	allow := func() {
		t.Helper()
		if _, err := db.Exec("DROP TRIGGER reject_process_receipt"); err != nil {
			t.Fatal(err)
		}
	}
	reject()
	if _, _, err := store.RecordProcess(context.Background(), initial); err == nil {
		t.Fatal("failed receipt accepted")
	}
	if _, err := store.GetProcess(context.Background(), initial.Scope, initial.SlotID, initial.GenerationID); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("binding escaped rollback: %v", err)
	}
	allow()
	if _, _, err := store.RecordProcess(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	ready := initial
	ready.IdempotencyKey, ready.EverReady = "first-ready", true
	reject()
	if _, _, err := store.RecordProcess(context.Background(), ready); err == nil {
		t.Fatal("failed readiness receipt accepted")
	}
	stored, err := store.GetProcess(context.Background(), initial.Scope, initial.SlotID, initial.GenerationID)
	if err != nil || stored.EverReady {
		t.Fatalf("readiness escaped rollback: %#v %v", stored, err)
	}
	allow()
	if got, recorded, err := store.RecordProcess(context.Background(), ready); err != nil || !recorded || !got.EverReady {
		t.Fatalf("failed receipt reserved key: %#v %v %v", got, recorded, err)
	}
}
