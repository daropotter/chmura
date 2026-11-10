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

func TestReadinessHistoryRetainsFirstReadinessButRechecksCurrentState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		config := readinessConfig()
		config.Ready.Successes = 2
		updates, done := make(chan probe.Readiness, 8), make(chan error, 1)
		var attempts atomic.Int32
		go func() {
			done <- probe.MonitorReadinessWithHistory(ctx, time.Now().Add(-3*time.Minute), config, true,
				func(context.Context, manifest.Probe) error { attempts.Add(1); return nil },
				func(state probe.Readiness) { updates <- state })
		}()
		if state := <-updates; state != (probe.Readiness{EverReady: true}) {
			t.Fatalf("history assumed current readiness or forgot first readiness: %#v", state)
		}
		synctest.Wait()
		if attempts.Load() != 1 {
			t.Fatalf("initial attempts = %d", attempts.Load())
		}
		select {
		case state := <-updates:
			t.Fatalf("partial success granted readiness: %#v", state)
		default:
		}
		time.Sleep(time.Second)
		if state := <-updates; state != (probe.Readiness{Ready: true, EverReady: true}) {
			t.Fatalf("readiness not reacquired with full threshold: %#v", state)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("history monitor stopped with %v", err)
		}
	})
}

func TestReadinessHistoryNeverRearmsStartupTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		var last probe.Readiness
		var attempts atomic.Int32
		go func() {
			done <- probe.MonitorReadinessWithHistory(ctx, time.Now().Add(-3*time.Minute), readinessConfig(), true,
				func(context.Context, manifest.Probe) error { attempts.Add(1); return errors.New("still unready") },
				func(state probe.Readiness) { last = state })
		}()
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("history rearmed startup timeout: %v", err)
		default:
		}
		if last != (probe.Readiness{EverReady: true}) || attempts.Load() < 2 {
			t.Fatalf("resumed monitor: state=%#v attempts=%d", last, attempts.Load())
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestReadinessWithoutHistoryDoesNotRenewExpiredStartupBudget(t *testing.T) {
	var last probe.Readiness
	attempts := 0
	err := probe.MonitorReadinessWithHistory(context.Background(), time.Now().Add(-3*time.Minute), readinessConfig(), false,
		func(context.Context, manifest.Probe) error { attempts++; return nil }, func(state probe.Readiness) { last = state })
	if !errors.Is(err, probe.ErrStartupTimeout) || attempts != 0 || last != (probe.Readiness{StartupFailed: true}) {
		t.Fatalf("expired generation: state=%#v attempts=%d error=%v", last, attempts, err)
	}
}

func TestReadinessWithHistoryHonorsPreCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := probe.MonitorReadinessWithHistory(ctx, time.Now(), readinessConfig(), true,
		func(context.Context, manifest.Probe) error { called = true; return nil }, func(probe.Readiness) { called = true })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled history monitor: called=%v error=%v", called, err)
	}
}
