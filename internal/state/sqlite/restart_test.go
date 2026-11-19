package sqlite_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

func restartAdmission(t *testing.T, store *sqlite.Store, id string) state.RestartAdmissionRequest {
	t.Helper()
	intent := []byte(`{"restart-successor":"` + id + `"}`)
	op, _, err := store.Accept(context.Background(), state.Submission{Scope: "project-a", IdempotencyKey: "restart-operation-" + id, Kind: state.RestartOperationKind, Intent: intent})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(context.Background(), progressUpdate(op, "start", state.OperationRunning, "")); err != nil {
		t.Fatal(err)
	}
	return state.RestartAdmissionRequest{GenerationRequest: state.GenerationRequest{Scope: op.Scope, SlotID: "api/0", ID: id, OperationID: op.ID, IdempotencyKey: "restart-admission-" + id, ExpectedVersion: 1, Intent: intent}, PreviousGenerationID: "generation-a"}
}

func restartSource(t *testing.T, store *sqlite.Store, mode string) state.ProcessObservation {
	t.Helper()
	initial := processObservation(t, store, "generation-a")
	initial.EverReady = mode == "unhealthy" || mode == "ready exit"
	if mode != "unbound" {
		if _, _, err := store.RecordProcess(context.Background(), initial); err != nil {
			t.Fatal(err)
		}
	}
	if mode != "unbound" && mode != "live" {
		cause := state.ProcessExited
		if mode == "startup" {
			cause = state.ProcessStartupTimeout
		}
		if mode == "unhealthy" {
			cause = state.ProcessUnhealthy
		}
		if _, _, err := store.RecordProcessTerminal(context.Background(), terminalObservation(initial, cause)); err != nil {
			t.Fatal(err)
		}
	}
	return initial
}

func TestRestartAdmissionAndCurrencySurviveReopenAndObsoleteReplay(t *testing.T) {
	for _, mode := range []string{"exit", "ready exit", "unhealthy"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "state.db")
			store := openStore(t, path)
			initial := restartSource(t, store, mode)
			old, err := store.GetGeneration(ctx, initial.Scope, initial.SlotID, initial.GenerationID)
			if err != nil {
				t.Fatal(err)
			}
			oldOp, err := store.Get(ctx, old.Scope, old.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Update(ctx, progressUpdate(oldOp, "finish", state.OperationSucceeded, `{}`)); err != nil {
				t.Fatal(err)
			}
			request := restartAdmission(t, store, "generation-b")
			got, created, err := store.AdmitRestart(ctx, request)
			if err != nil || !created || got.ID != request.ID || got.Version != 2 {
				t.Fatalf("restart admission: %#v %v %v", got, created, err)
			}
			if _, err := store.GetProcess(ctx, got.Scope, got.SlotID, got.ID); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("admission fabricated successor process history: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(path)
			if err != nil || bytes.Contains(contents, []byte(request.IdempotencyKey)) || bytes.Contains(contents, []byte("restart-operation-generation-b")) {
				t.Fatal("raw restart receipt key persisted or database unreadable")
			}
			store = openStore(t, path)
			current, err := store.GetCurrentRestart(ctx, request.Scope, request.SlotID, request.ID, request.OperationID)
			if err != nil || current.Generation.ID != request.ID || current.Previous.ID != initial.GenerationID || current.Process.Terminal == nil || current.Process.EverReady != initial.EverReady || current.Operation.Status != state.OperationRunning || !bytes.Equal(current.Operation.Intent, current.Generation.Intent) {
				t.Fatalf("restart recovery: %#v %v", current, err)
			}
			current.Process.Identity[0], current.Process.Terminal.Evidence[0], current.Generation.Intent[0], current.Operation.Intent[0] = '!', '!', '!', '!'
			current, err = store.GetCurrentRestart(ctx, request.Scope, request.SlotID, request.ID, request.OperationID)
			if err != nil || !bytes.Equal(current.Process.Identity, initial.Identity) || !bytes.Equal(current.Generation.Intent, request.Intent) {
				t.Fatal("caller mutation changed retained restart")
			}
			if _, _, err := store.Update(ctx, progressUpdate(current.Operation, "finish", state.OperationSucceeded, `{"created":"acknowledged"}`)); err != nil {
				t.Fatal(err)
			}
			if result, err := store.GetCurrentRestart(ctx, request.Scope, request.SlotID, request.ID, request.OperationID); !errors.Is(err, state.ErrConflict) || !reflect.DeepEqual(result, state.Restart{}) {
				t.Fatalf("completed operation revived: %#v %v", result, err)
			}
			later := generationRequest(t, store, "generation-c")
			later.ExpectedVersion = 2
			if _, _, err := store.AdmitGeneration(ctx, later); err != nil {
				t.Fatal(err)
			}
			got, created, err = store.AdmitRestart(ctx, request)
			if err != nil || created || got.ID != later.ID || got.Version != 3 {
				t.Fatalf("old receipt restored restart head: %#v %v %v", got, created, err)
			}
			if _, err := store.GetCurrentRestart(ctx, request.Scope, request.SlotID, request.ID, request.OperationID); !errors.Is(err, state.ErrConflict) {
				t.Fatalf("obsolete restart executable: %v", err)
			}
		})
	}
}

func TestRestartAdmissionRejectsInvalidSourceOperationAndIntent(t *testing.T) {
	for _, mode := range []string{"no predecessor", "wrong predecessor", "first generation", "live", "unbound", "startup", "wrong kind", "changed intent", "pending", "terminal", "stale version", "same generation", "missing key", "missing scope", "missing slot", "missing generation", "missing operation", "empty intent", "pre-canceled"} {
		t.Run(mode, func(t *testing.T) {
			store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
			restartSource(t, store, mode)
			request := restartAdmission(t, store, "generation-b")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "no predecessor":
				request.PreviousGenerationID = " "
			case "wrong predecessor":
				request.PreviousGenerationID = "other"
			case "first generation":
				request.ExpectedVersion = 0
			case "wrong kind", "pending":
				kind := state.RestartOperationKind
				if mode == "wrong kind" {
					kind = "deploy"
				}
				op, _, err := store.Accept(ctx, state.Submission{Scope: request.Scope, IdempotencyKey: "other-operation", Kind: kind, Intent: request.Intent})
				if err != nil {
					t.Fatal(err)
				}
				if mode == "wrong kind" {
					if _, _, err := store.Update(ctx, progressUpdate(op, "start", state.OperationRunning, "")); err != nil {
						t.Fatal(err)
					}
				}
				request.OperationID = op.ID
			case "terminal":
				op, err := store.Get(ctx, request.Scope, request.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.Update(ctx, progressUpdate(op, "cancel", state.OperationCanceled, `{}`)); err != nil {
					t.Fatal(err)
				}
			case "changed intent":
				request.Intent = []byte(`{"different":true}`)
			case "stale version":
				request.ExpectedVersion = 2
			case "same generation":
				request.ID = "generation-a"
			case "missing key":
				request.IdempotencyKey = ""
			case "missing scope":
				request.Scope = " "
			case "missing slot":
				request.SlotID = ""
			case "missing generation":
				request.ID = ""
			case "missing operation":
				request.OperationID = ""
			case "empty intent":
				request.Intent = nil
			case "pre-canceled":
				cancel()
			}
			got, created, err := store.AdmitRestart(ctx, request)
			if err == nil || created || got.ID != "" {
				t.Fatalf("invalid restart admitted: %#v %v %v", got, created, err)
			}
			head, err := store.CurrentGeneration(context.Background(), "project-a", "api/0")
			if err != nil || head.ID != "generation-a" || head.Version != 1 {
				t.Fatalf("rejection changed head: %#v %v", head, err)
			}
			if _, err := store.GetGeneration(context.Background(), "project-a", "api/0", "generation-b"); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("rejected successor retained: %v", err)
			}
		})
	}
}

func TestRestartAdmissionRejectsReceiptRebindingWithoutReservingRejectedKey(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
	restartSource(t, store, "exit")
	request := restartAdmission(t, store, "generation-b")
	wrong := request
	wrong.Intent = []byte(`{"changed":true}`)
	if _, _, err := store.AdmitRestart(context.Background(), wrong); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("intent mismatch accepted: %v", err)
	}
	if _, created, err := store.AdmitRestart(context.Background(), request); err != nil || !created {
		t.Fatalf("rejection reserved key: %v %v", created, err)
	}
	for _, mode := range []string{"predecessor", "identity", "operation", "intent", "base"} {
		t.Run(mode, func(t *testing.T) {
			changed := request
			switch mode {
			case "predecessor":
				changed.PreviousGenerationID = "other"
			case "identity":
				changed.ID = "other"
			case "operation":
				changed.OperationID = "other"
			case "intent":
				changed.Intent = []byte(`{"changed":true}`)
			case "base":
				changed.ExpectedVersion++
			}
			if got, created, err := store.AdmitRestart(context.Background(), changed); !errors.Is(err, state.ErrConflict) || created || got.ID != "" {
				t.Fatalf("restart receipt rebound: %#v %v %v", got, created, err)
			}
		})
	}
}

func TestCurrentRestartRejectsTerminalAndForeignSelections(t *testing.T) {
	for _, mode := range []string{"succeeded", "failed", "canceled", "wrong generation", "wrong operation", "wrong scope", "wrong slot", "missing generation", "pre-canceled"} {
		t.Run(mode, func(t *testing.T) {
			store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
			restartSource(t, store, "exit")
			request := restartAdmission(t, store, "generation-b")
			if _, _, err := store.AdmitRestart(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			scope, slot, generation, operation := request.Scope, request.SlotID, request.ID, request.OperationID
			switch mode {
			case "succeeded", "failed", "canceled":
				op, err := store.Get(ctx, scope, operation)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.Update(ctx, progressUpdate(op, "finish", state.OperationStatus(mode), `{}`)); err != nil {
					t.Fatal(err)
				}
			case "wrong generation":
				generation = "generation-a"
			case "wrong operation":
				operation = "other"
			case "wrong scope":
				scope = "other"
			case "wrong slot":
				slot = "other"
			case "missing generation":
				generation = ""
			case "pre-canceled":
				cancel()
			}
			got, err := store.GetCurrentRestart(ctx, scope, slot, generation, operation)
			if err == nil || !reflect.DeepEqual(got, state.Restart{}) {
				t.Fatalf("invalid restart selection executable: %#v %v", got, err)
			}
		})
	}
}

func TestCurrentRestartRevalidatesGenericallyAdmittedHistory(t *testing.T) {
	for _, mode := range []string{"wrong kind", "wrong intent", "live", "unbound", "startup"} {
		t.Run(mode, func(t *testing.T) {
			store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
			restartSource(t, store, mode)
			request := restartAdmission(t, store, "generation-b")
			if mode == "wrong kind" {
				request.GenerationRequest = generationRequest(t, store, "generation-b")
				request.ExpectedVersion = 1
			}
			if mode == "wrong intent" {
				request.Intent = []byte(`{"unrelated":true}`)
			}
			if _, _, err := store.AdmitGeneration(context.Background(), request.GenerationRequest); err != nil {
				t.Fatal(err)
			}
			if got, err := store.GetCurrentRestart(context.Background(), request.Scope, request.SlotID, request.ID, request.OperationID); err == nil || !reflect.DeepEqual(got, state.Restart{}) {
				t.Fatalf("generic admission bypassed restart validation: %#v %v", got, err)
			}
		})
	}
}

func TestCurrentRestartCannotRecreateAnAlreadyBoundSuccessor(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			ctx := context.Background()
			store := openStore(t, filepath.Join(t.TempDir(), "state.db"))
			initial := restartSource(t, store, "exit")
			request := restartAdmission(t, store, "generation-b")
			if _, _, err := store.AdmitRestart(ctx, request); err != nil {
				t.Fatal(err)
			}
			next := initial
			next.GenerationID, next.IdempotencyKey, next.Identity = request.ID, "bind-successor", []byte("successor-process")
			if _, _, err := store.RecordProcess(ctx, next); err != nil {
				t.Fatal(err)
			}
			if terminal {
				if _, _, err := store.RecordProcessTerminal(ctx, terminalObservation(next, state.ProcessExited)); err != nil {
					t.Fatal(err)
				}
			}
			// The restart operation is deliberately still running after recovery.
			if got, err := store.GetCurrentRestart(ctx, request.Scope, request.SlotID, request.ID, request.OperationID); !errors.Is(err, state.ErrConflict) || !reflect.DeepEqual(got, state.Restart{}) {
				t.Fatalf("bound successor still permitted creation replay: %#v %v", got, err)
			}
			if head, created, err := store.AdmitRestart(ctx, request); err != nil || created || head.ID != request.ID {
				t.Fatalf("history acknowledgement was lost: %#v %v %v", head, created, err)
			}
		})
	}
}

func TestRestartAdmissionSerializesIndependentHandles(t *testing.T) {
	for _, identical := range []bool{true, false} {
		t.Run(map[bool]string{true: "identical retries", false: "competing successors"}[identical], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			first := openStore(t, path)
			restartSource(t, first, "exit")
			second := openStore(t, path)
			base := restartAdmission(t, first, "generation-b")
			const callers = 8
			requests := make([]state.RestartAdmissionRequest, callers)
			for i := range callers {
				requests[i] = base
				if !identical {
					requests[i] = restartAdmission(t, first, fmt.Sprintf("generation-%d", i))
				}
			}
			type outcome struct {
				head    state.Generation
				created bool
				err     error
				request state.RestartAdmissionRequest
			}
			done := make(chan outcome, callers)
			var workers sync.WaitGroup
			for i, request := range requests {
				workers.Add(1)
				go func() {
					defer workers.Done()
					store := first
					if i%2 != 0 {
						store = second
					}
					head, created, err := store.AdmitRestart(context.Background(), request)
					done <- outcome{head, created, err, request}
				}()
			}
			workers.Wait()
			close(done)
			created := 0
			for got := range done {
				if got.created {
					created++
				}
				if identical && got.err != nil {
					t.Fatalf("identical retry failed: %v", got.err)
				}
				if !identical && !got.created && !errors.Is(got.err, state.ErrConflict) {
					t.Fatalf("competing admission unexpectedly accepted: %#v", got)
				}
				if got.created {
					if _, err := first.GetCurrentRestart(context.Background(), got.request.Scope, got.request.SlotID, got.head.ID, got.request.OperationID); err != nil {
						t.Fatalf("winner not current: %v", err)
					}
				}
			}
			if created != 1 {
				t.Fatalf("admitted %d successors", created)
			}
		})
	}
}
