package k3s_test

import (
	"bytes"
	"context"
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

func restartAdmissionDriverFixture(t *testing.T, mode string) (string, *sqlite.Store, k3s.ProcessRestartAdmission) {
	t.Helper()
	inputMode := mode
	if mode == "live" {
		inputMode = "exit zero"
	}
	input := processRestartInput(t, inputMode)
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p := input.Process
	admitResumeGeneration(t, store, k3s.ResumeRequest{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID}, 0)
	if _, _, err := store.RecordProcess(context.Background(), state.ProcessObservation{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID,
		Identity: p.Identity, StartedAt: p.StartedAt, EverReady: p.EverReady, IdempotencyKey: "bind"}); err != nil {
		t.Fatal(err)
	}
	if mode != "live" {
		if _, _, err := store.RecordProcessTerminal(context.Background(), state.ProcessTerminalObservation{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID,
			Identity: p.Identity, StartedAt: p.StartedAt, Terminal: *p.Terminal, IdempotencyKey: "terminal"}); err != nil {
			t.Fatal(err)
		}
	}
	return path, store, k3s.ProcessRestartAdmission{Scope: p.Scope, SlotID: p.SlotID, PreviousGenerationID: p.GenerationID,
		GenerationID: "generation-b", PreviousVersion: 1, IdempotencyKey: "automatic-restart", Previous: input.Previous, Next: input.Next}
}

type restartAdmissionDriverStore struct {
	*sqlite.Store
	accept func(context.Context, state.Submission) (state.Operation, bool, error)
	update func(context.Context, state.OperationUpdate) (state.Operation, bool, error)
	admit  func(context.Context, state.RestartAdmissionRequest) (state.Generation, bool, error)
}

func (s restartAdmissionDriverStore) Accept(ctx context.Context, r state.Submission) (state.Operation, bool, error) {
	if s.accept != nil {
		return s.accept(ctx, r)
	}
	return s.Store.Accept(ctx, r)
}
func (s restartAdmissionDriverStore) Update(ctx context.Context, r state.OperationUpdate) (state.Operation, bool, error) {
	if s.update != nil {
		return s.update(ctx, r)
	}
	return s.Store.Update(ctx, r)
}
func (s restartAdmissionDriverStore) AdmitRestart(ctx context.Context, r state.RestartAdmissionRequest) (state.Generation, bool, error) {
	if s.admit != nil {
		return s.admit(ctx, r)
	}
	return s.Store.AdmitRestart(ctx, r)
}

func assertAdmittedRestart(t *testing.T, store *sqlite.Store, request k3s.ProcessRestartAdmission, selection k3s.RestartSelection) {
	t.Helper()
	if selection.Scope != request.Scope || selection.SlotID != request.SlotID || selection.GenerationID != request.GenerationID || selection.OperationID == "" {
		t.Fatalf("admission lost explicit selection: %#v", selection)
	}
	current, err := store.GetCurrentRestart(context.Background(), selection.Scope, selection.SlotID, selection.GenerationID, selection.OperationID)
	if err != nil || current.Generation.Version != request.PreviousVersion+1 || current.Previous.ID != request.PreviousGenerationID || current.Operation.Status != state.OperationRunning || !bytes.Equal(current.Generation.Intent, current.Operation.Intent) {
		t.Fatalf("admission not durably executable: %#v %v", current, err)
	}
	op, created, err := store.Accept(context.Background(), state.Submission{Scope: request.Scope, IdempotencyKey: request.IdempotencyKey, Kind: state.RestartOperationKind, Intent: current.Operation.Intent})
	if err != nil || created || op.ID != selection.OperationID {
		t.Fatalf("retry did not retain one acceptance: %#v %v", op, err)
	}
}

func TestAdmitProcessRestartRecoversHistoryAndAcknowledgesOneSuccessor(t *testing.T) {
	for _, mode := range []string{"exit zero", "unhealthy"} {
		t.Run(mode, func(t *testing.T) {
			path, store, request := restartAdmissionDriverFixture(t, mode)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := sqlite.Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			selection, err := k3s.AdmitProcessRestart(context.Background(), store, request)
			if err != nil {
				t.Fatal(err)
			}
			assertAdmittedRestart(t, store, request, selection)
			replayed, err := k3s.AdmitProcessRestart(context.Background(), store, request)
			if err != nil || replayed != selection {
				t.Fatalf("exact admission replay changed selection: %#v %v", replayed, err)
			}
			assertAdmittedRestart(t, store, request, replayed)
		})
	}
}

func TestAdmitProcessRestartRecoversEachAmbiguousWrite(t *testing.T) {
	for _, stage := range []string{"accept", "start", "admit"} {
		for _, mode := range []string{"before commit", "lost reply", "canceled after commit"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				path, store, request := restartAdmissionDriverFixture(t, "exit zero")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				lost := errors.New("admission acknowledgement unavailable")
				journal := restartAdmissionDriverStore{Store: store}
				journal.accept = func(ctx context.Context, r state.Submission) (state.Operation, bool, error) {
					if stage == "accept" && mode == "before commit" {
						return state.Operation{}, false, lost
					}
					op, changed, err := store.Accept(ctx, r)
					if err == nil && stage == "accept" {
						if mode == "canceled after commit" {
							cancel()
						} else {
							return state.Operation{}, false, lost
						}
					}
					return op, changed, err
				}
				journal.update = func(ctx context.Context, r state.OperationUpdate) (state.Operation, bool, error) {
					if stage == "start" && mode == "before commit" {
						return state.Operation{}, false, lost
					}
					op, changed, err := store.Update(ctx, r)
					if err == nil && stage == "start" {
						if mode == "canceled after commit" {
							cancel()
						} else {
							return state.Operation{}, false, lost
						}
					}
					return op, changed, err
				}
				journal.admit = func(ctx context.Context, r state.RestartAdmissionRequest) (state.Generation, bool, error) {
					if stage == "admit" && mode == "before commit" {
						return state.Generation{}, false, lost
					}
					g, changed, err := store.AdmitRestart(ctx, r)
					if err == nil && stage == "admit" {
						if mode == "canceled after commit" {
							cancel()
						} else {
							return state.Generation{}, false, lost
						}
					}
					return g, changed, err
				}
				selection, err := k3s.AdmitProcessRestart(ctx, journal, request)
				if selection != (k3s.RestartSelection{}) || err == nil {
					t.Fatalf("unacknowledged admission exposed: %#v %v", selection, err)
				}
				if mode == "canceled after commit" {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation lost: %v", err)
					}
				} else if !errors.Is(err, lost) {
					t.Fatalf("storage cause lost: %v", err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				store, err = sqlite.Open(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				selection, err = k3s.AdmitProcessRestart(context.Background(), store, request)
				if err != nil {
					t.Fatal(err)
				}
				assertAdmittedRestart(t, store, request, selection)
			})
		}
	}
}

func TestAdmitProcessRestartValidatesBeforeAcceptance(t *testing.T) {
	for _, mode := range []string{"live", "startup", "scope", "slot", "previous generation", "previous version", "same generation", "empty generation", "empty key", "changed declaration", "wrong previous key", "same creation key", "pre-canceled"} {
		t.Run(mode, func(t *testing.T) {
			fixtureMode := "exit zero"
			if mode == "live" || mode == "startup" {
				fixtureMode = mode
			}
			_, store, request := restartAdmissionDriverFixture(t, fixtureMode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "scope":
				request.Scope = "other"
			case "slot":
				request.SlotID = "other"
			case "previous generation":
				request.PreviousGenerationID = "other"
			case "previous version":
				request.PreviousVersion++
			case "same generation":
				request.GenerationID = request.PreviousGenerationID
			case "empty generation":
				request.GenerationID = ""
			case "empty key":
				request.IdempotencyKey = ""
			case "changed declaration":
				request.Next.Plan.Health.Ready.Successes++
			case "wrong previous key":
				request.Previous.IdempotencyKey = "other"
			case "same creation key":
				request.Next.IdempotencyKey = request.Previous.IdempotencyKey
			case "pre-canceled":
				cancel()
			}
			accepts := 0
			journal := restartAdmissionDriverStore{Store: store, accept: func(ctx context.Context, r state.Submission) (state.Operation, bool, error) {
				accepts++
				return store.Accept(ctx, r)
			}}
			selection, err := k3s.AdmitProcessRestart(ctx, journal, request)
			if err == nil || selection != (k3s.RestartSelection{}) || accepts != 0 {
				t.Fatalf("invalid request reached acceptance: %#v %v accepts=%d", selection, err, accepts)
			}
		})
	}
}

func TestAdmitProcessRestartRejectsMalformedAcknowledgements(t *testing.T) {
	for _, stage := range []string{"accept", "start", "admit"} {
		for _, field := range []string{"scope", "identity", "intent", "status or version"} {
			t.Run(stage+"/"+field, func(t *testing.T) {
				_, store, request := restartAdmissionDriverFixture(t, "exit zero")
				mutate := func(op *state.Operation) {
					switch field {
					case "scope":
						op.Scope = "other"
					case "identity":
						op.ID = ""
					case "intent":
						op.Intent = []byte("other")
					case "status or version":
						op.Status = state.OperationCanceled
					}
				}
				journal := restartAdmissionDriverStore{Store: store,
					accept: func(ctx context.Context, r state.Submission) (state.Operation, bool, error) {
						op, c, err := store.Accept(ctx, r)
						if stage == "accept" && err == nil {
							mutate(&op)
						}
						return op, c, err
					},
					update: func(ctx context.Context, r state.OperationUpdate) (state.Operation, bool, error) {
						op, c, err := store.Update(ctx, r)
						if stage == "start" && err == nil {
							mutate(&op)
						}
						return op, c, err
					},
					admit: func(ctx context.Context, r state.RestartAdmissionRequest) (state.Generation, bool, error) {
						g, c, err := store.AdmitRestart(ctx, r)
						if stage == "admit" && err == nil {
							switch field {
							case "scope":
								g.Scope = "other"
							case "identity":
								g.ID = "other"
							case "intent":
								g.Intent = []byte("other")
							case "status or version":
								g.Version++
							}
						}
						return g, c, err
					},
				}
				selection, err := k3s.AdmitProcessRestart(context.Background(), journal, request)
				if err == nil || selection != (k3s.RestartSelection{}) {
					t.Fatalf("malformed acknowledgement exposed: %#v %v", selection, err)
				}
			})
		}
	}
}

func TestAdmitProcessRestartReceiptSurvivesCompletionAndRejectsSupersession(t *testing.T) {
	_, store, request := restartAdmissionDriverFixture(t, "exit zero")
	ctx := context.Background()
	selection, err := k3s.AdmitProcessRestart(ctx, store, request)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &lifetimeStore{}
	client, _, _ := lifetimeClient(t, runtime)
	runtime.mu.Lock()
	clear(runtime.pods)
	runtime.dropCreateReply = true
	runtime.mu.Unlock()
	_, _ = (k3s.Restarter{Client: client}).EnsureAdmitted(ctx, store, selection)
	created, err := (k3s.Restarter{Client: client}).EnsureAdmitted(ctx, store, selection)
	if err != nil || created.Identity.PodUID == "" {
		t.Fatalf("admission did not feed existing executor: %#v %v", created, err)
	}
	replayed, err := k3s.AdmitProcessRestart(ctx, store, request)
	if err != nil || replayed != selection {
		t.Fatalf("completed admission receipt not recovered: %#v %v", replayed, err)
	}
	changed := request
	changed.Next.CPUMilli++
	if result, err := k3s.AdmitProcessRestart(ctx, store, changed); !errors.Is(err, state.ErrConflict) || result != (k3s.RestartSelection{}) {
		t.Fatalf("accepted key rebound to changed intent: %#v %v", result, err)
	}
	// A second complete cycle in the same slot needs a distinct admission
	// receipt. Discover the first successor's confirmed early exit and let the
	// existing durable supervisor close it before admitting its own successor.
	pod, _, _ := runtime.snapshot(created.Identity.PodName)
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", ContainerID: "containerd://generation-b", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ContainerID: "containerd://generation-b", StartedAt: metav1.NewTime(time.Now().Add(-time.Second)), FinishedAt: metav1.Now(), Reason: "Completed"}}}}
	runtime.mu.Lock()
	runtime.pods[pod.Name] = pod
	runtime.mu.Unlock()
	resume, err := (k3s.Resolver{Client: client}).BindRestartSuccessor(ctx, store, selection)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).ResumeDurable(ctx, store, cycleRequest(resume), cycleOptions())
	if err != nil || !errors.Is(outcome.Outcome, k3s.ErrInstanceExited) {
		t.Fatalf("successor terminal handoff failed: %#v %v", outcome, err)
	}
	next := request
	next.PreviousGenerationID, next.GenerationID, next.PreviousVersion = request.GenerationID, "generation-c", 2
	next.IdempotencyKey = "automatic-restart-c"
	next.Previous, next.Next = request.Next, request.Next
	next.Next.IdempotencyKey = "third-generation"
	second, err := k3s.AdmitProcessRestart(ctx, store, next)
	if err != nil || second.OperationID == selection.OperationID {
		t.Fatalf("second restart collided with the first receipt: %#v %v", second, err)
	}
	assertAdmittedRestart(t, store, next, second)
	if result, err := k3s.AdmitProcessRestart(ctx, store, request); !errors.Is(err, state.ErrConflict) || result != (k3s.RestartSelection{}) {
		t.Fatalf("obsolete receipt restored a selection: %#v %v", result, err)
	}
	head, err := store.CurrentGeneration(ctx, request.Scope, request.SlotID)
	if err != nil || head.ID != "generation-c" || head.Version != 3 {
		t.Fatalf("historical retry changed head: %#v %v", head, err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.creates != 1 {
		t.Fatalf("completion replay duplicated creation: %d", runtime.creates)
	}
}

func TestAdmitProcessRestartDoesNotResumeFailedOrCanceledOperations(t *testing.T) {
	for _, status := range []state.OperationStatus{state.OperationFailed, state.OperationCanceled} {
		t.Run(string(status), func(t *testing.T) {
			_, store, request := restartAdmissionDriverFixture(t, "exit zero")
			ctx := context.Background()
			selection, err := k3s.AdmitProcessRestart(ctx, store, request)
			if err != nil {
				t.Fatal(err)
			}
			op, err := store.Get(ctx, selection.Scope, selection.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			closed, _, err := store.Update(ctx, state.OperationUpdate{Scope: op.Scope, OperationID: op.ID, ExpectedVersion: op.Version,
				IdempotencyKey: "stop", Status: status, Result: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if result, err := k3s.AdmitProcessRestart(ctx, store, request); !errors.Is(err, state.ErrConflict) || result != (k3s.RestartSelection{}) {
				t.Fatalf("terminal operation resumed: %#v %v", result, err)
			}
			after, err := store.Get(ctx, op.Scope, op.ID)
			if err != nil || after.Status != status || after.Version != closed.Version || !bytes.Equal(after.Result, closed.Result) {
				t.Fatalf("terminal operation was rewritten: %#v %v", after, err)
			}
		})
	}
}
