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

func terminalObservation(process state.ProcessObservation, cause state.ProcessTerminalCause) state.ProcessTerminalObservation {
	evidence := []byte(`{"exit-code":0}`)
	if cause == state.ProcessStartupTimeout {
		evidence = []byte(`{"startup-timeout":"1m"}`)
	} else if cause == state.ProcessUnhealthy {
		evidence = []byte(`{"unhealthy-failures":3}`)
	}
	return state.ProcessTerminalObservation{Scope: process.Scope, SlotID: process.SlotID, GenerationID: process.GenerationID,
		IdempotencyKey: "terminal-key-do-not-persist", Identity: bytes.Clone(process.Identity), StartedAt: process.StartedAt,
		Terminal: state.ProcessTerminal{Cause: cause, OccurredAt: process.StartedAt.Add(time.Minute), Evidence: evidence}}
}

func TestProcessTerminalSurvivesReopenAndStopsFreshObservations(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		cause state.ProcessTerminalCause
		ready bool
	}{
		{"exit before readiness", state.ProcessExited, false},
		{"exit after readiness", state.ProcessExited, true},
		{"startup timeout", state.ProcessStartupTimeout, false},
		{"unhealthy", state.ProcessUnhealthy, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			cause := scenario.cause
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "state.db")
			store := openStore(t, path)
			initial := processObservation(t, store, "generation-a")
			initial.EverReady = scenario.ready
			if _, _, err := store.RecordProcess(ctx, initial); err != nil {
				t.Fatal(err)
			}
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
			request := terminalObservation(initial, cause)
			got, recorded, err := store.RecordProcessTerminal(ctx, request)
			if err != nil || !recorded || got.Terminal == nil || got.Terminal.Cause != cause || got.EverReady != initial.EverReady {
				t.Fatalf("record terminal: %#v %v %v", got, recorded, err)
			}
			got.Terminal.Evidence[0] = '!'
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(path)
			if err != nil || bytes.Contains(contents, []byte(request.IdempotencyKey)) {
				t.Fatal("raw terminal key persisted or database unreadable")
			}
			store = openStore(t, path)
			request.StartedAt, request.Terminal.OccurredAt = request.StartedAt.UTC(), request.Terminal.OccurredAt.UTC()
			got, recorded, err = store.RecordProcessTerminal(ctx, request)
			if err != nil || recorded || got.Terminal == nil || !got.Terminal.OccurredAt.Equal(request.Terminal.OccurredAt) || !bytes.Equal(got.Terminal.Evidence, request.Terminal.Evidence) {
				t.Fatalf("terminal replay lost facts: %#v %v %v", got, recorded, err)
			}
			// Even an old readiness receipt returns the terminal snapshot.
			got, recorded, err = store.RecordProcess(ctx, initial)
			if err != nil || recorded || got.Terminal == nil {
				t.Fatalf("old receipt hid terminal: %#v %v %v", got, recorded, err)
			}
			initial.IdempotencyKey = "late-readiness"
			if _, _, err := store.RecordProcess(ctx, initial); !errors.Is(err, state.ErrConflict) {
				t.Fatalf("fresh unchanged observation accepted after terminal: %v", err)
			}
			initial.EverReady = !initial.EverReady
			if _, _, err := store.RecordProcess(ctx, initial); !errors.Is(err, state.ErrConflict) {
				t.Fatalf("terminal generation resumed: %v", err)
			}
			stored, err := store.GetProcess(ctx, request.Scope, request.SlotID, request.GenerationID)
			if err != nil || stored.Terminal == nil || stored.Terminal.Cause != cause || !bytes.Equal(stored.Terminal.Evidence, request.Terminal.Evidence) {
				t.Fatalf("read terminal: %#v %v", stored, err)
			}
			for _, scopeSlot := range [][2]string{{"other", request.SlotID}, {request.Scope, "other"}} {
				got, err := store.GetProcess(ctx, scopeSlot[0], scopeSlot[1], request.GenerationID)
				if !errors.Is(err, state.ErrNotFound) || got.Terminal != nil {
					t.Fatalf("foreign read exposed terminal: %#v %v", got, err)
				}
			}
		})
	}
}

func TestProcessTerminalIsImmutableAndSuccessorsInheritNothing(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	initial := processObservation(t, store, "generation-a")
	if _, _, err := store.RecordProcess(ctx, initial); err != nil {
		t.Fatal(err)
	}
	request := terminalObservation(initial, state.ProcessExited)
	if _, _, err := store.RecordProcessTerminal(ctx, request); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"key", "identity", "start", "cause", "time", "evidence"} {
		t.Run(mode, func(t *testing.T) {
			changed := request
			switch mode {
			case "key":
				changed.IdempotencyKey = "another-key"
			case "identity":
				changed.Identity = []byte("another-process")
			case "start":
				changed.StartedAt = changed.StartedAt.Add(time.Nanosecond)
			case "cause":
				changed.Terminal.Cause = state.ProcessStartupTimeout
			case "time":
				changed.Terminal.OccurredAt = changed.Terminal.OccurredAt.Add(time.Nanosecond)
			case "evidence":
				changed.Terminal.Evidence = []byte(`{"exit-code":1}`)
			}
			if got, recorded, err := store.RecordProcessTerminal(ctx, changed); !errors.Is(err, state.ErrConflict) || recorded || got.Terminal != nil {
				t.Fatalf("terminal rebound: %#v %v %v", got, recorded, err)
			}
		})
	}
	next := generationRequest(t, store, "generation-b")
	next.ExpectedVersion = 1
	if _, _, err := store.AdmitGeneration(ctx, next); err != nil {
		t.Fatal(err)
	}
	if got, recorded, err := store.RecordProcessTerminal(ctx, request); err != nil || recorded || got.Terminal == nil {
		t.Fatalf("historical replay: %#v %v %v", got, recorded, err)
	}
	successor := initial
	successor.GenerationID, successor.Identity, successor.IdempotencyKey = next.ID, []byte("successor-process"), "successor-binding"
	if got, recorded, err := store.RecordProcess(ctx, successor); err != nil || !recorded || got.Terminal != nil || got.EverReady {
		t.Fatalf("successor inherited facts: %#v %v %v", got, recorded, err)
	}
}

func TestProcessTerminalRejectsInvalidAndObsoleteFacts(t *testing.T) {
	for _, mode := range []string{"scope", "slot", "generation", "key", "identity", "start", "zero time", "invalid time", "before start", "empty evidence", "observation-lost", "unavailable", "replaced", "canceled cause", "unhealthy before ready", "timeout after ready", "unbound", "foreign scope", "foreign slot", "missing", "obsolete", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
			initial := processObservation(t, store, "generation-a")
			initial.EverReady = mode == "timeout after ready"
			if mode != "unbound" {
				if _, _, err := store.RecordProcess(ctx, initial); err != nil {
					t.Fatal(err)
				}
			}
			request := terminalObservation(initial, state.ProcessExited)
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
			case "zero time":
				request.Terminal.OccurredAt = time.Time{}
			case "invalid time":
				request.Terminal.OccurredAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "before start":
				request.Terminal.OccurredAt = request.StartedAt.Add(-time.Nanosecond)
			case "empty evidence":
				request.Terminal.Evidence = nil
			case "observation-lost", "unavailable", "replaced", "canceled cause":
				request.Terminal.Cause = state.ProcessTerminalCause(mode)
			case "unhealthy before ready":
				request.Terminal.Cause = state.ProcessUnhealthy
			case "timeout after ready":
				request.Terminal.Cause = state.ProcessStartupTimeout
			case "foreign scope":
				request.Scope = "other"
			case "foreign slot":
				request.SlotID = "other"
			case "missing":
				request.GenerationID = "missing"
			case "obsolete":
				next := generationRequest(t, store, "generation-b")
				next.ExpectedVersion = 1
				if _, _, err := store.AdmitGeneration(ctx, next); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			got, recorded, err := store.RecordProcessTerminal(ctx, request)
			if err == nil || recorded || got.GenerationID != "" {
				t.Fatalf("invalid terminal accepted: %#v %v %v", got, recorded, err)
			}
			switch mode {
			case "unbound", "foreign scope", "foreign slot", "missing":
				if !errors.Is(err, state.ErrNotFound) {
					t.Fatalf("lost missing cause: %v", err)
				}
			case "obsolete", "unhealthy before ready", "timeout after ready":
				if !errors.Is(err, state.ErrConflict) {
					t.Fatalf("lost conflict cause: %v", err)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
			}
			stored, err := store.GetProcess(context.Background(), initial.Scope, initial.SlotID, initial.GenerationID)
			if mode != "unbound" && (err != nil || stored.Terminal != nil) {
				t.Fatalf("rejection changed process: %#v %v", stored, err)
			}
		})
	}
}

func TestProcessTerminalsSerializeAcrossHandles(t *testing.T) {
	for _, race := range []string{"same terminal", "different terminal", "readiness"} {
		t.Run(race, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			stores := []*sqlite.Store{openStore(t, path), openStore(t, path)}
			initial := processObservation(t, stores[0], "generation-a")
			if _, _, err := stores[0].RecordProcess(context.Background(), initial); err != nil {
				t.Fatal(err)
			}
			first := terminalObservation(initial, state.ProcessStartupTimeout)
			second := first
			if race == "different terminal" {
				second.Terminal.Cause = state.ProcessExited
			}
			ready := initial
			ready.IdempotencyKey, ready.EverReady = "ready", true
			start, done := make(chan struct{}), make(chan error, 2)
			go func() {
				<-start
				_, _, err := stores[0].RecordProcessTerminal(context.Background(), first)
				done <- err
			}()
			go func() {
				<-start
				var err error
				if race == "readiness" {
					_, _, err = stores[1].RecordProcess(context.Background(), ready)
				} else {
					_, _, err = stores[1].RecordProcessTerminal(context.Background(), second)
				}
				done <- err
			}()
			close(start)
			conflicts := 0
			for range 2 {
				if err := <-done; errors.Is(err, state.ErrConflict) {
					conflicts++
				} else if err != nil {
					t.Fatal(err)
				}
			}
			want := 1
			if race == "same terminal" {
				want = 0
			}
			if conflicts != want {
				t.Fatalf("competing outcomes accepted: conflicts=%d", conflicts)
			}
			got, err := stores[0].GetProcess(context.Background(), initial.Scope, initial.SlotID, initial.GenerationID)
			if err != nil || (race == "readiness" && got.EverReady == (got.Terminal != nil)) || (race != "readiness" && got.Terminal == nil) {
				t.Fatalf("inconsistent outcome: %#v %v", got, err)
			}
		})
	}
}

func TestProcessTerminalWriteFailureAndLockedCancellationReserveNothing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openStore(t, path)
	initial := processObservation(t, store, "generation-a")
	if _, _, err := store.RecordProcess(ctx, initial); err != nil {
		t.Fatal(err)
	}
	request := terminalObservation(initial, state.ProcessExited)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TRIGGER reject_terminal BEFORE INSERT ON process_terminals BEGIN SELECT RAISE(ABORT, 'terminal rejected'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RecordProcessTerminal(ctx, request); err == nil {
		t.Fatal("failed write accepted")
	}
	if got, err := store.GetProcess(ctx, initial.Scope, initial.SlotID, initial.GenerationID); err != nil || got.Terminal != nil {
		t.Fatalf("terminal escaped rollback: %#v %v", got, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_terminal"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer db.Exec("ROLLBACK")
	limited, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if got, recorded, err := store.RecordProcessTerminal(limited, request); !errors.Is(err, context.DeadlineExceeded) || recorded || got.Terminal != nil {
		t.Fatalf("locked cancellation: %#v %v %v", got, recorded, err)
	}
	if _, err := db.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if got, recorded, err := store.RecordProcessTerminal(ctx, request); err != nil || !recorded || got.Terminal == nil {
		t.Fatalf("failure reserved receipt: %#v %v %v", got, recorded, err)
	}
}

func TestTerminalMigrationPreservesProcessReceiptsAndRollsBackFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "rollback chain"}[fail], func(t *testing.T) {
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
				if _, err := db.Exec("CREATE TABLE process_terminals (unexpected TEXT)"); err != nil {
					t.Fatal(err)
				}
				if store, err := sqlite.Open(context.Background(), path); err == nil {
					_ = store.Close()
					t.Fatal("failed migration accepted")
				}
				var version, count int
				if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
					t.Fatalf("failed migration changed version: %d %v", version, err)
				}
				if err := db.QueryRow("SELECT count(*) FROM generation_processes").Scan(&count); err == nil {
					t.Fatal("migration chain left partial binding table")
				}
				if err := db.QueryRow("SELECT count(*) FROM process_observations").Scan(&count); err == nil {
					t.Fatal("migration chain left partial receipt table")
				}
				if err := db.QueryRow("SELECT count(*) FROM generations WHERE id='generation-a'").Scan(&count); err != nil || count != 1 {
					t.Fatalf("migration lost history: %d %v", count, err)
				}
				return
			}
			if _, err := db.Exec(versionFourSchema); err != nil {
				t.Fatal(err)
			}
			// Frozen receipt for SHA256("bind"); migration must not admit it again.
			if _, err := db.Exec(`INSERT INTO generation_processes VALUES ('scope','api/0','generation-a',X'72756E74696D65','identity-hash','2026-11-11T12:00:00Z',1);
				INSERT INTO process_observations VALUES ('scope','api/0','generation-a','f08dd851c430f52f3fbe9692678a2e2c3cf9009035a13a5cf080ce9ed2125ce9',1);`); err != nil {
				t.Fatal(err)
			}
			store := openStore(t, path)
			ctx := context.Background()
			initial := state.ProcessObservation{Scope: "scope", SlotID: "api/0", GenerationID: "generation-a", IdempotencyKey: "bind", Identity: []byte("runtime"), StartedAt: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), EverReady: true}
			got, recorded, err := store.RecordProcess(ctx, initial)
			if err != nil || recorded || !got.EverReady || got.Terminal != nil {
				t.Fatalf("migration lost receipt or fabricated terminal: %#v %v %v", got, recorded, err)
			}
			op, err := store.Get(ctx, "scope", "op_legacy")
			if err != nil || op.Status != state.OperationSucceeded || op.Version != 3 || !bytes.Equal(op.Result, []byte(`{}`)) {
				t.Fatalf("migration lost operation: %#v %v", op, err)
			}
			if got, recorded, err := store.RecordProcessTerminal(ctx, terminalObservation(initial, state.ProcessUnhealthy)); err != nil || !recorded || got.Terminal == nil {
				t.Fatalf("migrated terminal: %#v %v %v", got, recorded, err)
			}
			var version int
			if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 5 {
				t.Fatalf("schema version: %d %v", version, err)
			}
		})
	}
}

// Frozen version-4 process tables from #57.
const versionFourSchema = `CREATE TABLE generation_processes (
scope TEXT NOT NULL, slot_id TEXT NOT NULL, generation_id TEXT NOT NULL,
identity BLOB NOT NULL CHECK (length(identity) > 0), identity_hash TEXT NOT NULL,
started_at TEXT NOT NULL, ever_ready INTEGER NOT NULL CHECK (ever_ready IN (0, 1)),
PRIMARY KEY (scope, slot_id, generation_id), UNIQUE (scope, identity_hash),
FOREIGN KEY (scope, slot_id, generation_id) REFERENCES generations(scope, slot_id, id)) STRICT;
CREATE TABLE process_observations (
scope TEXT NOT NULL, slot_id TEXT NOT NULL, generation_id TEXT NOT NULL, key_hash TEXT NOT NULL,
ever_ready INTEGER NOT NULL CHECK (ever_ready IN (0, 1)), PRIMARY KEY (scope, slot_id, generation_id, key_hash),
FOREIGN KEY (scope, slot_id, generation_id) REFERENCES generation_processes(scope, slot_id, generation_id)) STRICT;
PRAGMA user_version = 4;`
