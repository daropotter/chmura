package probe

import (
	"context"
	"errors"
	"time"
)

// Result is the outcome of a completed attempt. FinishedAt records completion,
// independently of when a consumer receives it. A nil Err means success.
type Result struct {
	FinishedAt time.Time
	Err        error
}

// Schedule runs one check serially until ctx is canceled. The first attempt is
// immediate; later attempts wait at least interval after the previous completion.
// It applies backpressure rather than buffering results or catching up missed
// intervals. The caller owns results and must keep it open while Schedule runs.
// attempt must honor cancellation and enforce its own per-attempt timeout.
// Schedule waits for the active attempt to return and never closes results.
func Schedule(ctx context.Context, interval time.Duration, attempt func(context.Context) error, results chan<- Result) error {
	if interval <= 0 {
		return errors.New("probe interval must be positive")
	}
	if attempt == nil || results == nil {
		return errors.New("probe attempt and result channel are required")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := attempt(ctx)
		result := Result{FinishedAt: time.Now(), Err: err}
		if err := ctx.Err(); err != nil {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case results <- result:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}
