package k3s_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/supervision"
)

func TestSlotRunnerOwnsDurableSupervisionUntilDrainAndAcknowledgement(t *testing.T) {
	for _, mode := range []string{"terminal acknowledgement", "cancellation drain"} {
		t.Run(mode, func(t *testing.T) {
			pod := supervisedPod()
			store, _, resume := retainedProcess(t, pod.Status.ContainerStatuses[1].State.Running.StartedAt.Time, false)
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			defer app.Close()
			resume.Plan = supervisionPlan(app.Listener.Addr().(*net.TCPAddr).Port)
			blocked, release, watchStopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var blockedOnce, releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			history := cycleStore{Store: store}
			if mode == "cancellation drain" {
				history.observe = func(ctx context.Context, request state.ProcessObservation) (state.Process, bool, error) {
					if request.EverReady {
						blockedOnce.Do(func() { close(blocked) })
						select {
						case <-release:
						case <-ctx.Done():
							return state.Process{}, false, ctx.Err()
						}
					}
					return store.RecordProcess(ctx, request)
				}
			} else {
				history.finish = func(ctx context.Context, request state.ProcessTerminalObservation) (state.Process, bool, error) {
					p, changed, err := store.RecordProcessTerminal(ctx, request)
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
			}
			client := observerClient(t, pod, func(w http.ResponseWriter, r *http.Request) {
				defer close(watchStopped)
				openWatch(w)
				if mode == "terminal acknowledgement" {
					exited := pod.DeepCopy()
					exited.ResourceVersion = "exited"
					markExited(exited)
					watchEvent(t, w, "MODIFIED", exited)
				}
				<-r.Context().Done()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var runner supervision.SlotRunner
			type completion struct {
				result k3s.DurableSupervisionResult
				err    error
			}
			done := make(chan completion, 1)
			go func() {
				var result k3s.DurableSupervisionResult
				err := runner.Run(ctx, resume.Scope, resume.SlotID, func(ctx context.Context) error {
					options := cycleOptions()
					options.FinalizeTimeout, options.Writes.AttemptTimeout = 3*time.Second, 2*time.Second
					var err error
					result, err = (k3s.Supervisor{Resolver: k3s.Resolver{Client: client}}).ResumeDurable(ctx, history, cycleRequest(resume), options)
					return err
				})
				done <- completion{result, err}
			}()
			select {
			case <-blocked:
			case got := <-done:
				t.Fatalf("supervision ended before blocked acknowledgement: %v", got.err)
			case <-ctx.Done():
				t.Fatal("supervision did not reach acknowledgement")
			}
			if mode == "cancellation drain" {
				cancel()
			}
			select {
			case <-watchStopped:
			case <-time.After(time.Second):
				t.Fatal("old observation did not join")
			}
			calls := 0
			next := func(context.Context) error {
				calls++
				if mode == "terminal acknowledgement" {
					admitResumeGeneration(t, store, k3s.ResumeRequest{Scope: resume.Scope, SlotID: resume.SlotID, GenerationID: "generation-b"}, 1)
				}
				return nil
			}
			if err := runner.Run(context.Background(), resume.Scope, resume.SlotID, next); !errors.Is(err, state.ErrConflict) || calls != 0 {
				t.Fatalf("new action overtook acknowledgement/drain: %v calls=%d", err, calls)
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case got := <-done:
				if mode == "cancellation drain" {
					if !errors.Is(got.err, context.Canceled) || got.result.Outcome != nil {
						t.Fatalf("cancellation exposed terminal work: %#v %v", got.result, got.err)
					}
				} else if got.err != nil || got.result.Process.Terminal == nil {
					t.Fatalf("terminal acknowledgement lost: %#v %v", got.result, got.err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("old owner did not finish")
			}
			p, err := store.GetProcess(context.Background(), resume.Scope, resume.SlotID, resume.GenerationID)
			if err != nil || (mode == "cancellation drain" && (!p.EverReady || p.Terminal != nil)) || (mode == "terminal acknowledgement" && p.Terminal == nil) {
				t.Fatalf("drained history incorrect: %#v %v", p, err)
			}
			if err := runner.Run(context.Background(), resume.Scope, resume.SlotID, next); err != nil || calls != 1 {
				t.Fatalf("drained slot could not progress: %v calls=%d", err, calls)
			}
		})
	}
}
