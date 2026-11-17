package k3s_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
	"github.com/daropotter/chmura/internal/supervision"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type cycleStore struct {
	*sqlite.Store
	observe func(context.Context, state.ProcessObservation) (state.Process, bool, error)
	finish  func(context.Context, state.ProcessTerminalObservation) (state.Process, bool, error)
}

func (s cycleStore) RecordProcess(ctx context.Context, r state.ProcessObservation) (state.Process, bool, error) {
	if s.observe != nil {
		return s.observe(ctx, r)
	}
	return s.Store.RecordProcess(ctx, r)
}
func (s cycleStore) RecordProcessTerminal(ctx context.Context, r state.ProcessTerminalObservation) (state.Process, bool, error) {
	if s.finish != nil {
		return s.finish(ctx, r)
	}
	return s.Store.RecordProcessTerminal(ctx, r)
}

func cycleRequest(resume k3s.ResumeRequest) k3s.DurableSupervisionRequest {
	return k3s.DurableSupervisionRequest{Resume: resume, HistoryKey: "history-session", TerminalKey: "terminal-decision"}
}
func cycleOptions() k3s.DurableSupervisionOptions {
	return k3s.DurableSupervisionOptions{Writes: supervision.ReporterOptions{AttemptTimeout: time.Second, RetryInterval: time.Millisecond}, FinalizeTimeout: time.Second}
}

func TestDurableSupervisionDrainsHistoryBeforeAcknowledgingTerminal(t *testing.T) {
	pod := supervisedPod()
	started := pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time
	store, path, resume := retainedProcess(t, started, false)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer app.Close()
	resume.Plan = supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
	readyWrite, releaseWrite, watchStopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var readyOnce sync.Once
	var terminalCalls atomic.Int32
	history := cycleStore{Store: store}
	history.observe = func(ctx context.Context, r state.ProcessObservation) (state.Process, bool, error) {
		if r.EverReady {
			readyOnce.Do(func() { close(readyWrite) })
			select {
			case <-releaseWrite:
			case <-ctx.Done():
				return state.Process{}, false, ctx.Err()
			}
		}
		return store.RecordProcess(ctx, r)
	}
	history.finish = func(ctx context.Context, r state.ProcessTerminalObservation) (state.Process, bool, error) {
		terminalCalls.Add(1)
		select {
		case <-watchStopped:
		default:
			t.Error("terminal write started before monitor observation joined")
		}
		previous, err := store.GetProcess(ctx, r.Scope, r.SlotID, r.GenerationID)
		if err != nil || !previous.EverReady {
			t.Errorf("terminal overtook acknowledged first readiness: %#v %v", previous, err)
		}
		return store.RecordProcessTerminal(ctx, r)
	}
	client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) {
		defer close(watchStopped)
		openWatch(w)
		select {
		case <-readyWrite:
		case <-r.Context().Done():
			return
		}
		changed := pod.DeepCopy()
		changed.ResourceVersion = "43"
		markExited(changed)
		watchEvent(t, w, "MODIFIED", changed)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		value k3s.DurableSupervisionResult
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).ResumeDurable(ctx, history, cycleRequest(resume), cycleOptions())
		done <- result{value, err}
	}()
	select {
	case <-watchStopped:
	case got := <-done:
		t.Fatalf("cycle ended before exit cleanup: %v", got.err)
	case <-ctx.Done():
		t.Fatal("watch was not stopped")
	}
	if terminalCalls.Load() != 0 {
		t.Fatal("uncommitted readiness allowed terminal write")
	}
	select {
	case got := <-done:
		t.Fatalf("cycle acknowledged before drain: %#v %v", got.value, got.err)
	default:
	}
	close(releaseWrite)
	got := <-done
	var decision *k3s.RestartNeededError
	if got.err != nil || !errors.As(got.value.Outcome, &decision) || !got.value.Process.EverReady || got.value.Process.Terminal == nil || terminalCalls.Load() != 1 {
		t.Fatalf("durable completion: %#v %v", got.value, got.err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.GetProcess(context.Background(), resume.Scope, resume.SlotID, resume.GenerationID)
	if err != nil || persisted.Terminal == nil || !persisted.EverReady || !bytes.Equal(persisted.Terminal.Evidence, got.value.Process.Terminal.Evidence) {
		t.Fatalf("recovery lost acknowledged history: %#v %v", persisted, err)
	}
	if next, err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).ResumeDurable(context.Background(), reopened, cycleRequest(resume), cycleOptions()); !errors.Is(err, state.ErrConflict) || next.Outcome != nil {
		t.Fatalf("closed generation resumed: %#v %v", next, err)
	}
}

func TestDurableSupervisionReplaysIdenticalHealthyTerminalAfterLostResponse(t *testing.T) {
	pod := supervisedPod()
	pod.Status.ContainerStatuses[1].State.Running.StartedAt = metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	store, _, resume := retainedProcess(t, pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time, true)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer app.Close()
	resume.Plan = supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
	resume.Plan.Health.Healthy = &manifest.HealthyRule{UnhealthyFailures: 1, Check: &manifest.Probe{HTTP: &manifest.HTTPCheck{Path: "/healthy", Port: "web"}}}
	client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) { openWatch(w); <-r.Context().Done() })
	var first state.ProcessTerminalObservation
	var calls int
	history := cycleStore{Store: store, finish: func(ctx context.Context, r state.ProcessTerminalObservation) (state.Process, bool, error) {
		p, recorded, err := store.RecordProcessTerminal(ctx, r)
		if err != nil {
			return p, recorded, err
		}
		calls++
		if calls == 1 {
			first = r
			return state.Process{}, false, errors.New("committed terminal reply lost")
		}
		if recorded || r.IdempotencyKey != first.IdempotencyKey || !r.Terminal.OccurredAt.Equal(first.Terminal.OccurredAt) || !bytes.Equal(r.Terminal.Evidence, first.Terminal.Evidence) {
			t.Error("terminal retry changed its immutable request")
		}
		return p, recorded, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).ResumeDurable(ctx, history, cycleRequest(resume), cycleOptions())
	if err != nil || calls != 2 || !errors.Is(result.Outcome, probe.ErrUnhealthy) || result.Process.Terminal == nil || result.Process.Terminal.Cause != state.ProcessUnhealthy {
		t.Fatalf("healthy replay: %#v calls=%d %v", result, calls, err)
	}
}

func TestDurableSupervisionPreservesNonterminalHistoryOnCancellationAndWatchLoss(t *testing.T) {
	for _, mode := range []string{"cancellation", "watch loss"} {
		t.Run(mode, func(t *testing.T) {
			pod := supervisedPod()
			store, _, resume := retainedProcess(t, pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time, false)
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			defer app.Close()
			resume.Plan = supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ready := make(chan struct{})
			var readyOnce sync.Once
			history := cycleStore{Store: store, observe: func(ctx context.Context, r state.ProcessObservation) (state.Process, bool, error) {
				if r.EverReady {
					readyOnce.Do(func() { close(ready) })
					if mode == "cancellation" {
						cancel()
					}
				}
				return store.RecordProcess(ctx, r)
			}}
			client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) {
				openWatch(w)
				select {
				case <-ready:
				case <-r.Context().Done():
				}
			})
			result, err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).ResumeDurable(ctx, history, cycleRequest(resume), cycleOptions())
			want := k3s.ErrObservationLost
			if mode == "cancellation" {
				want = context.Canceled
			}
			if !errors.Is(err, want) || result.Outcome != nil {
				t.Fatalf("nonterminal result became durable restart outcome: %#v %v", result, err)
			}
			p, err := store.GetProcess(context.Background(), resume.Scope, resume.SlotID, resume.GenerationID)
			if err != nil || !p.EverReady || p.Terminal != nil {
				t.Fatalf("nonterminal stop lost first readiness or invented failure: %#v %v", p, err)
			}
			if mode == "watch loss" {
				recovered, err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).ResumeDurable(ctx, history, cycleRequest(resume), cycleOptions())
				if !errors.Is(err, k3s.ErrObservationLost) || recovered.Outcome != nil {
					t.Fatalf("retained first readiness changed the replayed session payload: %#v %v", recovered, err)
				}
			}
		})
	}
}

func TestDurableSupervisionStopsProbesWhenHistoryWorkerFails(t *testing.T) {
	pod := supervisedPod()
	store, _, resume := retainedProcess(t, pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time, false)
	var calls, finishes atomic.Int32
	history := cycleStore{Store: store, observe: func(ctx context.Context, r state.ProcessObservation) (state.Process, bool, error) {
		if r.EverReady {
			return state.Process{}, false, state.ErrConflict
		}
		return store.RecordProcess(ctx, r)
	}, finish: func(context.Context, state.ProcessTerminalObservation) (state.Process, bool, error) {
		finishes.Add(1)
		return state.Process{}, false, nil
	}}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer app.Close()
	resume.Plan = supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
	stopped := make(chan struct{})
	client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) { defer close(stopped); openWatch(w); <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).ResumeDurable(ctx, history, cycleRequest(resume), cycleOptions())
	if !errors.Is(err, state.ErrConflict) || result.Outcome != nil || finishes.Load() != 0 || calls.Load() == 0 {
		t.Fatalf("persistence failure leaked a terminal outcome: %#v %v", result, err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("history failure left observation running")
	}
}

func TestDurableSupervisionBlocksTerminalWhenFlushOrTerminalWriteFails(t *testing.T) {
	for _, mode := range []string{"flush timeout", "terminal failure", "cancel during drain"} {
		t.Run(mode, func(t *testing.T) {
			pod := supervisedPod()
			store, _, resume := retainedProcess(t, pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time, false)
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			defer app.Close()
			resume.Plan = supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
			ready := make(chan struct{})
			var readyOnce sync.Once
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var terminalCalls atomic.Int32
			history := cycleStore{Store: store, observe: func(attempt context.Context, r state.ProcessObservation) (state.Process, bool, error) {
				if r.EverReady {
					readyOnce.Do(func() { close(ready) })
					if mode != "terminal failure" {
						<-attempt.Done()
						return state.Process{}, false, attempt.Err()
					}
				}
				return store.RecordProcess(attempt, r)
			}, finish: func(context.Context, state.ProcessTerminalObservation) (state.Process, bool, error) {
				terminalCalls.Add(1)
				return state.Process{}, false, state.ErrConflict
			}}
			client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) {
				openWatch(w)
				select {
				case <-ready:
				case <-r.Context().Done():
					return
				}
				changed := pod.DeepCopy()
				changed.ResourceVersion = "43"
				markExited(changed)
				watchEvent(t, w, "MODIFIED", changed)
				<-r.Context().Done()
				if mode == "cancel during drain" {
					cancel()
				}
			})
			options := cycleOptions()
			if mode != "terminal failure" {
				options.FinalizeTimeout = 20 * time.Millisecond
			}
			result, err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).ResumeDurable(ctx, history, cycleRequest(resume), options)
			if err == nil || result.Outcome != nil || result.Process.Terminal != nil {
				t.Fatalf("unacknowledged terminal was exposed: %#v %v", result, err)
			}
			if mode == "terminal failure" {
				if !errors.Is(err, state.ErrConflict) || terminalCalls.Load() != 1 {
					t.Fatalf("terminal conflict retried or lost: calls=%d %v", terminalCalls.Load(), err)
				}
			} else if terminalCalls.Load() != 0 {
				t.Fatal("failed drain allowed terminal I/O")
			}
		})
	}
}

func TestDurableSupervisionValidatesBeforeRuntimeAndWrites(t *testing.T) {
	for _, mode := range []string{"history", "history key", "terminal key", "bounds", "unsupported plan", "foreign instance", "unbound generation", "terminal generation", "pre-canceled", "no runtime client"} {
		t.Run(mode, func(t *testing.T) {
			store, _, request := terminalInput(t, false)
			resume := k3s.ResumeRequest{Scope: request.Process.Scope, SlotID: request.Process.SlotID, GenerationID: request.Process.GenerationID, Instance: request.Instance, Plan: request.Plan}
			r := cycleRequest(resume)
			options := cycleOptions()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			history := cycleStore{Store: store, observe: func(context.Context, state.ProcessObservation) (state.Process, bool, error) {
				t.Error("rejected cycle wrote history")
				return state.Process{}, false, state.ErrConflict
			}}
			var backend k3s.DurableSupervisionStore = history
			switch mode {
			case "history":
				backend = nil
			case "history key":
				r.HistoryKey = " "
			case "terminal key":
				r.TerminalKey = ""
			case "bounds":
				options.FinalizeTimeout = 0
			case "unsupported plan":
				r.Resume.Plan.Health.Check = nil
			case "foreign instance":
				r.Resume.Instance.ContainerID = "other"
			case "unbound generation":
				r.Resume.GenerationID = "missing"
			case "terminal generation":
				observation, err := k3s.PrepareProcessTerminal(request)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.RecordProcessTerminal(ctx, observation); err != nil {
					t.Fatal(err)
				}
			case "pre-canceled":
				cancel()
			}
			client := apiClient(t, func(w http.ResponseWriter, r *http.Request) {
				t.Error("rejected cycle contacted runtime")
				w.WriteHeader(500)
			})
			supervisor := k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}
			if mode == "no runtime client" {
				supervisor.Resolver.Client = nil
			}
			result, err := supervisor.ResumeDurable(ctx, backend, r, options)
			if err == nil || result.Outcome != nil {
				t.Fatalf("invalid cycle accepted: %#v %v", result, err)
			}
		})
	}
}

func TestDurableSupervisionInitialAcknowledgementHasItsWriteBudget(t *testing.T) {
	pod := supervisedPod()
	store, _, resume := retainedProcess(t, pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time, false)
	history := cycleStore{Store: store, observe: func(ctx context.Context, r state.ProcessObservation) (state.Process, bool, error) {
		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return state.Process{}, false, ctx.Err()
		}
		return store.RecordProcess(ctx, r)
	}}
	var runtimeCalls atomic.Int32
	client := observerClient(t, pod, func(w http.ResponseWriter, _ *http.Request) { runtimeCalls.Add(1); openWatch(w) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	options := cycleOptions()
	options.FinalizeTimeout = 20 * time.Millisecond
	_, err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).ResumeDurable(ctx, history, cycleRequest(resume), options)
	if runtimeCalls.Load() != 1 {
		t.Fatalf("final-drain budget prematurely ended initial acknowledgement: runtime calls=%d error=%v", runtimeCalls.Load(), err)
	}
}

func TestDurableSupervisionRetainsStartupFailureWithoutRestartOutcome(t *testing.T) {
	pod := supervisedPod()
	pod.Status.ContainerStatuses[1].State.Running.StartedAt = metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	store, _, resume := retainedProcess(t, pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time, false)
	client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) { openWatch(w); <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	supervisor := k3s.Supervisor{Resolver: k3s.Resolver{Client: client}, Network: probe.Network{DialContext: func(context.Context, string, string) (net.Conn, error) {
		t.Error("expired startup performed a probe")
		return nil, errors.New("unexpected probe")
	}}}
	result, err := supervisor.ResumeDurable(ctx, store, cycleRequest(resume), cycleOptions())
	var restart *k3s.RestartNeededError
	if err != nil || !errors.Is(result.Outcome, probe.ErrStartupTimeout) || errors.As(result.Outcome, &restart) || result.Process.Terminal == nil || result.Process.Terminal.Cause != state.ProcessStartupTimeout {
		t.Fatalf("startup outcome: %#v %v", result, err)
	}
}

func TestDurableSupervisionRequiresInitialHistoryAcknowledgementBeforeRuntime(t *testing.T) {
	pod := supervisedPod()
	store, _, resume := retainedProcess(t, pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time, false)
	history := cycleStore{Store: store, observe: func(context.Context, state.ProcessObservation) (state.Process, bool, error) {
		return state.Process{}, false, state.ErrConflict
	}}
	client := apiClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unacknowledged initial history started runtime observation")
		w.WriteHeader(http.StatusInternalServerError)
	})
	result, err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).ResumeDurable(context.Background(), history, cycleRequest(resume), cycleOptions())
	if !errors.Is(err, state.ErrConflict) || result.Outcome != nil || result.Process.GenerationID != "" {
		t.Fatalf("initial history failure exposed completion: %#v %v", result, err)
	}
}

func TestDurableSupervisionRejectsUnacknowledgedTerminalCompletion(t *testing.T) {
	for _, mode := range []string{"retry deadline", "canceled after commit", "missing acknowledgement"} {
		t.Run(mode, func(t *testing.T) {
			pod := supervisedPod()
			store, _, resume := retainedProcess(t, pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time, false)
			markExited(pod)
			client := observerClient(t, pod, func(http.ResponseWriter, *http.Request) { t.Error("exited process started a watch") })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			unavailable := errors.New("terminal storage unavailable")
			calls := 0
			history := cycleStore{Store: store, finish: func(attempt context.Context, r state.ProcessTerminalObservation) (state.Process, bool, error) {
				calls++
				switch mode {
				case "retry deadline":
					return state.Process{}, false, unavailable
				case "canceled after commit":
					p, recorded, err := store.RecordProcessTerminal(attempt, r)
					cancel()
					return p, recorded, err
				default:
					return state.Process{}, false, nil
				}
			}}
			options := cycleOptions()
			if mode == "retry deadline" {
				options.FinalizeTimeout = 100 * time.Millisecond
			}
			result, err := (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).ResumeDurable(ctx, history, cycleRequest(resume), options)
			if err == nil || result.Outcome != nil || result.Process.GenerationID != "" {
				t.Fatalf("ambiguous terminal exposed completion: %#v %v", result, err)
			}
			switch mode {
			case "retry deadline":
				if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, unavailable) || calls < 2 {
					t.Fatalf("terminal retries lost deadline or storage cause: calls=%d %v", calls, err)
				}
			case "canceled after commit":
				p, readErr := store.GetProcess(context.Background(), resume.Scope, resume.SlotID, resume.GenerationID)
				if !errors.Is(err, context.Canceled) || readErr != nil || p.Terminal == nil || calls != 1 {
					t.Fatalf("ambiguous commit did not require recovery: calls=%d %#v %v %v", calls, p, readErr, err)
				}
			case "missing acknowledgement":
				if !errors.Is(err, state.ErrConflict) || calls != 1 {
					t.Fatalf("missing terminal acknowledgement retried: calls=%d %v", calls, err)
				}
			}
		})
	}
}
