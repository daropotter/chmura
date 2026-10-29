package probe_test

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
)

func TestScheduleRunsImmediatelyThenWaitsFromCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := time.Now()
		starts := make(chan time.Time, 8)
		results := make(chan probe.Result, 1)
		done := make(chan error, 1)
		go func() {
			done <- probe.Schedule(ctx, 5*time.Second, func(context.Context) error {
				starts <- time.Now()
				time.Sleep(2 * time.Second)
				return nil
			}, results)
		}()
		synctest.Wait()
		select {
		case got := <-starts:
			if !got.Equal(started) {
				t.Fatalf("first start = %v, want %v", got, started)
			}
		default:
			t.Fatal("first attempt did not start immediately")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		result := <-results
		if result.Err != nil || !result.FinishedAt.Equal(started.Add(2*time.Second)) {
			t.Fatalf("first result = %#v", result)
		}
		time.Sleep(4 * time.Second)
		synctest.Wait()
		select {
		case got := <-starts:
			t.Fatalf("attempt started before the completion interval: %v", got)
		default:
		}
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case got := <-starts:
			if !got.Equal(started.Add(7 * time.Second)) {
				t.Fatalf("next attempt start = %v", got)
			}
		default:
			t.Fatal("next attempt did not start after the completion interval")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("stop error = %v", err)
		}
	})
}

func TestScheduleDoesNotOverlapSlowAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		defer close(release)
		var calls atomic.Int32
		results := make(chan probe.Result, 1)
		done := make(chan error, 1)
		go func() {
			done <- probe.Schedule(ctx, time.Second, func(ctx context.Context) error {
				calls.Add(1)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}, results)
		}()
		synctest.Wait()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("slow attempt overlapped with %d calls", calls.Load())
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("stop error = %v", err)
		}
		select {
		case result := <-results:
			t.Errorf("canceled attempt was reported as an outcome: %#v", result)
		default:
		}
	})
}

func TestScheduleReportsFailureAndContinues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		failure := errors.New("unready")
		var calls atomic.Int32
		results := make(chan probe.Result)
		done := make(chan error, 1)
		go func() {
			done <- probe.Schedule(ctx, time.Second, func(context.Context) error {
				if calls.Add(1) == 1 {
					return failure
				}
				return nil
			}, results)
		}()
		if result := <-results; !errors.Is(result.Err, failure) {
			t.Fatalf("failure result = %#v", result)
		}
		if result := <-results; result.Err != nil {
			t.Fatalf("recovery result = %#v", result)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("stop error = %v", err)
		}
	})
}

func TestScheduleBackpressureDoesNotQueueCatchUpAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := time.Now()
		var calls atomic.Int32
		results := make(chan probe.Result)
		done := make(chan error, 1)
		go func() {
			done <- probe.Schedule(ctx, 5*time.Second, func(context.Context) error { calls.Add(1); return nil }, results)
		}()
		synctest.Wait()
		time.Sleep(20 * time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("slow consumer caused %d attempts", calls.Load())
		}
		first := <-results
		if !first.FinishedAt.Equal(started) {
			t.Fatalf("completion time changed while publishing: %v", first.FinishedAt)
		}
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("next attempt count = %d, want 2", calls.Load())
		}
		second := <-results
		if !second.FinishedAt.Equal(started.Add(20 * time.Second)) {
			t.Fatalf("next completion = %v", second.FinishedAt)
		}
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("scheduler attempted to catch up missed intervals")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("stop error = %v", err)
		}
	})
}

func TestScheduleCancellationWhileWaitingOrPublishing(t *testing.T) {
	for _, buffer := range []int{0, 1} {
		t.Run(map[int]string{0: "publishing", 1: "interval"}[buffer], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var calls atomic.Int32
				results := make(chan probe.Result, buffer)
				done := make(chan error, 1)
				go func() {
					done <- probe.Schedule(ctx, time.Second, func(context.Context) error { calls.Add(1); return nil }, results)
				}()
				synctest.Wait()
				if calls.Load() != 1 {
					t.Fatalf("attempt count = %d", calls.Load())
				}
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("stop error = %v", err)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if calls.Load() != 1 {
					t.Fatal("scheduler ran after cancellation")
				}
				select {
				case _, open := <-results:
					if !open {
						t.Fatal("scheduler closed the caller's result channel")
					}
				default:
				}
			})
		})
	}
}

func TestScheduleWaitsForCanceledAttemptCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release, canceled := make(chan struct{}), make(chan struct{})
		results := make(chan probe.Result, 1)
		done := make(chan error, 1)
		go func() {
			done <- probe.Schedule(ctx, time.Second, func(ctx context.Context) error {
				<-ctx.Done()
				close(canceled)
				<-release
				return ctx.Err()
			}, results)
		}()
		synctest.Wait()
		cancel()
		<-canceled
		synctest.Wait()
		returnedEarly := false
		select {
		case err := <-done:
			t.Errorf("scheduler abandoned active attempt cleanup: %v", err)
			returnedEarly = true
		default:
		}
		close(release)
		if !returnedEarly {
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("stop error = %v", err)
			}
		}
		select {
		case result := <-results:
			t.Errorf("canceled attempt was reported: %#v", result)
		default:
		}
	})
}

func TestScheduleContinuesAfterNetworkAttemptTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		check := (&manifest.Health{Check: &manifest.Probe{
			TCP:      &manifest.TCPCheck{Port: "service"},
			Interval: manifest.Duration(100 * time.Millisecond),
			Timeout:  manifest.Duration(50 * time.Millisecond),
		}}).Effective().Check
		runner := probe.Network{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		target := probe.Target{Host: "app.test", Ports: map[string]manifest.Port{"service": {Number: 8080, Protocol: "tcp"}}}
		results := make(chan probe.Result)
		done := make(chan error, 1)
		started := time.Now()
		go func() {
			done <- probe.Schedule(ctx, time.Duration(check.Interval), func(ctx context.Context) error {
				return runner.Run(ctx, target, *check)
			}, results)
		}()
		for _, elapsed := range []time.Duration{50 * time.Millisecond, 200 * time.Millisecond} {
			result := <-results
			if !errors.Is(result.Err, context.DeadlineExceeded) || !result.FinishedAt.Equal(started.Add(elapsed)) {
				t.Fatalf("network timeout result = %#v, want completion after %v", result, elapsed)
			}
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("stop error = %v", err)
		}
	})
}

func TestScheduleAlreadyCanceledDoesNotRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := probe.Schedule(ctx, time.Second, func(context.Context) error { calls++; return nil }, make(chan probe.Result))
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("canceled schedule: calls=%d error=%v", calls, err)
	}
}

func TestScheduleRejectsInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		attempt  func(context.Context) error
		results  chan probe.Result
	}{
		{"zero interval", 0, func(context.Context) error { t.Error("invalid scheduler ran"); return nil }, make(chan probe.Result)},
		{"negative interval", -time.Second, func(context.Context) error { t.Error("invalid scheduler ran"); return nil }, make(chan probe.Result)},
		{"nil attempt", time.Second, nil, make(chan probe.Result)},
		{"nil output", time.Second, func(context.Context) error { t.Error("invalid scheduler ran"); return nil }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := probe.Schedule(context.Background(), tc.interval, tc.attempt, tc.results); err == nil {
				t.Fatal("invalid scheduler accepted")
			}
		})
	}
}
