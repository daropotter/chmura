package probe

import (
	"context"
	"sync"
	"time"
)

// completion is a single-slot handoff. Recording the timestamp and making the
// outcome visible share a lock with take, so a deadline cannot miss a recorded
// completion even if its notification has not been delivered yet.
type completion struct {
	mu       sync.Mutex
	pending  *Result
	consumed chan struct{}
	notify   chan<- struct{}
	ready    <-chan struct{}
}

func (c *completion) publish(ctx context.Context, err error) (time.Time, error) {
	c.mu.Lock()
	result := Result{FinishedAt: time.Now(), Err: err}
	consumed := make(chan struct{})
	c.pending, c.consumed = &result, consumed
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	case <-ctx.Done():
		return result.FinishedAt, ctx.Err()
	}
	// Receiving the notification alone does not release backpressure: take must
	// consume the slot before the scheduler can record another completion.
	select {
	case <-consumed:
		return result.FinishedAt, nil
	case <-ctx.Done():
		return result.FinishedAt, ctx.Err()
	}
}

func (c *completion) take() (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		return Result{}, false
	}
	result := *c.pending
	c.pending = nil
	close(c.consumed)
	return result, true
}
