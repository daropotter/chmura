package k3s_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

func restartSuccessorFixture(t *testing.T, mode string) (string, *sqlite.Store, *lifetimeStore, *coreclient.CoreV1Client, k3s.RestartSelection, *corev1.Pod) {
	t.Helper()
	path, store, runtime, client, selection := restartDriverFixture(t, mode, nil)
	runtime.mu.Lock()
	clear(runtime.pods) // Confirm the old resource's absence.
	runtime.mu.Unlock()
	created, err := (k3s.Restarter{Client: client}).EnsureAdmitted(context.Background(), store, selection)
	if err != nil {
		t.Fatal(err)
	}
	pod, _, _ := runtime.snapshot(created.Identity.PodName)
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	pod.Status.Phase, pod.Status.PodIP = corev1.PodRunning, "127.0.0.1"
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", ContainerID: "containerd://successor", Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Second).Truncate(time.Second))}}}}
	runtime.mu.Lock()
	runtime.pods[pod.Name] = pod.DeepCopy()
	runtime.mu.Unlock()
	return path, store, runtime, client, selection, pod
}

func assertNoSuccessorResume(t *testing.T, request k3s.ResumeRequest, err error) {
	t.Helper()
	if err == nil || !reflect.DeepEqual(request, k3s.ResumeRequest{}) {
		t.Fatalf("unsafe supervision request exposed: %#v %v", request, err)
	}
}

func TestBindRestartSuccessorPersistsFreshProcessAndRecoversWithoutRuntime(t *testing.T) {
	path, store, runtime, client, selection, pod := restartSuccessorFixture(t, "unhealthy")
	ctx := context.Background()
	resume, err := (k3s.Resolver{Client: client}).BindRestartSuccessor(ctx, store, selection)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.GetProcess(ctx, selection.Scope, selection.SlotID, selection.GenerationID)
	identity, identityErr := k3s.EncodeProcessIdentity(resume.Instance)
	if err != nil || identityErr != nil || p.EverReady || p.Terminal != nil || !bytes.Equal(identity, p.Identity) ||
		!p.StartedAt.Equal(pod.Status.ContainerStatuses[0].State.Running.StartedAt.Time) || resume.Instance.PodUID != string(pod.UID) || resume.Instance.ContainerID != "containerd://successor" ||
		resume.Scope != selection.Scope || resume.SlotID != selection.SlotID || resume.GenerationID != selection.GenerationID {
		t.Fatalf("binding lost exact process/start or inherited first readiness: %#v %#v %v", p, resume, err)
	}
	// Keep known first readiness on later recovery, independently of runtime Ready.
	if _, _, err := store.RecordProcess(ctx, state.ProcessObservation{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID,
		Identity: p.Identity, StartedAt: p.StartedAt, EverReady: true, IdempotencyKey: "first-readiness"}); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	clear(runtime.pods)
	requests := runtime.requests
	runtime.mu.Unlock()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recovered, err := (k3s.Resolver{}).BindRestartSuccessor(ctx, store, selection)
	if err != nil || !reflect.DeepEqual(resume, recovered) {
		t.Fatalf("bound recovery rediscovered runtime: %#v %v", recovered, err)
	}
	p, err = store.GetProcess(ctx, selection.Scope, selection.SlotID, selection.GenerationID)
	if err != nil || !p.EverReady {
		t.Fatalf("binding recovery erased history: %#v %v", p, err)
	}
	resume.Plan.Health.Check.HTTP.Path = "/mutated"
	if recovered.Plan.Health.Check.HTTP.Path == "/mutated" {
		t.Fatal("recovered plans share caller memory")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.requests != requests || runtime.creates != 1 || len(runtime.pods) != 0 {
		t.Fatal("binding recovery contacted runtime or recreated a resource")
	}
}

func TestBindRestartSuccessorAcknowledgesBeforeExposingResume(t *testing.T) {
	for _, mode := range []string{"before commit", "lost reply", "canceled after commit", "wrong identity", "wrong start", "wrong selection", "terminal acknowledgement", "superseded after commit"} {
		t.Run(mode, func(t *testing.T) {
			_, store, _, client, selection, _ := restartSuccessorFixture(t, "exit zero")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lost := errors.New("binding acknowledgement unavailable")
			journal := cycleStore{Store: store, observe: func(ctx context.Context, input state.ProcessObservation) (state.Process, bool, error) {
				if mode == "before commit" {
					return state.Process{}, false, lost
				}
				p, changed, err := store.RecordProcess(ctx, input)
				if err != nil {
					return p, changed, err
				}
				switch mode {
				case "lost reply":
					return state.Process{}, false, lost
				case "canceled after commit":
					cancel()
				case "wrong identity":
					p.Identity = []byte("other")
				case "wrong start":
					p.StartedAt = p.StartedAt.Add(time.Second)
				case "wrong selection":
					p.GenerationID = "other"
				case "terminal acknowledgement":
					p.Terminal = &state.ProcessTerminal{}
				case "superseded after commit":
					admitResumeGeneration(t, store, k3s.ResumeRequest{Scope: selection.Scope, SlotID: selection.SlotID, GenerationID: "generation-c"}, 2)
				}
				return p, changed, nil
			}}
			resume, err := (k3s.Resolver{Client: client}).BindRestartSuccessor(ctx, journal, selection)
			assertNoSuccessorResume(t, resume, err)
			if (mode == "before commit" || mode == "lost reply") && !errors.Is(err, lost) {
				t.Fatalf("binding cause lost: %v", err)
			}
			if mode == "canceled after commit" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if mode == "superseded after commit" {
				return
			}
			retryClient := client
			if mode != "before commit" {
				retryClient = nil
			}
			if _, err := (k3s.Resolver{Client: retryClient}).BindRestartSuccessor(context.Background(), store, selection); err != nil {
				t.Fatalf("exact retry failed: %v", err)
			}
		})
	}
}

func TestBindRestartSuccessorRejectsIneligibleWorkBeforeRuntime(t *testing.T) {
	for _, mode := range []string{"running", "bad result", "bad intent", "superseded", "bound mismatch", "terminal", "wrong scope", "wrong slot", "wrong generation", "wrong operation", "pre-canceled"} {
		t.Run(mode, func(t *testing.T) {
			_, store, runtime, client, selection, _ := restartSuccessorFixture(t, "exit zero")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			journal := restartReadStore{Store: store, operation: func(op *state.Operation) {
				switch mode {
				case "running":
					op.Status = state.OperationRunning
				case "bad result":
					op.Result = []byte(`{}`)
				case "bad intent":
					op.Intent = []byte(`{}`)
				}
			}}
			switch mode {
			case "superseded":
				admitResumeGeneration(t, store, k3s.ResumeRequest{Scope: selection.Scope, SlotID: selection.SlotID, GenerationID: "generation-c"}, 2)
			case "bound mismatch", "terminal":
				_, err := (k3s.Resolver{Client: client}).BindRestartSuccessor(ctx, store, selection)
				if err != nil {
					t.Fatal(err)
				}
				p, err := store.GetProcess(ctx, selection.Scope, selection.SlotID, selection.GenerationID)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "terminal" {
					if _, _, err := store.RecordProcessTerminal(ctx, state.ProcessTerminalObservation{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID,
						Identity: p.Identity, StartedAt: p.StartedAt, IdempotencyKey: "closed", Terminal: state.ProcessTerminal{Cause: state.ProcessStartupTimeout, OccurredAt: time.Now(), Evidence: []byte(`{}`)}}); err != nil {
						t.Fatal(err)
					}
				} else {
					// A different completed Pod cannot adopt the existing binding.
					journal = restartReadStore{Store: store}
					journal.operation = func(op *state.Operation) {
						op.Result = bytes.Replace(op.Result, []byte(`"pod-uid":"new-uid-1"`), []byte(`"pod-uid":"foreign-uid"`), 1)
					}
				}
			case "wrong scope":
				selection.Scope = "other"
			case "wrong slot":
				selection.SlotID = "other"
			case "wrong generation":
				selection.GenerationID = "other"
			case "wrong operation":
				selection.OperationID = "other"
			case "pre-canceled":
				cancel()
			}
			runtime.mu.Lock()
			requests := runtime.requests
			runtime.mu.Unlock()
			resume, err := (k3s.Resolver{Client: client}).BindRestartSuccessor(ctx, journal, selection)
			assertNoSuccessorResume(t, resume, err)
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			if runtime.requests != requests {
				t.Fatal("ineligible successor contacted runtime")
			}
		})
	}
}

func TestBindRestartSuccessorRejectsUnverifiedRuntimeWithoutBinding(t *testing.T) {
	for _, mode := range []string{"replaced", "foreign owner", "changed key", "changed plan", "pending", "waiting", "missing Pod", "forbidden", "missing id", "missing start", "missing version", "terminating", "duplicate status", "unknown termination", "missing finish", "mixed states"} {
		t.Run(mode, func(t *testing.T) {
			_, store, runtime, client, selection, pod := restartSuccessorFixture(t, "exit zero")
			status := &pod.Status.ContainerStatuses[0]
			switch mode {
			case "missing Pod":
				runtime.mu.Lock()
				runtime.getStatus = http.StatusNotFound
				runtime.mu.Unlock()
			case "forbidden":
				runtime.mu.Lock()
				runtime.getStatus = http.StatusForbidden
				runtime.mu.Unlock()
			case "replaced":
				pod.UID = "replacement"
			case "foreign owner":
				pod.OwnerReferences = []metav1.OwnerReference{{UID: "foreign"}}
			case "changed key":
				pod.Annotations["chmura/request-key-sha256"] = strings.Repeat("0", 64)
			case "changed plan":
				pod.Spec.Containers[0].Image = "other"
			case "pending":
				pod.Status.Phase = corev1.PodPending
			case "waiting":
				status.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
			case "missing id":
				status.ContainerID = ""
			case "missing start":
				status.State.Running.StartedAt = metav1.Time{}
			case "missing version":
				pod.ResourceVersion = ""
			case "terminating":
				now := metav1.Now()
				pod.DeletionTimestamp = &now
			case "duplicate status":
				pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, *status)
			case "unknown termination", "missing finish", "mixed states":
				started := status.State.Running.StartedAt
				status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: status.ContainerID, StartedAt: started, FinishedAt: metav1.Now(), Reason: "Completed"}}
				if mode == "unknown termination" {
					status.State.Terminated.Reason = "ContainerStatusUnknown"
				}
				if mode == "missing finish" {
					status.State.Terminated.FinishedAt = metav1.Time{}
				}
				if mode == "mixed states" {
					status.State.Running = &corev1.ContainerStateRunning{StartedAt: started}
				}
			}
			runtime.mu.Lock()
			runtime.pods[pod.Name] = pod
			runtime.mu.Unlock()
			resume, err := (k3s.Resolver{Client: client}).BindRestartSuccessor(context.Background(), store, selection)
			assertNoSuccessorResume(t, resume, err)
			if _, err := store.GetProcess(context.Background(), selection.Scope, selection.SlotID, selection.GenerationID); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("unverified runtime was bound: %v", err)
			}
		})
	}
}

func TestBindRestartSuccessorCapturesExitBeforeDiscoveryAndFeedsDurableSupervision(t *testing.T) {
	_, store, _, _, selection, pod := restartSuccessorFixture(t, "exit zero")
	started := pod.Status.ContainerStatuses[0].State.Running.StartedAt
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ContainerID: "containerd://successor", StartedAt: started, FinishedAt: metav1.Now(), ExitCode: 0, Reason: "Completed"}}
	client := apiClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("watch") != "" {
			t.Errorf("fast-exit handoff mutated runtime or started watch: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pod)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resume, err := (k3s.Resolver{Client: client}).BindRestartSuccessor(ctx, store, selection)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).ResumeDurable(ctx, store, cycleRequest(resume), cycleOptions())
	var exited *k3s.InstanceExitError
	if err != nil || result.Process.Terminal == nil || result.Process.Terminal.Cause != state.ProcessExited || result.Process.EverReady ||
		!errors.As(result.Outcome, &exited) || exited.ExitCode != 0 || !result.Process.StartedAt.Equal(started.Time) {
		t.Fatalf("fast successor exit lost: %#v %v", result, err)
	}
}

func TestBindRestartSuccessorResumesRunningProcessWithFreshReadiness(t *testing.T) {
	_, store, _, _, selection, pod := restartSuccessorFixture(t, "exit zero")
	attempted, allowReady, readyStored := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var attemptedOnce, readyOnce sync.Once
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attemptedOnce.Do(func() { close(attempted) })
		select {
		case <-allowReady:
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer app.Close()
	client := apiClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("handoff changed runtime: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") == "true" {
			openWatch(w)
			select {
			case <-readyStored:
			case <-r.Context().Done():
				return
			}
			exited := pod.DeepCopy()
			exited.ResourceVersion = "exited"
			status := &exited.Status.ContainerStatuses[0]
			status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: status.ContainerID,
				StartedAt: status.State.Running.StartedAt, FinishedAt: metav1.Now(), Reason: "Completed"}}
			watchEvent(t, w, "MODIFIED", exited)
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode(pod)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resume, err := (k3s.Resolver{Client: client}).BindRestartSuccessor(ctx, store, selection)
	if err != nil {
		t.Fatal(err)
	}
	history := cycleStore{Store: store, observe: func(ctx context.Context, r state.ProcessObservation) (state.Process, bool, error) {
		p, changed, err := store.RecordProcess(ctx, r)
		if err == nil && r.EverReady {
			readyOnce.Do(func() { close(readyStored) })
		}
		return p, changed, err
	}}
	type completion struct {
		result k3s.DurableSupervisionResult
		err    error
	}
	done := make(chan completion, 1)
	go func() {
		supervisor := k3s.Supervisor{Resolver: k3s.Resolver{Client: client}, Network: probe.Network{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, app.Listener.Addr().String())
			},
		}}
		result, err := supervisor.ResumeDurable(ctx, history, cycleRequest(resume), cycleOptions())
		done <- completion{result, err}
	}()
	select {
	case <-attempted:
	case got := <-done:
		t.Fatalf("supervision stopped before first probe: %v", got.err)
	case <-ctx.Done():
		t.Fatal("first probe did not start")
	}
	p, err := store.GetProcess(ctx, selection.Scope, selection.SlotID, selection.GenerationID)
	if err != nil || p.EverReady || p.Terminal != nil {
		t.Fatalf("runtime Ready bypassed Chmura's first readiness: %#v %v", p, err)
	}
	close(allowReady)
	select {
	case got := <-done:
		if got.err != nil || !got.result.Process.EverReady || got.result.Process.Terminal == nil || got.result.Process.GenerationID != selection.GenerationID || !errors.Is(got.result.Outcome, k3s.ErrInstanceExited) {
			t.Fatalf("successor handoff did not retain its own readiness/exit: %#v %v", got.result, got.err)
		}
	case <-ctx.Done():
		t.Fatal("successor cycle did not finish")
	}
}
