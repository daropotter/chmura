package probe

import (
	"context"
	"errors"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
)

// ErrStartupTimeout means the instance never acquired readiness before its startup deadline.
var ErrStartupTimeout = errors.New("application did not become ready before startup-timeout")

// Readiness describes a checked instance. EverReady remains true after readiness
// is lost; StartupFailed is terminal for this process generation.
type Readiness struct {
	Ready         bool
	EverReady     bool
	StartupFailed bool
}

// MonitorReadiness applies readiness rules to a serial schedule of an explicit
// validated probe. processStartedAt is a runtime fact, not the monitor's start time.
// attempt must honor cancellation and treat its resolved probe as immutable.
// onChange runs serially for the initial state and changes; it must return
// promptly and update local state rather than perform blocking outbound I/O.
// Cancellation or startup failure waits for the active attempt to clean up.
// Restart rules and process-only readiness are separate concerns.
func MonitorReadiness(ctx context.Context, processStartedAt time.Time, health *manifest.Health,
	attempt func(context.Context, manifest.Probe) error, onChange func(Readiness)) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if processStartedAt.IsZero() || health == nil || health.Check == nil || attempt == nil || onChange == nil {
		return errors.New("readiness monitoring requires a process start time, explicit probe, attempt and observer")
	}
	config := health.Effective()
	if config.StartupTimeout <= 0 || config.Check.Interval <= 0 || config.Check.Timeout <= 0 || config.Ready.Successes <= 0 || config.Ready.UnreadyFailures <= 0 {
		return errors.New("readiness timings and thresholds must be positive")
	}
	deadline := processStartedAt.Add(time.Duration(config.StartupTimeout))
	state := Readiness{}
	failStartup := func() error {
		if err := contextError(ctx); err != nil {
			return err
		}
		state.StartupFailed = true
		onChange(state)
		return ErrStartupTimeout
	}
	if !time.Now().Before(deadline) {
		return failStartup()
	}
	onChange(state)
	workerCtx, cancel := context.WithCancel(ctx)
	results, done := make(chan Result), make(chan error, 1)
	go func() {
		done <- Schedule(workerCtx, time.Duration(config.Check.Interval), func(ctx context.Context) error {
			ctx, stop := context.WithTimeout(ctx, time.Duration(config.Check.Timeout))
			defer stop()
			err := attempt(ctx, *config.Check)
			if cause := contextError(ctx); cause != nil {
				return cause
			}
			return err
		}, results)
	}()
	defer func() { cancel(); <-done }()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	startup := timer.C
	var successes, failures manifest.Threshold
	apply := func(result Result) error {
		if !state.EverReady && !result.FinishedAt.Before(deadline) {
			return failStartup()
		}
		previous := state
		if result.Err == nil {
			failures = 0
			if successes < config.Ready.Successes {
				successes++
			}
			if successes == config.Ready.Successes {
				state.Ready, state.EverReady = true, true
				timer.Stop()
				startup = nil
			}
		} else {
			successes = 0
			if failures < config.Ready.UnreadyFailures {
				failures++
			}
			if failures == config.Ready.UnreadyFailures {
				state.Ready = false
			}
		}
		if state != previous {
			onChange(state)
		}
		return nil
	}
	for {
		if err := contextError(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-results:
			if err := apply(result); err != nil {
				return err
			}
		case <-startup:
			// Prefer an already completed, timely result when both channels are
			// ready; select ordering must not decide whether startup succeeded.
			select {
			case result := <-results:
				if err := apply(result); err != nil {
					return err
				}
			default:
			}
			if !state.EverReady {
				return failStartup()
			}
		}
	}
}
