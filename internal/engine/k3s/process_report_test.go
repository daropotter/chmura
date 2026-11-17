package k3s_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/supervision"
)

func TestResumeFlushesFirstReadinessBeforeRecordingExit(t *testing.T) {
	pod := supervisedPod()
	started := pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time
	store, _, request := retainedProcess(t, started, false)
	identity, err := k3s.EncodeProcessIdentity(request.Instance)
	if err != nil {
		t.Fatal(err)
	}
	r, err := supervision.NewProcessReporter(store, state.ProcessObservation{Scope: request.Scope, SlotID: request.SlotID, GenerationID: request.GenerationID,
		IdempotencyKey: "owner-history-session", Identity: identity, StartedAt: started}, supervision.ReporterOptions{AttemptTimeout: time.Second, RetryInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	done := make(chan error, 1)
	go func() { done <- r.Run(workerCtx) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	exitNow := make(chan struct{})
	client := observerClient(t, pod, func(w http.ResponseWriter, request *http.Request) {
		openWatch(w)
		select {
		case <-exitNow:
		case <-request.Context().Done():
			return
		}
		changed := pod.DeepCopy()
		changed.ResourceVersion = "43"
		markExited(changed)
		watchEvent(t, w, "MODIFIED", changed)
		<-request.Context().Done()
	})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer app.Close()
	request.Plan = supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
	err = (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).Resume(ctx, store, request, func(value probe.Readiness) {
		r.Observe(value)
		if value.Ready {
			close(exitNow)
		}
	})
	var decision *k3s.RestartNeededError
	var exit *k3s.InstanceExitError
	if !errors.As(err, &decision) || !errors.As(err, &exit) {
		t.Fatalf("exit decision lost: %v", err)
	}
	outcome := err
	// Monitor has joined its callbacks. Keep persistence alive until its queued
	// first-readiness fact is acknowledged, then stop/join before terminal I/O.
	if err := r.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	stopWorker()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	retained, err := store.GetProcess(ctx, request.Scope, request.SlotID, request.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := k3s.PrepareProcessTerminal(k3s.ProcessTerminalRequest{Process: retained, Instance: request.Instance, Plan: request.Plan, Cause: outcome, IdempotencyKey: "exit", ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	got, recorded, err := store.RecordProcessTerminal(ctx, terminal)
	if err != nil || !recorded || !got.EverReady || got.Terminal == nil || got.Terminal.Cause != state.ProcessExited {
		t.Fatalf("terminal overtook first-readiness persistence: %#v %v %v", got, recorded, err)
	}
}
