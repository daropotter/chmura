package supervision_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/supervision"
)

func TestSlotRunnerKeepsSlotUntilCanceledWorkReturns(t *testing.T) {
	var runner supervision.SlotRunner
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started, draining, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	failure := errors.New("history flush failed")
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx, "project-a", "api/0", func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(draining)
			<-release
			return errors.Join(ctx.Err(), failure)
		})
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("work did not start")
	}
	var competingCalls atomic.Int32
	compete := func() {
		t.Helper()
		err := runner.Run(context.Background(), "project-a", "api/0", func(context.Context) error { competingCalls.Add(1); return nil })
		if !errors.Is(err, state.ErrConflict) || competingCalls.Load() != 0 {
			t.Fatalf("overlapping work executed: %v calls=%d", err, competingCalls.Load())
		}
	}
	compete()
	cancel()
	select {
	case <-draining:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not reach work")
	}
	compete() // Cancellation is not completion of draining.
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, failure) {
			t.Fatalf("work result lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("completed work did not release its caller")
	}
	if err := runner.Run(context.Background(), "project-a", "api/0", func(context.Context) error { competingCalls.Add(1); return nil }); err != nil || competingCalls.Load() != 1 {
		t.Fatalf("completed slot could not run again: %v", err)
	}
}

func TestSlotRunnerSeparatesScopedSlotsAndPreservesWorkResult(t *testing.T) {
	var runner supervision.SlotRunner
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "owner-context")
	failure := errors.New("owner operation failed")
	calls := 0
	err := runner.Run(ctx, "project-a", "api/0", func(work context.Context) error {
		if work.Value(contextKey{}) != "owner-context" {
			t.Error("work lost its parent context")
		}
		for _, key := range [][2]string{{"project-b", "api/0"}, {"project-a", "api/1"}} {
			if err := runner.Run(work, key[0], key[1], func(context.Context) error { calls++; return nil }); err != nil {
				t.Errorf("independent slot blocked: %v", err)
			}
		}
		return failure
	})
	if err != failure || calls != 2 {
		t.Fatalf("slot isolation or error identity lost: %v calls=%d", err, calls)
	}
	if err := runner.Run(ctx, "project-a", "api/0", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("failed work retained the slot: %v", err)
	}
}

func TestSlotRunnerConcurrentContendersExecuteOneWorkItem(t *testing.T) {
	var runner supervision.SlotRunner
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const contenders = 16
	start, acquired, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var acquiredOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	results := make(chan error, contenders)
	for range contenders {
		go func() {
			<-start
			results <- runner.Run(ctx, "project-a", "api/0", func(context.Context) error {
				calls.Add(1)
				acquiredOnce.Do(func() { close(acquired) })
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
	}
	close(start)
	select {
	case <-acquired:
	case <-ctx.Done():
		t.Fatal("no contender acquired the slot")
	}
	for range contenders - 1 {
		select {
		case err := <-results:
			if !errors.Is(err, state.ErrConflict) {
				t.Fatalf("contender did not fail fast with conflict: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("contenders waited instead of reporting conflict")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("overlapping callbacks ran: %d", calls.Load())
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-results:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("winning work did not finish")
	}
}

func TestSlotRunnerRejectsInvalidAndPreCanceledWork(t *testing.T) {
	for _, mode := range []string{"nil runner", "scope", "slot", "invalid UTF-8", "nil work", "pre-canceled"} {
		t.Run(mode, func(t *testing.T) {
			var owner supervision.SlotRunner
			runner, scope, slot := &owner, "project-a", "api/0"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			work := func(context.Context) error { calls++; return nil }
			switch mode {
			case "nil runner":
				runner = nil
			case "scope":
				scope = " "
			case "slot":
				slot = ""
			case "invalid UTF-8":
				scope = string([]byte{0xff})
			case "nil work":
				work = nil
			case "pre-canceled":
				cancel()
			}
			err := runner.Run(ctx, scope, slot, work)
			if err == nil || calls != 0 {
				t.Fatalf("invalid work executed: %v calls=%d", err, calls)
			}
			if mode == "pre-canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if err := owner.Run(context.Background(), "project-a", "api/0", func(context.Context) error { return nil }); err != nil {
				t.Fatalf("rejected work reserved the slot: %v", err)
			}
		})
	}
}

func TestSlotRunnerReleasesAfterCallbackPanicUnwinds(t *testing.T) {
	var runner supervision.SlotRunner
	cleaned := false
	func() {
		defer func() {
			if got := recover(); got != "callback panic" {
				t.Errorf("panic was changed: %v", got)
			}
		}()
		_ = runner.Run(context.Background(), "project-a", "api/0", func(context.Context) error { defer func() { cleaned = true }(); panic("callback panic") })
	}()
	if !cleaned {
		t.Fatal("callback cleanup did not unwind")
	}
	if err := runner.Run(context.Background(), "project-a", "api/0", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("panic poisoned the slot: %v", err)
	}
}
