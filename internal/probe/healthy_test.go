package probe_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
)

func healthyConfig() *manifest.Health {
	return &manifest.Health{
		Check: &manifest.Probe{Process: &manifest.ProcessCheck{Name: "readiness-only"}},
		Healthy: &manifest.HealthyRule{UnhealthyFailures: 3, Check: &manifest.Probe{
			Exec:     &manifest.ExecCheck{Command: manifest.Command{"/bin/self-check"}},
			Interval: manifest.Duration(10 * time.Millisecond),
		}},
	}
}

func TestHealthyWaitsForFirstReadinessAndResetsFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		firstReady := make(chan struct{})
		done := make(chan error, 1)
		failure := errors.New("wedged")
		outcomes := []error{failure, failure, nil, failure, failure, failure}
		var calls atomic.Int32
		go func() {
			done <- probe.MonitorHealthy(ctx, firstReady, healthyConfig(), func(_ context.Context, check manifest.Probe) error {
				if check.Exec == nil || check.Exec.Command[0] != "/bin/self-check" || check.Process != nil {
					t.Error("restart monitor did not use its own probe")
				}
				i := int(calls.Add(1)) - 1
				if i >= len(outcomes) {
					t.Error("monitor continued after the unhealthy threshold")
					return failure
				}
				return outcomes[i]
			})
		}()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if calls.Load() != 0 {
			t.Fatal("healthy probe ran before first readiness")
		}
		started := time.Now()
		close(firstReady)
		if err := <-done; !errors.Is(err, probe.ErrUnhealthy) || calls.Load() != 6 || time.Since(started) != 50*time.Millisecond {
			t.Fatalf("unhealthy decision: error=%v calls=%d elapsed=%v", err, calls.Load(), time.Since(started))
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if calls.Load() != 6 {
			t.Fatal("monitor ran another attempt after its terminal decision")
		}
	})
}

func TestHealthyActivatesAfterFullReadinessAndContinuesAfterReadinessLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		config := healthyConfig()
		config.Check.Interval = manifest.Duration(10 * time.Millisecond)
		config.Ready = manifest.ReadyRule{Successes: 2, UnreadyFailures: 1}
		config.Healthy.UnhealthyFailures = 2
		config.Healthy.Check.Interval = manifest.Duration(20 * time.Millisecond)
		firstReady := make(chan struct{})
		updates := make(chan probe.Readiness, 8)
		readinessDone, healthyDone := make(chan error, 1), make(chan error, 1)
		var readyCalls, healthyCalls atomic.Int32
		var activated sync.Once
		started := time.Now()
		go func() {
			readinessDone <- probe.MonitorReadiness(ctx, started, config, func(context.Context, manifest.Probe) error {
				if readyCalls.Add(1) <= 2 {
					return nil
				}
				return errors.New("dependency unavailable")
			}, func(state probe.Readiness) {
				if state.EverReady {
					activated.Do(func() { close(firstReady) })
				}
				updates <- state
			})
		}()
		go func() {
			healthyDone <- probe.MonitorHealthy(ctx, firstReady, config, func(context.Context, manifest.Probe) error {
				healthyCalls.Add(1)
				return errors.New("local check failed")
			})
		}()
		<-updates
		synctest.Wait()
		if readyCalls.Load() != 1 || healthyCalls.Load() != 0 {
			t.Fatal("a partial readiness streak activated healthy")
		}
		if state := <-updates; !state.Ready || time.Since(started) != 10*time.Millisecond {
			t.Fatalf("first readiness = %#v at %v", state, time.Since(started))
		}
		if state := <-updates; state.Ready || !state.EverReady || time.Since(started) != 20*time.Millisecond {
			t.Fatalf("readiness loss = %#v at %v", state, time.Since(started))
		}
		if err := <-healthyDone; !errors.Is(err, probe.ErrUnhealthy) || healthyCalls.Load() != 2 || time.Since(started) != 30*time.Millisecond {
			t.Fatalf("healthy stopped or reset after readiness loss: error=%v calls=%d elapsed=%v", err, healthyCalls.Load(), time.Since(started))
		}
		cancel()
		if err := <-readinessDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("readiness stop = %v", err)
		}
	})
}

func TestHealthyDoesNotEnableOrInheritAProbe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		health *manifest.Health
	}{
		{"no health", nil},
		{"no healthy", &manifest.Health{Check: &manifest.Probe{Process: &manifest.ProcessCheck{Name: "app"}}}},
		{"process-only", &manifest.Health{Check: &manifest.Probe{Process: &manifest.ProcessCheck{Name: "app"}}, Healthy: &manifest.HealthyRule{UnhealthyFailures: 2}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := probe.MonitorHealthy(ctx, nil, tc.health, nil); err != nil {
					t.Fatalf("no additional probe should be scheduled: %v", err)
				}
			})
		})
	}
}

func TestHealthyTimeoutCountsAsFailureIncludingNilAtDeadline(t *testing.T) {
	for _, mode := range []string{"honor cancellation", "nil at deadline"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				firstReady := make(chan struct{})
				close(firstReady)
				config := healthyConfig()
				config.Healthy.UnhealthyFailures = 2
				config.Healthy.Check.Interval = manifest.Duration(5 * time.Millisecond)
				config.Healthy.Check.Timeout = manifest.Duration(20 * time.Millisecond)
				calls := 0
				started := time.Now()
				err := probe.MonitorHealthy(ctx, firstReady, config, func(ctx context.Context, _ manifest.Probe) error {
					calls++
					if mode == "nil at deadline" {
						time.Sleep(20 * time.Millisecond)
						return nil
					}
					<-ctx.Done()
					return ctx.Err()
				})
				if !errors.Is(err, probe.ErrUnhealthy) || calls != 2 || time.Since(started) != 45*time.Millisecond {
					t.Fatalf("timeout decision: error=%v calls=%d elapsed=%v", err, calls, time.Since(started))
				}
			})
		})
	}
}

func TestHealthyResolvesOwnDefaultsWithoutChangingManifest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		config := healthyConfig()
		config.Check.Timeout = manifest.Duration(time.Nanosecond)
		config.Healthy.UnhealthyFailures = 1
		config.Healthy.Check = &manifest.Probe{HTTP: &manifest.HTTPCheck{Path: "/local", Port: "http"}}
		firstReady := make(chan struct{})
		close(firstReady)
		started := time.Now()
		err := probe.MonitorHealthy(ctx, firstReady, config, func(ctx context.Context, check manifest.Probe) error {
			if check.HTTP == nil || check.HTTP.Path != "/local" || len(check.HTTP.ExpectStatus) != 1 || check.HTTP.ExpectStatus[0] != 200 || check.Interval != manifest.Duration(5*time.Second) || check.Timeout != manifest.Duration(2*time.Second) {
				t.Errorf("unresolved healthy probe = %#v", check)
			}
			<-ctx.Done()
			return nil
		})
		if !errors.Is(err, probe.ErrUnhealthy) || time.Since(started) != 2*time.Second {
			t.Fatalf("own timeout not applied: error=%v elapsed=%v", err, time.Since(started))
		}
		if check := config.Healthy.Check; check.Interval != 0 || check.Timeout != 0 || check.HTTP.ExpectStatus != nil {
			t.Fatal("monitor changed declared parameters")
		}
	})
}

func TestHealthyParentStopWaitsForCleanupWithoutUnhealthyDecision(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				if mode == "deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
				}
				defer cancel()
				firstReady := make(chan struct{})
				close(firstReady)
				config := healthyConfig()
				config.Healthy.UnhealthyFailures = 1
				canceled, release := make(chan struct{}), make(chan struct{})
				done := make(chan error, 1)
				go func() {
					done <- probe.MonitorHealthy(ctx, firstReady, config, func(ctx context.Context, _ manifest.Probe) error {
						<-ctx.Done()
						close(canceled)
						<-release
						return nil
					})
				}()
				synctest.Wait()
				if mode == "cancel" {
					cancel()
				}
				<-canceled
				synctest.Wait()
				select {
				case err := <-done:
					t.Errorf("monitor abandoned active cleanup: %v", err)
					close(release)
					return
				default:
				}
				close(release)
				if err := <-done; !errors.Is(err, ctx.Err()) || errors.Is(err, probe.ErrUnhealthy) {
					t.Fatalf("parent stop became an unhealthy decision: %v", err)
				}
			})
		})
	}
}

func TestHealthyCancellationBeforeActivationOrDuringInterval(t *testing.T) {
	for _, mode := range []string{"waiting", "already canceled", "interval"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				firstReady := make(chan struct{})
				if mode != "waiting" {
					close(firstReady)
				}
				if mode == "already canceled" {
					cancel()
				}
				var calls atomic.Int32
				done := make(chan error, 1)
				go func() {
					done <- probe.MonitorHealthy(ctx, firstReady, healthyConfig(), func(context.Context, manifest.Probe) error { calls.Add(1); return nil })
				}()
				synctest.Wait()
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("stop error = %v", err)
				}
				want := int32(0)
				if mode == "interval" {
					want = 1
				}
				if calls.Load() != want {
					t.Fatalf("attempts = %d, want %d", calls.Load(), want)
				}
			})
		})
	}
}

func TestHealthyRejectsInvalidActiveConfiguration(t *testing.T) {
	for _, mode := range []string{"no activation", "no attempt", "zero threshold", "negative threshold", "negative interval", "negative timeout"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				config := healthyConfig()
				firstReady := make(chan struct{})
				close(firstReady)
				attempt := func(context.Context, manifest.Probe) error { t.Error("invalid monitor ran"); return nil }
				switch mode {
				case "no activation":
					firstReady = nil
				case "no attempt":
					attempt = nil
				case "zero threshold":
					config.Healthy.UnhealthyFailures = 0
				case "negative threshold":
					config.Healthy.UnhealthyFailures = -1
				case "negative interval":
					config.Healthy.Check.Interval = -1
				case "negative timeout":
					config.Healthy.Check.Timeout = -1
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := probe.MonitorHealthy(ctx, firstReady, config, attempt); err == nil || errors.Is(err, probe.ErrUnhealthy) || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("invalid configuration = %v", err)
				}
			})
		})
	}
}
