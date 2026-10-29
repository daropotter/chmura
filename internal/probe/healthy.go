package probe

import (
	"context"
	"errors"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
)

// ErrUnhealthy means the explicit healthy probe reached its consecutive-failure
// threshold. It is a restart decision, not evidence that a restart took place.
var ErrUnhealthy = errors.New("application reached unhealthy-failures threshold")

// MonitorHealthy evaluates the opt-in healthy rule for one process generation.
// The owner closes firstReady after the full readiness threshold is acquired;
// later readiness loss does not pause or reset this independent check.
// Only an explicit healthy.check schedules attempts. Absent rules and process-only
// rules return nil; runtime exit supervision remains the owner's responsibility.
// health must be validated. attempt must honor cancellation and treat the
// resolved probe as immutable. Each attempt has its own timeout; parent stop
// waits for cleanup and does not count as a failed check. ErrUnhealthy terminates
// this monitor; the owner implements restart actions and a new generation.
func MonitorHealthy(ctx context.Context, firstReady <-chan struct{}, health *manifest.Health,
	attempt func(context.Context, manifest.Probe) error) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if health == nil || health.Healthy == nil {
		return nil
	}
	config := health.Effective().Healthy
	if config.UnhealthyFailures <= 0 {
		return errors.New("healthy failure threshold must be positive")
	}
	if config.Check == nil {
		return nil
	}
	if firstReady == nil || attempt == nil || config.Check.Interval <= 0 || config.Check.Timeout <= 0 {
		return errors.New("healthy monitoring requires first readiness, an attempt and positive probe timings")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-firstReady:
	}
	var failures manifest.Threshold
	return schedule(ctx, time.Duration(config.Check.Interval), func(ctx context.Context) error {
		ctx, stop := context.WithTimeout(ctx, time.Duration(config.Check.Timeout))
		defer stop()
		err := attempt(ctx, *config.Check)
		if cause := contextError(ctx); cause != nil {
			return cause
		}
		return err
	}, func(outcome error) (time.Time, error) {
		finishedAt := time.Now()
		if err := contextError(ctx); err != nil {
			return finishedAt, err
		}
		if outcome == nil {
			failures = 0
		} else {
			failures++
			if failures == config.UnhealthyFailures {
				return finishedAt, ErrUnhealthy
			}
		}
		return finishedAt, nil
	})
}
