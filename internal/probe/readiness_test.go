package probe_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
)

func readinessConfig() *manifest.Health {
	return &manifest.Health{Check: &manifest.Probe{
		Process: &manifest.ProcessCheck{Name: "app"}, Interval: manifest.Duration(time.Second),
	}}
}

func TestReadinessConsecutiveThresholdsAndRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		failure := errors.New("probe failed")
		outcomes := []error{nil, failure, nil, nil, failure, nil, failure, failure, nil, failure, nil, nil}
		config := readinessConfig()
		config.Ready = manifest.ReadyRule{Successes: 2, UnreadyFailures: 2}
		config.Healthy = &manifest.HealthyRule{UnhealthyFailures: 1, Check: &manifest.Probe{Exec: &manifest.ExecCheck{Command: manifest.Command{"/bin/self-check"}}}}
		type event struct {
			state   probe.Readiness
			attempt int32
		}
		updates, done := make(chan event, 16), make(chan error, 1)
		var calls atomic.Int32
		go func() {
			done <- probe.MonitorReadiness(ctx, time.Now(), config, func(ctx context.Context, check manifest.Probe) error {
				if check.Process == nil || check.Process.Name != "app" {
					t.Error("readiness monitor ran the restart probe")
				}
				i := int(calls.Add(1)) - 1
				if i < len(outcomes) {
					return outcomes[i]
				}
				<-ctx.Done()
				return ctx.Err()
			}, func(state probe.Readiness) { updates <- event{state, calls.Load()} })
		}()
		for _, want := range []event{
			{probe.Readiness{}, 0},
			{probe.Readiness{Ready: true, EverReady: true}, 4},
			{probe.Readiness{EverReady: true}, 8},
			{probe.Readiness{Ready: true, EverReady: true}, 12},
		} {
			select {
			case got := <-updates:
				if got != want {
					t.Fatalf("readiness event = %#v, want %#v", got, want)
				}
			case err := <-done:
				t.Fatalf("monitor stopped before readiness transition: %v", err)
			}
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("stop error = %v", err)
		}
	})
}

func TestReadinessAlreadyCanceledDoesNotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := probe.MonitorReadiness(ctx, time.Now(), readinessConfig(), func(context.Context, manifest.Probe) error { called = true; return nil }, func(probe.Readiness) { called = true })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled monitor: called=%v error=%v", called, err)
	}
}

func TestReadinessAttemptCompletingAtItsDeadlineIsFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		config := readinessConfig()
		config.Check.Timeout = manifest.Duration(20 * time.Millisecond)
		config.StartupTimeout = manifest.Duration(50 * time.Millisecond)
		var last probe.Readiness
		started := time.Now()
		err := probe.MonitorReadiness(ctx, started, config, func(context.Context, manifest.Probe) error { time.Sleep(20 * time.Millisecond); return nil }, func(state probe.Readiness) { last = state })
		if !errors.Is(err, probe.ErrStartupTimeout) || !last.StartupFailed || last.EverReady || time.Since(started) != 50*time.Millisecond {
			t.Fatalf("deadline completion counted as readiness: state=%#v error=%v elapsed=%v", last, err, time.Since(started))
		}
	})
}

func TestReadinessDefaultsAndNoStartupDeadlineAfterFirstReadiness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type event struct {
			state   probe.Readiness
			attempt int32
		}
		updates, done := make(chan event, 16), make(chan error, 1)
		var calls atomic.Int32
		go func() {
			done <- probe.MonitorReadiness(ctx, time.Now(), readinessConfig(), func(context.Context, manifest.Probe) error {
				if calls.Add(1) == 1 {
					return nil
				}
				return errors.New("unready")
			}, func(state probe.Readiness) { updates <- event{state, calls.Load()} })
		}()
		for _, want := range []event{
			{probe.Readiness{}, 0},
			{probe.Readiness{Ready: true, EverReady: true}, 1},
			{probe.Readiness{EverReady: true}, 4},
		} {
			select {
			case got := <-updates:
				if got != want {
					t.Fatalf("default readiness event = %#v, want %#v", got, want)
				}
			case err := <-done:
				t.Fatalf("monitor stopped: %v", err)
			}
		}
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("startup deadline was rearmed after lost readiness: %v", err)
		default:
		}
		select {
		case state := <-updates:
			t.Errorf("unchanged readiness was republished: %#v", state)
		default:
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("stop error = %v", err)
		}
	})
}

func TestReadinessStartupBudgetStartsAtProcessStart(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		timeout, age, remaining time.Duration
	}{
		{"default", 0, 0, 2 * time.Minute},
		{"already running", 0, 30 * time.Second, 90 * time.Second},
		{"override", 20 * time.Millisecond, 5 * time.Millisecond, 15 * time.Millisecond},
		{"already expired", 0, 3 * time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				config := readinessConfig()
				config.StartupTimeout = manifest.Duration(tc.timeout)
				started := time.Now()
				var calls atomic.Int32
				var last probe.Readiness
				err := probe.MonitorReadiness(context.Background(), started.Add(-tc.age), config, func(ctx context.Context, _ manifest.Probe) error {
					calls.Add(1)
					<-ctx.Done()
					return ctx.Err()
				}, func(state probe.Readiness) { last = state })
				if !errors.Is(err, probe.ErrStartupTimeout) || time.Since(started) != tc.remaining {
					t.Fatalf("startup result: error=%v duration=%v, want %v", err, time.Since(started), tc.remaining)
				}
				if !last.StartupFailed || last.Ready || last.EverReady {
					t.Fatalf("startup state = %#v", last)
				}
				if tc.remaining == 0 && calls.Load() != 0 {
					t.Fatal("expired startup budget started a probe")
				}
			})
		})
	}
}

func TestReadinessPartialSuccessDoesNotDisableStartupDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := readinessConfig()
		config.Check.Interval = manifest.Duration(5 * time.Second)
		config.Ready.Successes = 2
		config.StartupTimeout = manifest.Duration(2 * time.Second)
		var last probe.Readiness
		var calls atomic.Int32
		started := time.Now()
		err := probe.MonitorReadiness(context.Background(), started, config, func(context.Context, manifest.Probe) error { calls.Add(1); return nil }, func(state probe.Readiness) { last = state })
		if !errors.Is(err, probe.ErrStartupTimeout) || time.Since(started) != 2*time.Second || calls.Load() != 1 || !last.StartupFailed || last.EverReady {
			t.Fatalf("partial success bypassed startup limit: state=%#v calls=%d error=%v", last, calls.Load(), err)
		}
	})
}

func TestReadinessStartupTimeoutWaitsForActiveAttemptCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := readinessConfig()
		config.StartupTimeout = manifest.Duration(20 * time.Millisecond)
		config.Check.Timeout = manifest.Duration(time.Second)
		canceled, release := make(chan struct{}), make(chan struct{})
		updates, done := make(chan probe.Readiness, 8), make(chan error, 1)
		go func() {
			done <- probe.MonitorReadiness(context.Background(), time.Now(), config, func(ctx context.Context, _ manifest.Probe) error {
				<-ctx.Done()
				close(canceled)
				<-release
				return ctx.Err()
			}, func(state probe.Readiness) { updates <- state })
		}()
		if state := <-updates; state != (probe.Readiness{}) {
			t.Fatalf("initial state = %#v", state)
		}
		if state := <-updates; !state.StartupFailed {
			t.Fatalf("startup failure state = %#v", state)
		}
		<-canceled
		synctest.Wait()
		returnedEarly := false
		select {
		case err := <-done:
			returnedEarly = true
			t.Errorf("monitor abandoned active cleanup: %v", err)
		default:
		}
		close(release)
		if !returnedEarly {
			if err := <-done; !errors.Is(err, probe.ErrStartupTimeout) {
				t.Fatalf("startup error = %v", err)
			}
		}
	})
}

func TestReadinessParentCancellationIsNotStartupFailure(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				if mode == "deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
				}
				defer cancel()
				updates, done := make(chan probe.Readiness, 8), make(chan error, 1)
				go func() {
					done <- probe.MonitorReadiness(ctx, time.Now(), readinessConfig(), func(ctx context.Context, _ manifest.Probe) error { <-ctx.Done(); return ctx.Err() }, func(state probe.Readiness) { updates <- state })
				}()
				<-updates
				synctest.Wait()
				if mode == "cancel" {
					cancel()
				}
				err := <-done
				if !errors.Is(err, ctx.Err()) || errors.Is(err, probe.ErrStartupTimeout) {
					t.Fatalf("parent stop = %v, want %v", err, ctx.Err())
				}
				select {
				case state := <-updates:
					t.Errorf("parent stop changed readiness: %#v", state)
				default:
				}
			})
		})
	}
}

func TestReadinessPassesResolvedProbeAndAppliesAttemptTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := &manifest.Health{Check: &manifest.Probe{HTTP: &manifest.HTTPCheck{Path: "/ready", Port: "http"}}}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls atomic.Int32
		updates, done := make(chan probe.Readiness, 8), make(chan error, 1)
		go func() {
			done <- probe.MonitorReadiness(ctx, time.Now(), config, func(ctx context.Context, check manifest.Probe) error {
				if check.Interval != manifest.Duration(5*time.Second) || check.Timeout != manifest.Duration(2*time.Second) || len(check.HTTP.ExpectStatus) != 1 || check.HTTP.ExpectStatus[0] != 200 {
					t.Errorf("unresolved check = %#v", check)
				}
				if calls.Add(1) == 1 {
					<-ctx.Done()
					return nil
				}
				return nil
			}, func(state probe.Readiness) { updates <- state })
		}()
		started := time.Now()
		<-updates
		select {
		case state := <-updates:
			if !state.Ready || time.Since(started) != 7*time.Second || calls.Load() != 2 {
				t.Fatalf("attempt timeout counted as success: state=%#v elapsed=%v", state, time.Since(started))
			}
		case err := <-done:
			t.Fatalf("monitor stopped: %v", err)
		}
		if config.Check.Timeout != 0 || config.Check.Interval != 0 || config.Check.HTTP.ExpectStatus != nil {
			t.Fatal("monitor modified the declared health configuration")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("stop error = %v", err)
		}
	})
}

func TestReadinessStartupDeadlineBoundary(t *testing.T) {
	for _, duration := range []time.Duration{49 * time.Millisecond, 50 * time.Millisecond} {
		t.Run(duration.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				config := readinessConfig()
				config.StartupTimeout = manifest.Duration(50 * time.Millisecond)
				config.Check.Timeout = manifest.Duration(time.Second)
				updates, done := make(chan probe.Readiness, 8), make(chan error, 1)
				go func() {
					done <- probe.MonitorReadiness(ctx, time.Now(), config, func(context.Context, manifest.Probe) error { time.Sleep(duration); return nil }, func(state probe.Readiness) { updates <- state })
				}()
				<-updates
				state := <-updates
				if duration < 50*time.Millisecond {
					if !state.Ready || !state.EverReady || state.StartupFailed {
						t.Fatalf("timely success state = %#v", state)
					}
					cancel()
					if err := <-done; !errors.Is(err, context.Canceled) {
						t.Fatalf("stop error = %v", err)
					}
				} else {
					if state.Ready || state.EverReady || !state.StartupFailed {
						t.Fatalf("deadline success was accepted: %#v", state)
					}
					if err := <-done; !errors.Is(err, probe.ErrStartupTimeout) {
						t.Fatalf("startup error = %v", err)
					}
				}
			})
		})
	}
}

func TestReadinessRejectsInvalidConfiguration(t *testing.T) {
	for _, name := range []string{"no health", "no check", "no process start", "no attempt", "no observer", "negative startup", "negative interval", "negative timeout", "negative successes", "negative failures"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				config := readinessConfig()
				started := time.Now()
				attempt := func(context.Context, manifest.Probe) error { t.Error("invalid monitor ran"); return nil }
				observer := func(probe.Readiness) { t.Error("invalid monitor published state") }
				switch name {
				case "no health":
					config = nil
				case "no check":
					config.Check = nil
				case "no process start":
					started = time.Time{}
				case "no attempt":
					attempt = nil
				case "no observer":
					observer = nil
				case "negative startup":
					config.StartupTimeout = -1
				case "negative interval":
					config.Check.Interval = -1
				case "negative timeout":
					config.Check.Timeout = -1
				case "negative successes":
					config.Ready.Successes = -1
				case "negative failures":
					config.Ready.UnreadyFailures = -1
				}
				if err := probe.MonitorReadiness(ctx, started, config, attempt, observer); err == nil || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("invalid configuration was not rejected immediately: %v", err)
				}
			})
		})
	}
}
