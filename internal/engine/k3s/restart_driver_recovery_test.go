package k3s_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

type restartReadStore struct {
	*sqlite.Store
	operation  func(*state.Operation)
	generation func(*state.Generation)
	current    func(*state.Restart)
	failureAt  string
	failure    error
}

func (s restartReadStore) Get(ctx context.Context, scope, id string) (state.Operation, error) {
	if s.failureAt == "operation" {
		return state.Operation{}, s.failure
	}
	op, err := s.Store.Get(ctx, scope, id)
	if err == nil && s.operation != nil {
		s.operation(&op)
	}
	return op, err
}

func (s restartReadStore) GetGeneration(ctx context.Context, scope, slot, id string) (state.Generation, error) {
	if s.failureAt == "generation" {
		return state.Generation{}, s.failure
	}
	generation, err := s.Store.GetGeneration(ctx, scope, slot, id)
	if err == nil && s.generation != nil {
		s.generation(&generation)
	}
	return generation, err
}

func (s restartReadStore) GetCurrentRestart(ctx context.Context, scope, slot, generation, operation string) (state.Restart, error) {
	if s.failureAt == "currency" {
		return state.Restart{}, s.failure
	}
	current, err := s.Store.GetCurrentRestart(ctx, scope, slot, generation, operation)
	if err == nil && s.current != nil {
		s.current(&current)
	}
	return current, err
}

func TestRestartDriverRejectsInconsistentSnapshotsWithoutRuntime(t *testing.T) {
	for _, mode := range []string{"operation", "kind", "association", "intent", "previous id", "previous version", "process selection", "identity version", "identity duplicate", "identity noncanonical", "evidence", "current version", "current status"} {
		t.Run(mode, func(t *testing.T) {
			_, store, runtime, client, selection := restartDriverFixture(t, "exit zero", nil)
			journal := restartReadStore{Store: store,
				operation: func(op *state.Operation) {
					switch mode {
					case "operation":
						op.ID = "other"
					case "kind":
						op.Kind = "deploy"
					}
				},
				generation: func(g *state.Generation) {
					switch mode {
					case "association":
						g.OperationID = "other"
					case "intent":
						g.Intent = []byte("different")
					}
				},
				current: func(c *state.Restart) {
					switch mode {
					case "previous id":
						c.Previous.ID = "other"
					case "previous version":
						c.Previous.Version++
					case "process selection":
						c.Process.GenerationID = "other"
					case "identity version":
						c.Process.Identity = bytes.Replace(c.Process.Identity, []byte(`"version":1`), []byte(`"version":2`), 1)
					case "identity duplicate":
						c.Process.Identity = bytes.Replace(c.Process.Identity, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
					case "identity noncanonical":
						c.Process.Identity = append([]byte(" "), c.Process.Identity...)
					case "evidence":
						c.Process.Terminal.Evidence = []byte(`{}`)
					case "current version":
						c.Operation.Version++
					case "current status":
						c.Operation.Status = state.OperationCanceled
					}
				},
			}
			result, err := (k3s.Restarter{Client: client}).EnsureAdmitted(context.Background(), journal, selection)
			if err == nil || result != (k3s.RestartProgress{}) {
				t.Fatalf("inconsistent history executed: %#v %v", result, err)
			}
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			if runtime.requests != 0 {
				t.Fatal("inconsistent history contacted runtime")
			}
		})
	}
}

func TestRestartDriverPreservesReadFailures(t *testing.T) {
	for _, at := range []string{"operation", "generation", "currency"} {
		t.Run(at, func(t *testing.T) {
			_, store, runtime, client, selection := restartDriverFixture(t, "exit zero", nil)
			failure := errors.New("history unavailable")
			journal := restartReadStore{Store: store, failureAt: at, failure: failure}
			result, err := (k3s.Restarter{Client: client}).EnsureAdmitted(context.Background(), journal, selection)
			if !errors.Is(err, failure) || result != (k3s.RestartProgress{}) {
				t.Fatalf("read cause/result lost: %#v %v", result, err)
			}
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			if runtime.requests != 0 {
				t.Fatal("failed history read contacted runtime")
			}
		})
	}
}

func TestRestartDriverRejectsInvalidCompletionAcknowledgement(t *testing.T) {
	for _, mode := range []string{"operation", "scope", "kind", "intent", "version", "status", "result", "canceled after commit"} {
		t.Run(mode, func(t *testing.T) {
			_, store, runtime, client, selection := restartDriverFixture(t, "exit zero", nil)
			runtime.mu.Lock()
			clear(runtime.pods)
			runtime.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			journal := restartResultStore{Store: store, update: func(ctx context.Context, request state.OperationUpdate) (state.Operation, bool, error) {
				op, changed, err := store.Update(ctx, request)
				if err != nil {
					return op, changed, err
				}
				switch mode {
				case "operation":
					op.ID = "other"
				case "scope":
					op.Scope = "other"
				case "kind":
					op.Kind = "deploy"
				case "intent":
					op.Intent = []byte("different")
				case "version":
					op.Version--
				case "status":
					op.Status = state.OperationRunning
				case "result":
					op.Result = []byte(`{}`)
				case "canceled after commit":
					cancel()
				}
				return op, changed, nil
			}}
			result, err := (k3s.Restarter{Client: client}).EnsureAdmitted(ctx, journal, selection)
			if err == nil || result != (k3s.RestartProgress{}) {
				t.Fatalf("invalid acknowledgement exposed: %#v %v", result, err)
			}
			if mode == "canceled after commit" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if result, err := (k3s.Restarter{}).EnsureAdmitted(context.Background(), store, selection); err != nil || result.Identity.PodUID == "" {
				t.Fatalf("committed completion not recovered: %#v %v", result, err)
			}
		})
	}
}

func TestRestartDriverRejectsCorruptCompletedResultWithoutRuntime(t *testing.T) {
	_, store, runtime, client, selection := restartDriverFixture(t, "exit zero", nil)
	runtime.mu.Lock()
	clear(runtime.pods)
	runtime.mu.Unlock()
	if _, err := (k3s.Restarter{Client: client}).EnsureAdmitted(context.Background(), store, selection); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"version", "engine", "intent hash", "namespace", "name", "uid", "container", "unknown", "duplicate", "noncanonical", "broken"} {
		t.Run(mode, func(t *testing.T) {
			journal := restartReadStore{Store: store, operation: func(op *state.Operation) {
				var key string
				var value any
				switch mode {
				case "version":
					key, value = "version", 2
				case "engine":
					key, value = "engine", "other"
				case "intent hash":
					key, value = "intent-sha256", strings.Repeat("0", 64)
				case "namespace":
					key, value = "namespace", "other"
				case "name":
					key, value = "pod-name", "other"
				case "uid":
					key, value = "pod-uid", ""
				case "container":
					key, value = "container-name", "other"
				case "unknown":
					op.Result = bytes.Replace(op.Result, []byte(`"version":1`), []byte(`"version":1,"unknown":true`), 1)
					return
				case "duplicate":
					op.Result = bytes.Replace(op.Result, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
					return
				case "noncanonical":
					op.Result = append([]byte(" "), op.Result...)
					return
				case "broken":
					op.Result = []byte("{")
					return
				}
				// Retain canonical field order so the semantic mutations, rather
				// than an unrelated representation change, cause rejection.
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(op.Result, &fields); err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(value)
				op.Result = bytes.Replace(op.Result, append([]byte(`"`+key+`":`), fields[key]...), append([]byte(`"`+key+`":`), encoded...), 1)
			}}
			result, err := (k3s.Restarter{}).EnsureAdmitted(context.Background(), journal, selection)
			if err == nil || result != (k3s.RestartProgress{}) {
				t.Fatalf("corrupt result replayed: %#v %v", result, err)
			}
		})
	}
}

func TestRestartOperationIntentV1Compatibility(t *testing.T) {
	input := processRestartInput(t, "exit zero")
	intent, err := k3s.PrepareRestartOperationIntent(input, "generation-b", 1)
	if err != nil {
		t.Fatal(err)
	}
	// This fingerprint freezes private recovery bytes, not display formatting.
	if got := fmt.Sprintf("%x", sha256.Sum256(intent)); got != "5ac5bca1adb369a44a5fed5d23e8a91164ea825a7fbc70d4f78cda791f8e0459" {
		t.Fatalf("v1 recovery encoding changed: %s\n%s", got, intent)
	}
	if result, err := k3s.PrepareRestartOperationIntent(input, "generation-b", math.MaxInt64); err == nil || len(result) != 0 {
		t.Fatal("unadvanceable predecessor version encoded")
	}
}
