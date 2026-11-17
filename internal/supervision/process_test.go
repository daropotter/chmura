package supervision_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
	"github.com/daropotter/chmura/internal/supervision"
)

type recorderFunc func(context.Context, state.ProcessObservation) (state.Process, bool, error)

func (f recorderFunc) RecordProcess(ctx context.Context, request state.ProcessObservation) (state.Process, bool, error) {
	return f(ctx, request)
}

func processHistory(t *testing.T) (*sqlite.Store, string, state.ProcessObservation) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	op, _, err := store.Accept(ctx, state.Submission{Scope: "project-a", IdempotencyKey: "deploy", Kind: "deploy", Intent: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, state.OperationUpdate{Scope: op.Scope, OperationID: op.ID, IdempotencyKey: "run", ExpectedVersion: 1, Status: state.OperationRunning}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AdmitGeneration(ctx, state.GenerationRequest{Scope: op.Scope, SlotID: "api/0", ID: "generation-a", OperationID: op.ID, IdempotencyKey: "admit", Intent: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	return store, path, state.ProcessObservation{Scope: op.Scope, SlotID: "api/0", GenerationID: "generation-a", IdempotencyKey: "owner-session-do-not-log", Identity: []byte("exact-runtime-reference"), StartedAt: time.Now().UTC()}
}

func reporter(t *testing.T, writer supervision.ProcessRecorder, request state.ProcessObservation) *supervision.ProcessReporter {
	t.Helper()
	r, err := supervision.NewProcessReporter(writer, request, supervision.ReporterOptions{AttemptTimeout: time.Second, RetryInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestProcessReporterAcknowledgesCommittedStickyHistoryOutsideCallbacks(t *testing.T) {
	store, path, request := processHistory(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	writer := recorderFunc(func(ctx context.Context, write state.ProcessObservation) (state.Process, bool, error) {
		calls.Add(1)
		if write.EverReady {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return state.Process{}, false, ctx.Err()
			}
		}
		return store.RecordProcess(ctx, write)
	})
	r := reporter(t, writer, request)
	// Construction freezes the selected process, including caller-owned bytes.
	request.Identity[0] = '!'
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetProcess(ctx, request.Scope, request.SlotID, request.GenerationID); err != nil || got.EverReady || bytes.Equal(got.Identity, request.Identity) {
		t.Fatalf("initial binding: %#v %v", got, err)
	}
	r.Observe(probe.Readiness{Ready: true, EverReady: true})
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("ready write did not start")
	}
	var observers sync.WaitGroup
	for i := 0; i < 100; i++ {
		observers.Add(1)
		go func() {
			defer observers.Done()
			r.Observe(probe.Readiness{})
			r.Observe(probe.Readiness{EverReady: true})
		}()
	}
	callbacksDone := make(chan struct{})
	go func() { observers.Wait(); close(callbacksDone) }()
	select {
	case <-callbacksDone:
	case <-ctx.Done():
		t.Fatal("callback blocked behind storage I/O")
	}
	limited, stopFlush := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stopFlush()
	if err := r.Flush(limited); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("uncommitted fact acknowledged: %v", err)
	}
	if got, err := store.GetProcess(ctx, request.Scope, request.SlotID, request.GenerationID); err != nil || got.EverReady {
		t.Fatalf("blocked write changed history: %#v %v", got, err)
	}
	close(release)
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("duplicate callbacks created redundant writes: %d", calls.Load())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.GetProcess(context.Background(), request.Scope, request.SlotID, request.GenerationID)
	if err != nil || !got.EverReady || got.Terminal != nil {
		t.Fatalf("acknowledged history lost on reopen: %#v %v", got, err)
	}
}

func TestProcessReporterReplaysIdenticalRequestAfterLostResponse(t *testing.T) {
	for _, lostReady := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial binding", true: "first readiness"}[lostReady], func(t *testing.T) {
			store, _, request := processHistory(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lost := errors.New("committed response lost")
			var previous state.ProcessObservation
			var calls atomic.Int32
			lostOnce := false
			writer := recorderFunc(func(ctx context.Context, write state.ProcessObservation) (state.Process, bool, error) {
				got, recorded, err := store.RecordProcess(ctx, write)
				if err != nil {
					return got, recorded, err
				}
				calls.Add(1)
				if write.EverReady == lostReady && !lostOnce {
					lostOnce = true
					previous = write
					return state.Process{}, false, lost
				}
				if write.EverReady == lostReady && (write.IdempotencyKey != previous.IdempotencyKey || write.EverReady != previous.EverReady || !bytes.Equal(write.Identity, previous.Identity) || !write.StartedAt.Equal(previous.StartedAt) || recorded) {
					t.Error("ambiguous write was not replayed exactly")
				}
				return got, recorded, nil
			})
			r := reporter(t, writer, request)
			r.Observe(probe.Readiness{EverReady: true})
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx) }()
			if err := r.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if calls.Load() != 3 {
				t.Fatalf("lost acknowledgement was not recovered: %d", calls.Load())
			}
		})
	}
}

func TestProcessReporterPreservesEarlyFactsAndReplaysItsSession(t *testing.T) {
	for _, seed := range []bool{false, true} {
		t.Run(map[bool]string{false: "early observation", true: "verified initial history"}[seed], func(t *testing.T) {
			store, _, request := processHistory(t)
			request.EverReady = seed
			for _, replay := range []bool{false, true} {
				var writes []state.ProcessObservation
				writer := recorderFunc(func(ctx context.Context, write state.ProcessObservation) (state.Process, bool, error) {
					writes = append(writes, write)
					got, recorded, err := store.RecordProcess(ctx, write)
					if err == nil && recorded == replay {
						t.Error("session did not preserve receipt replay semantics")
					}
					return got, recorded, err
				})
				r := reporter(t, writer, request)
				r.Observe(probe.Readiness{EverReady: true})
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				done := make(chan error, 1)
				go func() { done <- r.Run(ctx) }()
				err := r.Flush(ctx)
				cancel()
				<-done
				if err != nil {
					t.Fatal(err)
				}
				want := 2
				if seed {
					want = 1
				}
				if len(writes) != want || writes[0].EverReady != seed || !writes[len(writes)-1].EverReady || writes[0].IdempotencyKey == request.IdempotencyKey {
					t.Fatalf("initial fact/key changed or early readiness lost: %#v", writes)
				}
			}
		})
	}
}

func TestProcessReporterFlushTimeoutRetainsLastStorageFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failure := errors.New("temporary storage failure")
		request := state.ProcessObservation{Scope: "scope", SlotID: "slot", GenerationID: "generation", IdempotencyKey: "session", Identity: []byte("identity"), StartedAt: time.Now()}
		writer := recorderFunc(func(context.Context, state.ProcessObservation) (state.Process, bool, error) {
			return state.Process{}, false, failure
		})
		r, err := supervision.NewProcessReporter(writer, request, supervision.ReporterOptions{AttemptTimeout: time.Second, RetryInterval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		synctest.Wait()
		limited, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		if err := r.Flush(limited); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, failure) {
			t.Fatalf("flush hid storage failure: %v", err)
		}
		cancel()
		<-done
	})
}

func TestProcessReporterCanceledAcknowledgementCanRecoverWithExactSession(t *testing.T) {
	for _, mode := range []string{"before write", "after commit"} {
		t.Run(mode, func(t *testing.T) {
			store, _, request := processHistory(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls int
			writer := recorderFunc(func(ctx context.Context, write state.ProcessObservation) (state.Process, bool, error) {
				calls++
				got, recorded, err := store.RecordProcess(ctx, write)
				cancel()
				return got, recorded, err
			})
			r := reporter(t, writer, request)
			if mode == "before write" {
				cancel()
			}
			if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled worker: %v", err)
			}
			if err := r.Flush(context.Background()); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled response acknowledged: %v", err)
			}
			if (mode == "before write" && calls != 0) || (mode == "after commit" && calls != 1) {
				t.Fatalf("canceled writes=%d", calls)
			}
			// Cancellation does not prove that the transaction was rolled back.
			r = reporter(t, store, request)
			retryCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			done := make(chan error, 1)
			go func() { done <- r.Run(retryCtx) }()
			if err := r.Flush(retryCtx); err != nil {
				t.Fatal(err)
			}
			stop()
			<-done
		})
	}
}

func TestProcessReporterStopsOnStateErrorsAndTerminalReceipts(t *testing.T) {
	for _, mode := range []string{"missing", "obsolete", "terminal replay"} {
		t.Run(mode, func(t *testing.T) {
			store, _, request := processHistory(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			want := state.ErrConflict
			if mode == "missing" {
				request.GenerationID = "missing"
				want = state.ErrNotFound
			}
			var initial state.ProcessObservation
			writer := recorderFunc(func(ctx context.Context, write state.ProcessObservation) (state.Process, bool, error) {
				initial = write
				return store.RecordProcess(ctx, write)
			})
			r := reporter(t, writer, request)
			if mode == "terminal replay" {
				workerCtx, stop := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() { done <- r.Run(workerCtx) }()
				if err := r.Flush(ctx); err != nil {
					t.Fatal(err)
				}
				stop()
				<-done
				if _, _, err := store.RecordProcessTerminal(ctx, state.ProcessTerminalObservation{Scope: request.Scope, SlotID: request.SlotID, GenerationID: request.GenerationID,
					IdempotencyKey: "terminal", Identity: request.Identity, StartedAt: request.StartedAt, Terminal: state.ProcessTerminal{Cause: state.ProcessStartupTimeout, OccurredAt: request.StartedAt.Add(time.Minute), Evidence: []byte(`{}`)}}); err != nil {
					t.Fatal(err)
				}
				if got, recorded, err := store.RecordProcess(ctx, initial); err != nil || recorded || got.Terminal == nil {
					t.Fatalf("control historical receipt: %#v %v %v", got, recorded, err)
				}
				r = reporter(t, store, request)
			} else if mode == "obsolete" {
				op, _, err := store.Accept(ctx, state.Submission{Scope: request.Scope, IdempotencyKey: "successor", Kind: "deploy", Intent: []byte(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.Update(ctx, state.OperationUpdate{Scope: op.Scope, OperationID: op.ID, IdempotencyKey: "run", ExpectedVersion: 1, Status: state.OperationRunning}); err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.AdmitGeneration(ctx, state.GenerationRequest{Scope: op.Scope, SlotID: request.SlotID, ID: "generation-b", OperationID: op.ID, IdempotencyKey: "admit-next", ExpectedVersion: 1, Intent: []byte(`{}`)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Run(ctx); !errors.Is(err, want) {
				t.Fatalf("worker lost permanent cause: %v", err)
			}
			if err := r.Flush(ctx); !errors.Is(err, want) {
				t.Fatalf("flush hid permanent failure: %v", err)
			}
		})
	}
}

func TestProcessReporterCancellationAndAttemptBounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		request := state.ProcessObservation{Scope: "scope", SlotID: "slot", GenerationID: "generation", IdempotencyKey: "session", Identity: []byte("identity"), StartedAt: time.Now()}
		var calls atomic.Int32
		writer := recorderFunc(func(ctx context.Context, _ state.ProcessObservation) (state.Process, bool, error) {
			calls.Add(1)
			<-ctx.Done()
			return state.Process{}, false, ctx.Err()
		})
		r, err := supervision.NewProcessReporter(writer, request, supervision.ReporterOptions{AttemptTimeout: time.Second, RetryInterval: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		done, flushed := make(chan error, 1), make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		go func() { flushed <- r.Flush(context.Background()) }()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("initial writes=%d", calls.Load())
		}
		if err := r.Run(ctx); err == nil {
			t.Fatal("worker ran twice")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("retry interval was skipped")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("attempt did not retry with its bound: %d", calls.Load())
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if err := <-flushed; !errors.Is(err, context.Canceled) {
			t.Fatalf("pending flush did not unblock: %v", err)
		}
		r.Observe(probe.Readiness{EverReady: true})
		if err := r.Flush(context.Background()); !errors.Is(err, context.Canceled) {
			t.Fatalf("ended worker acknowledged later observation: %v", err)
		}
	})
}

func TestProcessReporterBeforeRunRequiresAcknowledgementAndIgnoresTrafficReadiness(t *testing.T) {
	store, _, request := processHistory(t)
	r := reporter(t, store, request)
	r.Observe(probe.Readiness{Ready: true, StartupFailed: true})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.Flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("not-started worker acknowledged binding: %v", err)
	}
	workerCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- r.Run(workerCtx) }()
	if err := r.Flush(workerCtx); err != nil {
		t.Fatal(err)
	}
	stop()
	<-done
	got, err := store.GetProcess(context.Background(), request.Scope, request.SlotID, request.GenerationID)
	if err != nil || got.EverReady || got.Terminal != nil {
		t.Fatalf("traffic/startup flag invented history: %#v %v", got, err)
	}
}

func TestProcessReporterValidatesInputBeforeIO(t *testing.T) {
	for _, mode := range []string{"writer", "scope", "slot", "generation", "key", "identity", "start", "invalid time", "attempt", "retry"} {
		t.Run(mode, func(t *testing.T) {
			var writer supervision.ProcessRecorder = recorderFunc(func(context.Context, state.ProcessObservation) (state.Process, bool, error) {
				t.Error("constructor performed I/O")
				return state.Process{}, false, nil
			})
			request := state.ProcessObservation{Scope: "scope", SlotID: "slot", GenerationID: "generation", IdempotencyKey: "session", Identity: []byte("identity"), StartedAt: time.Now()}
			options := supervision.ReporterOptions{AttemptTimeout: time.Second, RetryInterval: time.Second}
			switch mode {
			case "writer":
				writer = nil
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
			case "attempt":
				options.AttemptTimeout = 0
			case "retry":
				options.RetryInterval = 0
			}
			if r, err := supervision.NewProcessReporter(writer, request, options); err == nil || r != nil {
				t.Fatalf("invalid reporter accepted: %#v %v", r, err)
			}
		})
	}
}
