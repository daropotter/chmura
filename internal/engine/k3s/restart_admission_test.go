package k3s_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAdmittedRestartRecoveryChecksCurrencyBeforeRuntime(t *testing.T) {
	for _, retiredBy := range []string{"completion", "supersession", "successor binding"} {
		t.Run(retiredBy, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			input := processRestartInput(t, "exit zero")
			prepared, err := k3s.PrepareProcessRestart(input)
			if err != nil {
				t.Fatal(err)
			}
			// The fixture owner defines its private opaque intent codec. Storage
			// neither knows this format nor interprets engine requests.
			payload, err := json.Marshal(prepared)
			if err != nil {
				t.Fatal(err)
			}
			envelope := struct {
				Request []byte `json:"fixture-request"`
			}{payload}
			intent, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "state.db")
			store, err := sqlite.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if store != nil {
					_ = store.Close()
				}
			})
			p := input.Process
			admitResumeGeneration(t, store, k3s.ResumeRequest{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID}, 0)
			if _, _, err := store.RecordProcess(ctx, state.ProcessObservation{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID, IdempotencyKey: "bind", Identity: p.Identity, StartedAt: p.StartedAt}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.RecordProcessTerminal(ctx, state.ProcessTerminalObservation{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID, IdempotencyKey: "terminal", Identity: p.Identity, StartedAt: p.StartedAt, Terminal: *p.Terminal}); err != nil {
				t.Fatal(err)
			}
			op, _, err := store.Accept(ctx, state.Submission{Scope: p.Scope, IdempotencyKey: "restart", Kind: state.RestartOperationKind, Intent: intent})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Update(ctx, state.OperationUpdate{Scope: p.Scope, OperationID: op.ID, IdempotencyKey: "start", ExpectedVersion: op.Version, Status: state.OperationRunning}); err != nil {
				t.Fatal(err)
			}
			admission := state.RestartAdmissionRequest{GenerationRequest: state.GenerationRequest{Scope: p.Scope, SlotID: p.SlotID, ID: "generation-b", OperationID: op.ID, IdempotencyKey: "admit", ExpectedVersion: 1, Intent: intent}, PreviousGenerationID: p.GenerationID}
			if _, _, err := store.AdmitRestart(ctx, admission); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = sqlite.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			selected, err := store.CurrentGeneration(ctx, p.Scope, p.SlotID)
			if err != nil {
				t.Fatal(err)
			}
			runtime := &lifetimeStore{}
			client, _, ref := lifetimeClient(t, runtime)
			runtime.mu.Lock()
			old := runtime.pods[ref.PodName]
			old.Status.Phase = corev1.PodSucceeded
			old.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ContainerID: ref.ContainerID, StartedAt: metav1.NewTime(p.StartedAt), FinishedAt: metav1.NewTime(p.Terminal.OccurredAt), Reason: "Completed",
			}}
			runtime.mu.Unlock()
			// One serialized owner: check the explicit selection before each step,
			// recover the request from durable intent and verify terminal evidence.
			step := func() (k3s.RestartResult, error) {
				current, err := store.GetCurrentRestart(ctx, selected.Scope, selected.SlotID, selected.ID, selected.OperationID)
				if err != nil {
					return k3s.RestartResult{}, err
				}
				var saved struct {
					Request []byte `json:"fixture-request"`
				}
				if err := json.Unmarshal(current.Operation.Intent, &saved); err != nil {
					return k3s.RestartResult{}, err
				}
				var request k3s.RestartRequest
				if err := json.Unmarshal(saved.Request, &request); err != nil {
					return k3s.RestartResult{}, err
				}
				checked, err := k3s.PrepareProcessRestart(k3s.ProcessRestartRequest{Process: current.Process, Instance: request.Instance, Previous: request.Previous, Next: request.Next})
				if err != nil {
					return k3s.RestartResult{}, err
				}
				return (k3s.Restarter{Client: client}).Ensure(ctx, checked)
			}
			pending, err := step()
			if err != nil || !pending.Retiring {
				t.Fatalf("recovered restart skipped retirement: %#v %v", pending, err)
			}
			runtime.mu.Lock()
			delete(runtime.pods, ref.PodName)
			runtime.dropCreateReply = true
			runtime.mu.Unlock()
			_, _ = step()
			created, err := step()
			if err != nil || created.Retiring || created.Instance.Created || created.Instance.Identity.PodUID == "" {
				t.Fatalf("recovered creation replay failed: %#v %v", created, err)
			}
			if retiredBy == "completion" {
				current, err := store.Get(ctx, selected.Scope, selected.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				result, _ := json.Marshal(map[string]string{"pod-uid": created.Instance.Identity.PodUID})
				if _, _, err := store.Update(ctx, state.OperationUpdate{Scope: current.Scope, OperationID: current.ID, ExpectedVersion: current.Version, IdempotencyKey: "finish", Status: state.OperationSucceeded, Result: result}); err != nil {
					t.Fatal(err)
				}
			} else if retiredBy == "supersession" {
				admitResumeGeneration(t, store, k3s.ResumeRequest{Scope: selected.Scope, SlotID: selected.SlotID, GenerationID: "generation-c"}, 2)
			} else {
				identity, err := k3s.EncodeProcessIdentity(k3s.Instance{Namespace: created.Instance.Identity.Namespace, PodName: created.Instance.Identity.PodName, PodUID: created.Instance.Identity.PodUID, ContainerName: created.Instance.Identity.ContainerName, ContainerID: "containerd://successor"})
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.RecordProcess(ctx, state.ProcessObservation{Scope: selected.Scope, SlotID: selected.SlotID, GenerationID: selected.ID, IdempotencyKey: "bind-successor", Identity: identity, StartedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			runtime.mu.Lock()
			delete(runtime.pods, created.Instance.Identity.PodName)
			requests, creates := runtime.requests, runtime.creates
			runtime.mu.Unlock()
			if _, err := step(); !errors.Is(err, state.ErrConflict) {
				t.Fatalf("obsolete restart was not rejected: %v", err)
			}
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			if runtime.requests != requests || runtime.creates != creates || creates != 1 || len(runtime.pods) != 0 {
				t.Fatal("obsolete operation contacted runtime or recreated a deleted successor")
			}
		})
	}
}
