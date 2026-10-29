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
	return schedule(ctx, interval, attempt, func(err error) (time.Time, error) {
		result := Result{FinishedAt: time.Now(), Err: err}
		if err := contextError(ctx); err != nil {
			return result.FinishedAt, err
		}
		select {
		case results <- result:
			return result.FinishedAt, nil
		case <-ctx.Done():
			return result.FinishedAt, ctx.Err()
		}
	})
}

// complete records completion and waits for consumption. Its timestamp anchors
// the interval even if the consumer applies backpressure for longer than that.
func schedule(ctx context.Context, interval time.Duration, attempt func(context.Context) error,
	complete func(error) (time.Time, error)) error {
	for {
		if err := contextError(ctx); err != nil {
			return err
		}
		err := attempt(ctx)
		if err := contextError(ctx); err != nil {
			return err
		}
		finishedAt, err := complete(err)
		if err != nil {
			return err
		}
		timer := time.NewTimer(time.Until(finishedAt.Add(interval)))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}
