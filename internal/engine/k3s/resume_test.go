package k3s_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Frozen canonical v1 identity: changing its encoding would strand stored history.
const persistedInstanceIdentity = `{"version":1,"engine":"k3s","namespace":"team-a","pod-name":"app-a","pod-uid":"pod-generation-a","container-name":"app","container-id":"containerd://generation-a"}`

func retainedProcess(t *testing.T, started time.Time, ready bool) (*sqlite.Store, string, k3s.ResumeRequest) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	request := k3s.ResumeRequest{Scope: "project-a", SlotID: "api/0", GenerationID: "generation-a", Instance: instance(), Plan: supervisionPlan(8080)}
	admitResumeGeneration(t, store, request, 0)
	if _, _, err := store.RecordProcess(context.Background(), state.ProcessObservation{Scope: request.Scope, SlotID: request.SlotID, GenerationID: request.GenerationID,
		IdempotencyKey: "bind", Identity: []byte(persistedInstanceIdentity), StartedAt: started, EverReady: ready}); err != nil {
		t.Fatal(err)
	}
	return store, path, request
}

func admitResumeGeneration(t *testing.T, store *sqlite.Store, request k3s.ResumeRequest, version int64) {
	t.Helper()
	ctx := context.Background()
	op, _, err := store.Accept(ctx, state.Submission{Scope: request.Scope, IdempotencyKey: "deploy-" + request.GenerationID, Kind: "deploy", Intent: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, state.OperationUpdate{Scope: request.Scope, OperationID: op.ID, IdempotencyKey: "running", ExpectedVersion: 1, Status: state.OperationRunning}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AdmitGeneration(ctx, state.GenerationRequest{Scope: request.Scope, SlotID: request.SlotID, ID: request.GenerationID, OperationID: op.ID,
		IdempotencyKey: "admit-" + request.GenerationID, ExpectedVersion: version, Intent: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, state.OperationUpdate{Scope: request.Scope, OperationID: op.ID, IdempotencyKey: "finish", ExpectedVersion: 2, Status: state.OperationSucceeded, Result: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeProcessIdentityRetainsCompleteCanonicalReference(t *testing.T) {
	encoded, err := k3s.EncodeProcessIdentity(instance())
	if err != nil || !bytes.Equal(encoded, []byte(persistedInstanceIdentity)) {
		t.Fatalf("canonical identity incompatible with retained v1: %q %v", encoded, err)
	}
	for _, field := range []string{"namespace", "pod name", "pod UID", "container name", "container ID"} {
		t.Run(field, func(t *testing.T) {
			for _, value := range []string{"different", "", " ", string([]byte{0xff})} {
				ref := instance()
				switch field {
				case "namespace":
					ref.Namespace = value
				case "pod name":
					ref.PodName = value
				case "pod UID":
					ref.PodUID = value
				case "container name":
					ref.ContainerName = value
				case "container ID":
					ref.ContainerID = value
				}
				got, err := k3s.EncodeProcessIdentity(ref)
				if value == "different" {
					if err != nil || bytes.Equal(got, encoded) {
						t.Fatalf("changed reference lost identity: %q %v", got, err)
					}
				} else if err == nil || len(got) != 0 {
					t.Fatalf("incomplete/noncanonical reference encoded: %q %v", got, err)
				}
			}
		})
	}
}

func TestResumeRestoresHistoryButRequiresFullCurrentReadiness(t *testing.T) {
	pod := supervisedPod()
	pod.Status.ContainerStatuses[1].State.Running.StartedAt = metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	store, path, request := retainedProcess(t, pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time, true)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var probes atomic.Int32
	second, grant := make(chan struct{}), make(chan struct{})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if probes.Add(1) == 2 {
			close(second)
			select {
			case <-grant:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer app.Close()
	request.Plan = supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
	request.Plan.Health.Ready.Successes = 2
	var watches atomic.Int32
	watchStopped := make(chan struct{})
	client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) {
		watches.Add(1)
		defer close(watchStopped)
		openWatch(w)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	updates, done := make(chan probe.Readiness, 8), make(chan error, 1)
	go func() {
		done <- (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).Resume(ctx, store, request, func(value probe.Readiness) { updates <- value })
	}()
	checkSupervisionState(t, updates, done, probe.Readiness{EverReady: true})
	select {
	case <-second:
	case err := <-done:
		t.Fatalf("history rearmed startup timeout: %v", err)
	case <-ctx.Done():
		t.Fatal("second probe did not start")
	}
	select {
	case value := <-updates:
		t.Fatalf("partial success granted readiness: %#v", value)
	default:
	}
	close(grant)
	checkSupervisionState(t, updates, done, probe.Readiness{Ready: true, EverReady: true})
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("resume stop: %v", err)
	}
	if last := <-updates; last != (probe.Readiness{EverReady: true}) {
		t.Fatalf("stop retained traffic readiness: %#v", last)
	}
	select {
	case <-watchStopped:
	case <-time.After(time.Second):
		t.Fatal("resume left observation running")
	}
	if probes.Load() < 2 || watches.Load() != 1 {
		t.Fatalf("unexpected work: probes=%d watches=%d", probes.Load(), watches.Load())
	}
}

func TestResumeRejectsMissingObsoleteTerminalAndUnrelatedHistoryBeforeRuntime(t *testing.T) {
	for _, mode := range []string{"terminal", "obsolete", "foreign scope", "foreign slot", "missing generation", "unbound successor", "namespace", "pod name", "pod UID", "container name", "container ID", "blank scope", "blank slot", "blank generation", "no history", "no observer", "pre-canceled", "closed database"} {
		t.Run(mode, func(t *testing.T) {
			started := time.Now().Truncate(time.Second)
			store, _, request := retainedProcess(t, started, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := state.ErrConflict
			var history k3s.SupervisionHistory = store
			observer := func(probe.Readiness) { t.Error("rejected resume published readiness") }
			switch mode {
			case "terminal":
				if _, _, err := store.RecordProcessTerminal(ctx, state.ProcessTerminalObservation{Scope: request.Scope, SlotID: request.SlotID, GenerationID: request.GenerationID,
					IdempotencyKey: "failed", Identity: []byte(persistedInstanceIdentity), StartedAt: started,
					Terminal: state.ProcessTerminal{Cause: state.ProcessStartupTimeout, OccurredAt: started.Add(time.Minute), Evidence: []byte(`{}`)}}); err != nil {
					t.Fatal(err)
				}
			case "obsolete", "unbound successor":
				next := request
				next.GenerationID = "generation-b"
				admitResumeGeneration(t, store, next, 1)
				if mode == "unbound successor" {
					request = next
					want = state.ErrNotFound
				}
			case "foreign scope":
				request.Scope = "foreign"
				want = state.ErrNotFound
			case "foreign slot":
				request.SlotID = "foreign"
				want = state.ErrNotFound
			case "missing generation":
				request.GenerationID = "missing"
			case "namespace":
				request.Instance.Namespace = "other"
			case "pod name":
				request.Instance.PodName = "other"
			case "pod UID":
				request.Instance.PodUID = "other"
			case "container name":
				request.Instance.ContainerName = "other"
			case "container ID":
				request.Instance.ContainerID = "other"
			case "blank scope":
				request.Scope = " "
				want = nil
			case "blank slot":
				request.SlotID = ""
				want = nil
			case "blank generation":
				request.GenerationID = ""
				want = nil
			case "no observer":
				observer = nil
				want = nil
			case "no history":
				history = nil
				want = nil
			case "pre-canceled":
				cancel()
				want = context.Canceled
			case "closed database":
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				want = nil
			}
			client := apiClient(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("rejected resume contacted runtime: %s", r.URL.Path)
				w.WriteHeader(500)
			})
			err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).Resume(ctx, history, request, observer)
			if err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatalf("resume error=%v, want %v", err, want)
			}
		})
	}
}

type interruptedHistory struct {
	*sqlite.Store
	fault  error
	cancel context.CancelFunc
}

func (h interruptedHistory) GetProcess(ctx context.Context, scope, slot, generation string) (state.Process, error) {
	process, err := h.Store.GetProcess(ctx, scope, slot, generation)
	if err != nil {
		return process, err
	}
	if h.cancel != nil {
		h.cancel()
	}
	return process, h.fault
}

func TestResumePreservesHistoryReadFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"read failed", "canceled after read", "canceled with read failure"} {
		t.Run(mode, func(t *testing.T) {
			store, _, request := retainedProcess(t, time.Now(), true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("process history read failed")
			history := interruptedHistory{Store: store, fault: failure}
			want := failure
			if mode != "read failed" {
				history.cancel, want = cancel, context.Canceled
			}
			if mode == "canceled after read" {
				history.fault = nil
			}
			client := apiClient(t, func(w http.ResponseWriter, r *http.Request) {
				t.Error("failed/canceled history read contacted runtime")
				w.WriteHeader(500)
			})
			err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).Resume(ctx, history, request, func(probe.Readiness) { t.Error("failed/canceled resume published readiness") })
			if !errors.Is(err, want) {
				t.Fatalf("resume lost read/cancellation cause: %v, want %v", err, want)
			}
		})
	}
}

func TestResumeChecksPersistedStartBeforeChecksAndRestartHandoff(t *testing.T) {
	for _, mode := range []string{"running mismatch", "exit mismatch", "matching exit", "matching exit timezone"} {
		t.Run(mode, func(t *testing.T) {
			pod := supervisedPod()
			started := pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time
			if mode == "matching exit timezone" {
				started = started.In(time.FixedZone("offset", 3600))
			}
			store, _, request := retainedProcess(t, started, true)
			if mode == "running mismatch" || mode == "exit mismatch" {
				pod.Status.ContainerStatuses[1].State.Running.StartedAt = metav1.NewTime(started.Add(time.Second))
			}
			if mode != "running mismatch" {
				markExited(pod)
			}
			watchStopped := make(chan struct{})
			client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) {
				defer close(watchStopped)
				openWatch(w)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			supervisor := k3s.Supervisor{Resolver: k3s.Resolver{Client: client}, Network: probe.Network{DialContext: func(context.Context, string, string) (net.Conn, error) {
				t.Error("mismatched/exited process was probed")
				return nil, errors.New("unexpected probe")
			}}}
			err := supervisor.Resume(ctx, store, request, func(probe.Readiness) { t.Error("mismatched/exited process published readiness") })
			var decision *k3s.RestartNeededError
			var exit *k3s.InstanceExitError
			if mode == "matching exit" || mode == "matching exit timezone" {
				if !errors.As(err, &decision) || !errors.As(err, &exit) || !exit.StartedAt.Equal(started) {
					t.Fatalf("matching exit lost handoff: %v", err)
				}
			} else if !errors.Is(err, k3s.ErrObservationLost) || errors.As(err, &decision) || errors.Is(err, k3s.ErrInstanceExited) {
				t.Fatalf("changed start became crash evidence: %v", err)
			}
			if mode == "running mismatch" {
				select {
				case <-watchStopped:
				case <-time.After(time.Second):
					t.Fatal("start mismatch left watch running")
				}
			}
		})
	}
}

func TestResumeUsesRetainedFirstReadinessForStartupAndHealthy(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(map[bool]string{false: "never ready", true: "previously ready"}[ready], func(t *testing.T) {
			pod := supervisedPod()
			pod.Status.ContainerStatuses[1].State.Running.StartedAt = metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
			store, _, request := retainedProcess(t, pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time, ready)
			var healthy atomic.Int32
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthy" {
					healthy.Add(1)
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer app.Close()
			request.Plan = supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
			request.Plan.Health.Healthy = &manifest.HealthyRule{UnhealthyFailures: 1, Check: &manifest.Probe{HTTP: &manifest.HTTPCheck{Path: "/healthy", Port: "web"}}}
			client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) { openWatch(w); <-r.Context().Done() })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var last probe.Readiness
			err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).Resume(ctx, store, request, func(value probe.Readiness) { last = value })
			want := probe.ErrStartupTimeout
			if ready {
				want = probe.ErrUnhealthy
			}
			if !errors.Is(err, want) || last.EverReady != ready || last.Ready || last.StartupFailed == ready || (ready && healthy.Load() != 1) || (!ready && healthy.Load() != 0) {
				t.Fatalf("history applied incorrectly: last=%#v healthy=%d error=%v", last, healthy.Load(), err)
			}
		})
	}
}
