package k3s_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func publicationReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == k3s.ReadinessGate {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func publicationUpdate(t *testing.T, r *http.Request) (bool, bool) {
	t.Helper()
	if r.Method != http.MethodPut {
		return false, false
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
		return false, false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	var pod corev1.Pod
	if err := json.Unmarshal(data, &pod); err != nil {
		t.Error(err)
	}
	return publicationReady(&pod), true
}

func publicationCycle(t *testing.T, retainedReady bool) (*gateStore, *sqlite.Store, k3s.Supervisor, k3s.DurableSupervisionRequest, k3s.DurableSupervisionOptions) {
	t.Helper()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(app.Close)
	s := &gateStore{}
	client := gateClient(t, s)
	s.pod.Status.PodIP = "127.0.0.1"
	s.pod.Status.ContainerStatuses[1].State.Running.StartedAt = metav1.NewTime(time.Now().Truncate(time.Second))
	s.pod.Status.Conditions = append(s.pod.Status.Conditions, corev1.PodCondition{Type: k3s.ReadinessGate, Status: corev1.ConditionTrue})
	s.watch = func(w http.ResponseWriter, r *http.Request) { openWatch(w); <-r.Context().Done() }
	store, _, resume := retainedProcess(t, s.pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time, retainedReady)
	resume.Plan = supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
	options := cycleOptions()
	options.Readiness = &k3s.ReadinessPublication{SessionKey: t.Name(), Writes: k3s.ReporterOptions{AttemptTimeout: time.Second, RetryInterval: time.Millisecond}}
	return s, store, k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}, cycleRequest(resume), options
}

func waitPublication(t *testing.T, ctx context.Context, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-ctx.Done():
		t.Fatal("publication cycle did not reach its expected observation")
	}
}

func exitAfterPublication(t *testing.T, s *gateStore, ready <-chan struct{}, stopped chan<- struct{}) {
	t.Helper()
	s.watch = func(w http.ResponseWriter, r *http.Request) {
		defer close(stopped)
		pod, _, _ := s.snapshot()
		if publicationReady(pod) {
			t.Error("monitor started before initial unready acknowledgement")
		}
		openWatch(w)
		select {
		case <-ready:
		case <-r.Context().Done():
			return
		}
		s.mu.Lock()
		markExited(s.pod)
		s.pod.ResourceVersion = "exit-snapshot"
		changed := s.pod.DeepCopy()
		s.mu.Unlock()
		watchEvent(t, w, "MODIFIED", changed)
		<-r.Context().Done()
	}
}

func TestDurablePublicationDrainsBothReportersBeforeTerminal(t *testing.T) {
	for _, retainedReady := range []bool{false, true} {
		t.Run(map[bool]string{false: "first readiness", true: "retained readiness"}[retainedReady], func(t *testing.T) {
			s, store, supervisor, request, options := publicationCycle(t, retainedReady)
			ready, stopped, historyPending, finalPending := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			releaseHistory, releaseFinal := make(chan struct{}), make(chan struct{})
			var readyOnce, historyOnce, finalOnce sync.Once
			s.afterWrite = func(p *corev1.Pod) {
				if publicationReady(p) {
					readyOnce.Do(func() { close(ready) })
				}
			}
			s.beforeRequest = func(w http.ResponseWriter, r *http.Request) bool {
				if value, update := publicationUpdate(t, r); update && !value {
					select {
					case <-ready:
						finalOnce.Do(func() { close(finalPending) })
						select {
						case <-releaseFinal:
						case <-r.Context().Done():
							return false
						}
					default:
					}
				}
				return true
			}
			exitAfterPublication(t, s, ready, stopped)
			var terminalCalls atomic.Int32
			history := cycleStore{Store: store, observe: func(ctx context.Context, r state.ProcessObservation) (state.Process, bool, error) {
				if r.EverReady {
					historyOnce.Do(func() { close(historyPending) })
					select {
					case <-releaseHistory:
					case <-ctx.Done():
						return state.Process{}, false, ctx.Err()
					}
				}
				return store.RecordProcess(ctx, r)
			}, finish: func(ctx context.Context, r state.ProcessTerminalObservation) (state.Process, bool, error) {
				terminalCalls.Add(1)
				pod, _, _ := s.snapshot()
				p, err := store.GetProcess(ctx, r.Scope, r.SlotID, r.GenerationID)
				if err != nil || !p.EverReady || publicationReady(pod) {
					t.Error("terminal write overtook reporter acknowledgements")
				}
				return store.RecordProcessTerminal(ctx, r)
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				result, err := supervisor.ResumeDurable(ctx, history, request, options)
				var decision *k3s.RestartNeededError
				if err == nil && (!errors.As(result.Outcome, &decision) || result.Process.Terminal == nil || !result.Process.EverReady) {
					t.Error("completion lost acknowledged terminal outcome")
				}
				done <- err
			}()
			waitPublication(t, ctx, historyPending)
			waitPublication(t, ctx, stopped)
			waitPublication(t, ctx, finalPending)
			if terminalCalls.Load() != 0 {
				t.Fatal("terminal overtook blocked reporters")
			}
			close(releaseHistory)
			select {
			case err := <-done:
				t.Fatalf("completion overtook final unready: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			close(releaseFinal)
			if err := <-done; err != nil || terminalCalls.Load() != 1 {
				t.Fatalf("published durable cycle: terminal calls=%d %v", terminalCalls.Load(), err)
			}
		})
	}
}

func TestDurablePublicationCancellationPreservesHistoryAndRevokes(t *testing.T) {
	s, store, supervisor, request, options := publicationCycle(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.afterWrite = func(p *corev1.Pod) {
		if publicationReady(p) {
			cancel()
		}
	}
	result, err := supervisor.ResumeDurable(ctx, store, request, options)
	pod, _, _ := s.snapshot()
	p, readErr := store.GetProcess(context.Background(), request.Resume.Scope, request.Resume.SlotID, request.Resume.GenerationID)
	if !errors.Is(err, context.Canceled) || result.Outcome != nil || readErr != nil || !p.EverReady || p.Terminal != nil || publicationReady(pod) {
		t.Fatalf("cancellation did not drain both histories: result=%#v process=%#v gate=%t error=%v read=%v", result, p, publicationReady(pod), err, readErr)
	}
}

func TestDurablePublicationWorkerFailureStopsMonitoring(t *testing.T) {
	for _, mode := range []string{"history", "publication"} {
		t.Run(mode, func(t *testing.T) {
			s, store, supervisor, request, options := publicationCycle(t, false)
			stored, stopped := make(chan struct{}), make(chan struct{})
			var storedOnce sync.Once
			history := cycleStore{Store: store, observe: func(ctx context.Context, r state.ProcessObservation) (state.Process, bool, error) {
				if r.EverReady && mode == "history" {
					return state.Process{}, false, state.ErrConflict
				}
				p, recorded, err := store.RecordProcess(ctx, r)
				if r.EverReady && err == nil {
					storedOnce.Do(func() { close(stored) })
				}
				return p, recorded, err
			}}
			s.beforeRequest = func(w http.ResponseWriter, r *http.Request) bool {
				if ready, update := publicationUpdate(t, r); update && ready && mode == "publication" {
					select {
					case <-stored:
					case <-r.Context().Done():
						return false
					}
					apiStatus(w, http.StatusForbidden)
					return false
				}
				return true
			}
			s.watch = func(w http.ResponseWriter, r *http.Request) { defer close(stopped); openWatch(w); <-r.Context().Done() }
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := supervisor.ResumeDurable(ctx, history, request, options)
			if err == nil || result.Outcome != nil {
				t.Fatalf("worker failure exposed completion: %#v %v", result, err)
			}
			if mode == "publication" && !apierrors.IsForbidden(err) || mode == "history" && !errors.Is(err, state.ErrConflict) {
				t.Fatalf("worker failure lost cause: %v", err)
			}
			waitPublication(t, ctx, stopped)
			pod, _, _ := s.snapshot()
			p, readErr := store.GetProcess(context.Background(), request.Resume.Scope, request.Resume.SlotID, request.Resume.GenerationID)
			if publicationReady(pod) || readErr != nil || p.Terminal != nil || mode == "publication" && !p.EverReady {
				t.Fatalf("worker failure left unsafe state: %#v %v", p, readErr)
			}
		})
	}
}

func TestDurablePublicationFinalAcknowledgementGatesCompletion(t *testing.T) {
	for _, mode := range []string{"deadline", "forbidden", "cancel during drain"} {
		t.Run(mode, func(t *testing.T) {
			s, store, supervisor, request, options := publicationCycle(t, false)
			ready, stopped := make(chan struct{}), make(chan struct{})
			var once sync.Once
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.afterWrite = func(p *corev1.Pod) {
				if publicationReady(p) {
					once.Do(func() { close(ready) })
				}
			}
			s.beforeRequest = func(w http.ResponseWriter, r *http.Request) bool {
				if value, update := publicationUpdate(t, r); update && !value {
					select {
					case <-ready:
						switch mode {
						case "deadline":
							apiStatus(w, http.StatusServiceUnavailable)
							return false
						case "forbidden":
							apiStatus(w, http.StatusForbidden)
							return false
						default:
							cancel()
						}
					default:
					}
				}
				return true
			}
			exitAfterPublication(t, s, ready, stopped)
			if mode == "deadline" {
				options.FinalizeTimeout = 50 * time.Millisecond
			}
			result, err := supervisor.ResumeDurable(ctx, store, request, options)
			p, readErr := store.GetProcess(context.Background(), request.Resume.Scope, request.Resume.SlotID, request.Resume.GenerationID)
			if err == nil || result.Outcome != nil || result.Process.GenerationID != "" || readErr != nil || p.Terminal != nil {
				t.Fatalf("unacknowledged publication exposed terminal: %#v %#v %v %v", result, p, err, readErr)
			}
			if mode == "deadline" && !errors.Is(err, context.DeadlineExceeded) || mode == "forbidden" && !apierrors.IsForbidden(err) || mode == "cancel during drain" && !errors.Is(err, context.Canceled) {
				t.Fatalf("finalization lost cause: %v", err)
			}
			if mode == "cancel during drain" {
				pod, _, _ := s.snapshot()
				if publicationReady(pod) {
					t.Fatal("cancellation discarded final unready")
				}
			}
		})
	}
}

func TestDurablePublicationRejectsInvalidOptionsBeforeWrites(t *testing.T) {
	for _, mode := range []string{"session", "attempt", "retry", "pre-canceled"} {
		t.Run(mode, func(t *testing.T) {
			s, store, supervisor, request, options := publicationCycle(t, false)
			s.beforeRequest = func(http.ResponseWriter, *http.Request) bool {
				t.Error("invalid publication contacted runtime")
				return false
			}
			history := cycleStore{Store: store, observe: func(context.Context, state.ProcessObservation) (state.Process, bool, error) {
				t.Error("invalid publication wrote history")
				return state.Process{}, false, state.ErrConflict
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "session":
				options.Readiness.SessionKey = " "
			case "attempt":
				options.Readiness.Writes.AttemptTimeout = 0
			case "retry":
				options.Readiness.Writes.RetryInterval = 0
			case "pre-canceled":
				cancel()
			}
			if result, err := supervisor.ResumeDurable(ctx, history, request, options); err == nil || result.Outcome != nil {
				t.Fatalf("invalid publication accepted: %#v %v", result, err)
			}
		})
	}
}

func TestDurablePublicationInitialAcknowledgementsPrecedeMonitor(t *testing.T) {
	for _, mode := range []string{"history", "publication"} {
		t.Run(mode, func(t *testing.T) {
			s, store, supervisor, request, options := publicationCycle(t, false)
			history := cycleStore{Store: store}
			if mode == "history" {
				history.observe = func(context.Context, state.ProcessObservation) (state.Process, bool, error) {
					return state.Process{}, false, state.ErrConflict
				}
			} else {
				s.getStatus = http.StatusForbidden
			}
			s.watch = func(http.ResponseWriter, *http.Request) { t.Error("failed initial acknowledgement started monitor") }
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := supervisor.ResumeDurable(ctx, history, request, options)
			if err == nil || result.Outcome != nil {
				t.Fatalf("initial acknowledgement failure exposed completion: %#v %v", result, err)
			}
			_, _, requests := s.snapshot()
			if mode == "history" && requests != 0 {
				t.Fatal("failed history acknowledgement started publication")
			}
		})
	}
}

func TestDurablePublicationInitialWriteUsesItsOwnBudget(t *testing.T) {
	s, store, supervisor, request, options := publicationCycle(t, false)
	s.beforeRequest = func(_ http.ResponseWriter, r *http.Request) bool {
		if _, update := publicationUpdate(t, r); update {
			timer := time.NewTimer(50 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return false
			}
		}
		return true
	}
	var watches atomic.Int32
	s.watch = func(w http.ResponseWriter, _ *http.Request) { watches.Add(1); openWatch(w) }
	options.FinalizeTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := supervisor.ResumeDurable(ctx, store, request, options)
	if watches.Load() != 1 {
		t.Fatalf("finalization budget cut off initial publication: watches=%d %v", watches.Load(), err)
	}
}

func TestDurablePublicationStopsBeforeFirstReadiness(t *testing.T) {
	for _, mode := range []string{"exit", "watch loss"} {
		t.Run(mode, func(t *testing.T) {
			s, store, supervisor, request, options := publicationCycle(t, false)
			supervisor.Network = probe.Network{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }}
			if mode == "exit" {
				markExited(s.pod)
				s.pod.Status.PodIP = ""
				s.watch = func(http.ResponseWriter, *http.Request) { t.Error("already exited process started a watch") }
			} else {
				s.watch = func(w http.ResponseWriter, _ *http.Request) { openWatch(w) }
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := supervisor.ResumeDurable(ctx, store, request, options)
			pod, _, _ := s.snapshot()
			p, readErr := store.GetProcess(context.Background(), request.Resume.Scope, request.Resume.SlotID, request.Resume.GenerationID)
			if readErr != nil || p.EverReady || publicationReady(pod) {
				t.Fatalf("early stop invented readiness or left gate open: %#v %v", p, readErr)
			}
			if mode == "exit" {
				if err != nil || !errors.Is(result.Outcome, k3s.ErrInstanceExited) || p.Terminal == nil || p.Terminal.Cause != state.ProcessExited {
					t.Fatalf("early exit lost acknowledged terminal: %#v %v", result, err)
				}
			} else if !errors.Is(err, k3s.ErrObservationLost) || result.Outcome != nil || p.Terminal != nil {
				t.Fatalf("watch loss invented terminal: %#v %v", result, err)
			}
		})
	}
}
