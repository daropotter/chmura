package probe

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
)

func TestReadinessStartupUsesCompletionBeforeNotification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		duration  time.Duration
		outcome   error
		successes manifest.Threshold
		prior     bool
		wantReady bool
	}{
		{"timely success", 49 * time.Millisecond, nil, 1, false, true},
		{"final threshold success", 49 * time.Millisecond, nil, 2, true, true},
		{"partial success", 49 * time.Millisecond, nil, 2, false, false},
		{"failure", 49 * time.Millisecond, errors.New("not ready"), 1, false, false},
		{"at deadline", 50 * time.Millisecond, nil, 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				config := &manifest.Health{
					Check: &manifest.Probe{Process: &manifest.ProcessCheck{Name: "app"},
						Interval: manifest.Duration(time.Millisecond), Timeout: manifest.Duration(time.Second)},
					StartupTimeout: manifest.Duration(50 * time.Millisecond),
					Ready:          manifest.ReadyRule{Successes: tc.successes},
				}
				// Intercept the scheduler's notification after it records completion.
				// The monitor cannot receive it until after its startup deadline.
				notifications, intercepted := make(chan struct{}), make(chan struct{})
				results := &completion{notify: intercepted, ready: notifications}
				updates, done := make(chan Readiness, 8), make(chan error, 1)
				go func() {
					calls := 0
					done <- monitorReadiness(ctx, time.Now(), config, func(context.Context, manifest.Probe) error {
						calls++
						if tc.prior && calls == 1 {
							return nil
						}
						duration := tc.duration
						if tc.prior {
							duration -= time.Millisecond
						}
						time.Sleep(duration)
						return tc.outcome
					}, func(state Readiness) { updates <- state }, results)
				}()
				<-updates // Initial not-ready snapshot.
				if tc.prior {
					<-intercepted
					notifications <- struct{}{}
				}
				if tc.duration < 50*time.Millisecond {
					<-intercepted
					time.Sleep(50*time.Millisecond - tc.duration)
				} else {
					time.Sleep(50 * time.Millisecond)
				}
				synctest.Wait()
				state := <-updates
				if state.Ready != tc.wantReady || state.EverReady != tc.wantReady || state.StartupFailed == tc.wantReady {
					t.Errorf("startup state with delayed notification = %#v, want ready=%v", state, tc.wantReady)
				}
				if state.Ready {
					// Deliver the stale notification too: it must not apply the
					// same completion again or block future cancellation.
					notifications <- struct{}{}
					synctest.Wait()
					select {
					case duplicate := <-updates:
						t.Errorf("completion was applied twice: %#v", duplicate)
					default:
					}
					cancel()
				}
				err := <-done
				if tc.wantReady {
					if !errors.Is(err, context.Canceled) {
						t.Errorf("ready monitor stop = %v", err)
					}
				} else if !errors.Is(err, ErrStartupTimeout) {
					t.Errorf("startup error = %v", err)
				}
			})
		})
	}
}
