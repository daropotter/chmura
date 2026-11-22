package k3s_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
	"github.com/daropotter/chmura/internal/supervision"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type restartOwnerHistory struct {
	cycleStore
	update func(context.Context, state.OperationUpdate) (state.Operation, bool, error)
}

func (s restartOwnerHistory) Update(ctx context.Context, request state.OperationUpdate) (state.Operation, bool, error) {
	if s.update != nil {
		return s.update(ctx, request)
	}
	return s.Store.Update(ctx, request)
}

type ownerRig struct {
	path     string
	store    *sqlite.Store
	runtime  *lifetimeStore
	owner    k3s.RestartOwner
	request  k3s.RestartOwnerRequest
	options  k3s.RestartOwnerOptions
	nextName string
}

func newOwnerRig(t *testing.T) *ownerRig {
	t.Helper()
	path, store, admission := restartAdmissionDriverFixture(t, "exit zero")
	runtime := &lifetimeStore{}
	client, _, _ := lifetimeClient(t, runtime)
	desired, err := k3s.PrepareInstance(admission.Next)
	if err != nil {
		t.Fatal(err)
	}
	return &ownerRig{path: path, store: store, runtime: runtime, nextName: desired.Name,
		owner:   k3s.RestartOwner{Slots: new(supervision.SlotRunner), Supervisor: k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}},
		request: k3s.RestartOwnerRequest{Admission: admission, HistoryKey: "owner-history", TerminalKey: "owner-terminal"},
		options: k3s.RestartOwnerOptions{RetireInterval: time.Millisecond, Supervision: cycleOptions()}}
}

func (r *ownerRig) observeSuccessor(exited bool) error {
	r.runtime.mu.Lock()
	defer r.runtime.mu.Unlock()
	pod := r.runtime.pods[r.nextName]
	if pod == nil {
		return errors.New("successor was not created before completion")
	}
	if len(pod.Status.ContainerStatuses) > 0 {
		return nil
	}
	started, finished := metav1.NewTime(time.Now().Add(-time.Second).Truncate(time.Second)), metav1.Now()
	status := corev1.ContainerStatus{Name: "app", ContainerID: "containerd://owner-successor", Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}}}
	pod.Status.Phase, pod.Status.PodIP = corev1.PodRunning, "127.0.0.1"
	if exited {
		pod.Status.Phase = corev1.PodSucceeded
		status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: status.ContainerID, StartedAt: started, FinishedAt: finished, Reason: "Completed"}}
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{status}
	return nil
}

func (r *ownerRig) history(exited bool) restartOwnerHistory {
	return restartOwnerHistory{cycleStore: cycleStore{Store: r.store}, update: func(ctx context.Context, request state.OperationUpdate) (state.Operation, bool, error) {
		op, changed, err := r.store.Update(ctx, request)
		if err == nil && request.Status == state.OperationSucceeded {
			err = r.observeSuccessor(exited)
		}
		return op, changed, err
	}}
}

func assertNoOwnerResult(t *testing.T, result k3s.DurableSupervisionResult, err error) {
	t.Helper()
	if err == nil || !reflect.DeepEqual(result, k3s.DurableSupervisionResult{}) {
		t.Fatalf("unacknowledged owner outcome exposed: %#v %v", result, err)
	}
}

func TestRestartOwnerKeepsSlotAcrossRetirementAndDurableSupervision(t *testing.T) {
	for _, mode := range []string{"terminal acknowledgement", "cancellation drain"} {
		t.Run(mode, func(t *testing.T) {
			rig := newOwnerRig(t)
			retired, ready, blocked, release, watchStopped := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			var readyOnce, blockedOnce, releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			defer app.Close()
			rig.owner.Supervisor.Network = probe.Network{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, app.Listener.Addr().String())
			}}
			rig.options.Supervision.FinalizeTimeout, rig.options.Supervision.Writes.AttemptTimeout = 3*time.Second, 2*time.Second
			history := rig.history(false)
			history.observe = func(ctx context.Context, request state.ProcessObservation) (state.Process, bool, error) {
				if request.EverReady && mode == "cancellation drain" {
					blockedOnce.Do(func() { close(blocked) })
					select {
					case <-release:
					case <-ctx.Done():
						return state.Process{}, false, ctx.Err()
					}
				}
				p, changed, err := rig.store.RecordProcess(ctx, request)
				if err == nil && request.EverReady {
					readyOnce.Do(func() { close(ready) })
				}
				return p, changed, err
			}
			history.finish = func(ctx context.Context, request state.ProcessTerminalObservation) (state.Process, bool, error) {
				p, changed, err := rig.store.RecordProcessTerminal(ctx, request)
				if err == nil {
					blockedOnce.Do(func() { close(blocked) })
					select {
					case <-release:
					case <-ctx.Done():
						return state.Process{}, false, ctx.Err()
					}
				}
				return p, changed, err
			}
			rig.runtime.mu.Lock()
			rig.runtime.accepted = retired
			rig.runtime.watch = func(w http.ResponseWriter, r *http.Request) {
				defer close(watchStopped)
				openWatch(w)
				if mode == "terminal acknowledgement" {
					select {
					case <-ready:
					case <-r.Context().Done():
						return
					}
					pod, _, _ := rig.runtime.snapshot(rig.nextName)
					status := &pod.Status.ContainerStatuses[0]
					status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: status.ContainerID, StartedAt: status.State.Running.StartedAt, FinishedAt: metav1.Now(), Reason: "Completed"}}
					pod.ResourceVersion = "exited"
					watchEvent(t, w, "MODIFIED", pod)
				}
				<-r.Context().Done()
			}
			rig.runtime.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			type completion struct {
				result k3s.DurableSupervisionResult
				err    error
			}
			done := make(chan completion, 1)
			go func() {
				value, err := rig.owner.Run(ctx, history, rig.request, rig.options)
				done <- completion{value, err}
			}()
			select {
			case <-retired:
			case got := <-done:
				t.Fatalf("owner skipped graceful retirement: %v", got.err)
			case <-ctx.Done():
				t.Fatal("retirement did not start")
			}
			rig.runtime.mu.Lock()
			creates := rig.runtime.creates
			rig.runtime.mu.Unlock()
			if creates != 0 {
				t.Fatal("owner created a successor before old absence")
			}
			contender, stopContender := context.WithTimeout(context.Background(), time.Second)
			value, err := rig.owner.Run(contender, history, rig.request, rig.options)
			stopContender()
			assertNoOwnerResult(t, value, err)
			if !errors.Is(err, state.ErrConflict) {
				t.Fatalf("contender was not rejected by shared ownership: %v", err)
			}
			rig.runtime.mu.Lock()
			clear(rig.runtime.pods)
			rig.runtime.mu.Unlock()
			select {
			case <-blocked:
			case got := <-done:
				t.Fatalf("owner ended before supervision acknowledgement: %#v %v", got.result, got.err)
			case <-ctx.Done():
				t.Fatal("supervision did not start")
			}
			if mode == "cancellation drain" {
				cancel()
			}
			select {
			case <-watchStopped:
			case <-time.After(time.Second):
				t.Fatal("supervisor observation did not stop")
			}
			called := false
			if err := rig.owner.Slots.Run(context.Background(), rig.request.Admission.Scope, rig.request.Admission.SlotID, func(context.Context) error { called = true; return nil }); !errors.Is(err, state.ErrConflict) || called {
				t.Fatalf("owner released slot before drain/ack: %v called=%v", err, called)
			}
			select {
			case got := <-done:
				t.Fatalf("owner exposed blocked result: %#v %v", got.result, got.err)
			default:
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case got := <-done:
				if mode == "cancellation drain" {
					assertNoOwnerResult(t, got.result, got.err)
					if !errors.Is(got.err, context.Canceled) {
						t.Fatalf("cancellation lost: %v", got.err)
					}
				} else if got.err != nil || !errors.Is(got.result.Outcome, k3s.ErrInstanceExited) || !got.result.Process.EverReady || got.result.Process.Terminal == nil || got.result.Process.GenerationID != rig.request.Admission.GenerationID {
					t.Fatalf("owned cycle did not acknowledge successor terminal history: %#v %v", got.result, got.err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("owner did not drain")
			}
			if err := rig.store.Close(); err != nil {
				t.Fatal(err)
			}
			rig.store, err = sqlite.Open(context.Background(), rig.path)
			if err != nil {
				t.Fatal(err)
			}
			defer rig.store.Close()
			p, err := rig.store.GetProcess(context.Background(), rig.request.Admission.Scope, rig.request.Admission.SlotID, rig.request.Admission.GenerationID)
			if err != nil || !p.EverReady || (mode == "cancellation drain") != (p.Terminal == nil) {
				t.Fatalf("reopen lost owned history: %#v %v", p, err)
			}
			rig.runtime.mu.Lock()
			creates, deletes, requests := rig.runtime.creates, rig.runtime.deletes, rig.runtime.requests
			rig.runtime.mu.Unlock()
			if creates != 1 || deletes != 1 {
				t.Fatalf("owner duplicated runtime work: creates=%d deletes=%d", creates, deletes)
			}
			if mode == "terminal acknowledgement" {
				value, err := rig.owner.Run(context.Background(), rig.store, rig.request, rig.options)
				assertNoOwnerResult(t, value, err)
				if !errors.Is(err, state.ErrConflict) {
					t.Fatalf("closed successor resumed: %v", err)
				}
				rig.runtime.mu.Lock()
				defer rig.runtime.mu.Unlock()
				if rig.runtime.requests != requests || rig.runtime.creates != 1 {
					t.Fatal("terminal retry contacted runtime or recreated successor")
				}
			} else {
				// Recovery resumes the same bound process with retained first
				// readiness; it does not create another successor after cancellation.
				rig.runtime.mu.Lock()
				rig.runtime.watch = func(w http.ResponseWriter, r *http.Request) {
					openWatch(w)
					pod, _, _ := rig.runtime.snapshot(rig.nextName)
					status := &pod.Status.ContainerStatuses[0]
					status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: status.ContainerID, StartedAt: status.State.Running.StartedAt, FinishedAt: metav1.Now(), Reason: "Completed"}}
					pod.ResourceVersion = "recovered-exit"
					watchEvent(t, w, "MODIFIED", pod)
					<-r.Context().Done()
				}
				rig.runtime.mu.Unlock()
				recovery, stopRecovery := context.WithTimeout(context.Background(), 5*time.Second)
				defer stopRecovery()
				value, err := rig.owner.Run(recovery, rig.store, rig.request, rig.options)
				if err != nil || !value.Process.EverReady || value.Process.Terminal == nil || !errors.Is(value.Outcome, k3s.ErrInstanceExited) {
					t.Fatalf("canceled cycle did not resume retained successor history: %#v %v", value, err)
				}
				rig.runtime.mu.Lock()
				defer rig.runtime.mu.Unlock()
				if rig.runtime.creates != 1 || rig.runtime.deletes != 1 {
					t.Fatal("canceled-cycle recovery repeated runtime creation/retirement")
				}
			}
		})
	}
}

func TestRestartOwnerRecoversFailedStageWithoutDuplicatingSuccessor(t *testing.T) {
	for _, mode := range []string{"runtime error", "completion reply", "binding reply", "pending successor", "terminal write"} {
		t.Run(mode, func(t *testing.T) {
			rig := newOwnerRig(t)
			rig.runtime.mu.Lock()
			clear(rig.runtime.pods)
			if mode == "runtime error" {
				rig.runtime.createStatus = http.StatusForbidden
			}
			rig.runtime.mu.Unlock()
			lost := errors.New("owned stage acknowledgement lost")
			history := rig.history(true)
			baseUpdate := history.update
			history.update = func(ctx context.Context, request state.OperationUpdate) (state.Operation, bool, error) {
				if mode == "pending successor" {
					return rig.store.Update(ctx, request)
				}
				op, changed, err := baseUpdate(ctx, request)
				if err == nil && mode == "completion reply" && request.Status == state.OperationSucceeded {
					return state.Operation{}, false, lost
				}
				return op, changed, err
			}
			if mode == "binding reply" {
				history.observe = func(ctx context.Context, request state.ProcessObservation) (state.Process, bool, error) {
					p, changed, err := rig.store.RecordProcess(ctx, request)
					if err == nil && request.IdempotencyKey == "restart-successor-bind-v1" {
						return state.Process{}, false, lost
					}
					return p, changed, err
				}
			}
			if mode == "terminal write" {
				rig.options.Supervision.FinalizeTimeout = 200 * time.Millisecond
				history.finish = func(context.Context, state.ProcessTerminalObservation) (state.Process, bool, error) {
					return state.Process{}, false, lost
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			value, err := rig.owner.Run(ctx, history, rig.request, rig.options)
			assertNoOwnerResult(t, value, err)
			switch mode {
			case "runtime error":
				if !apierrors.IsForbidden(err) {
					t.Fatalf("runtime cause lost: %v", err)
				}
			case "pending successor":
				if !errors.Is(err, k3s.ErrInstanceUnavailable) {
					t.Fatalf("pending process was not returned for reconciliation: %v", err)
				}
			default:
				if !errors.Is(err, lost) {
					t.Fatalf("storage cause lost: %v", err)
				}
			}
			if err := rig.store.Close(); err != nil {
				t.Fatal(err)
			}
			rig.store, err = sqlite.Open(context.Background(), rig.path)
			if err != nil {
				t.Fatal(err)
			}
			defer rig.store.Close()
			rig.runtime.mu.Lock()
			rig.runtime.createStatus = 0
			rig.runtime.mu.Unlock()
			if mode == "pending successor" {
				if err := rig.observeSuccessor(true); err != nil {
					t.Fatal(err)
				}
			}
			rig.options.Supervision.FinalizeTimeout = time.Second
			value, err = rig.owner.Run(ctx, rig.history(true), rig.request, rig.options)
			if err != nil || value.Process.Terminal == nil || !errors.Is(value.Outcome, k3s.ErrInstanceExited) {
				t.Fatalf("owned recovery failed: %#v %v", value, err)
			}
			rig.runtime.mu.Lock()
			defer rig.runtime.mu.Unlock()
			if rig.runtime.creates != 1 {
				t.Fatalf("recovery duplicated successor: %d", rig.runtime.creates)
			}
		})
	}
}

func TestRestartOwnerValidatesBeforeAdmissionOrRuntime(t *testing.T) {
	for _, mode := range []string{"slots", "history", "client", "interval", "history key", "terminal key", "attempt", "retry", "finalization", "publication key", "publication attempt", "publication retry", "plan", "pre-canceled"} {
		t.Run(mode, func(t *testing.T) {
			rig := newOwnerRig(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			accepts := 0
			var history k3s.RestartOwnerStore = restartAdmissionDriverStore{Store: rig.store, accept: func(ctx context.Context, request state.Submission) (state.Operation, bool, error) {
				accepts++
				return rig.store.Accept(ctx, request)
			}}
			if mode == "publication key" || mode == "publication attempt" || mode == "publication retry" {
				rig.options.Supervision.Readiness = &k3s.ReadinessPublication{SessionKey: "publication-session", Writes: k3s.ReporterOptions{AttemptTimeout: time.Second, RetryInterval: time.Millisecond}}
			}
			switch mode {
			case "slots":
				rig.owner.Slots = nil
			case "history":
				history = nil
			case "client":
				rig.owner.Supervisor.Resolver.Client = nil
			case "interval":
				rig.options.RetireInterval = 0
			case "history key":
				rig.request.HistoryKey = ""
			case "terminal key":
				rig.request.TerminalKey = ""
			case "attempt":
				rig.options.Supervision.Writes.AttemptTimeout = 0
			case "retry":
				rig.options.Supervision.Writes.RetryInterval = 0
			case "finalization":
				rig.options.Supervision.FinalizeTimeout = 0
			case "publication key":
				rig.options.Supervision.Readiness.SessionKey = ""
			case "publication attempt":
				rig.options.Supervision.Readiness.Writes.AttemptTimeout = 0
			case "publication retry":
				rig.options.Supervision.Readiness.Writes.RetryInterval = 0
			case "plan":
				rig.request.Admission.Next.Plan.Readiness = "process-only"
			case "pre-canceled":
				cancel()
			}
			value, err := rig.owner.Run(ctx, history, rig.request, rig.options)
			assertNoOwnerResult(t, value, err)
			rig.runtime.mu.Lock()
			defer rig.runtime.mu.Unlock()
			if accepts != 0 || rig.runtime.requests != 0 {
				t.Fatalf("bad owner configuration performed I/O: accepts=%d runtime=%d", accepts, rig.runtime.requests)
			}
		})
	}
}
