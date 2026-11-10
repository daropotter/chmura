package k3s

import (
	"context"
	"errors"
	"maps"
	"sync"

	"github.com/daropotter/chmura/internal/deployment"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
)

// SupervisionRequest selects one generation and its validated portable plan.
// EverReady is the owner's retained Chmura fact for this exact generation.
// A generation already declared failed must not be resumed.
type SupervisionRequest struct {
	Instance  Instance
	Plan      deployment.InstancePlan
	EverReady bool
}

// Supervisor joins lifecycle observation with independent readiness/healthy
// checks. Resolver and Network must remain immutable during monitoring.
type Supervisor struct {
	Resolver Resolver
	Network  probe.Network
}

// Monitor supervises one explicitly selected, already running generation.
// It starts checks only after snapshot/watch establishment, cancels all work on
// the first terminal cause, and waits for cleanup before returning that cause.
// onChange runs serially, must return promptly, and must perform no outbound I/O.
// On stop, any current readiness is revoked locally while EverReady is retained.
// Errors distinguish health/startup failures, runtime loss/replacement and gaps
// in observation. This method never reconnects, adopts, restarts or writes status;
// the owner reconciles those actions and persists generation history separately.
func (s Supervisor) Monitor(ctx context.Context, request SupervisionRequest, onChange func(probe.Readiness)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onChange == nil {
		return errors.New("instance supervision requires a readiness observer")
	}
	config, ports := request.Plan.Health.Effective(), maps.Clone(request.Plan.Ports)
	if request.Plan.Readiness != deployment.ReadinessChecked || !networkCheck(config.Check, ports) {
		return ErrUnsupportedPlan
	}
	if config.StartupTimeout <= 0 || config.Ready.Successes <= 0 || config.Ready.UnreadyFailures <= 0 {
		return errors.New("instance health timings and thresholds must be positive")
	}
	if healthy := config.Healthy; healthy != nil {
		if healthy.UnhealthyFailures <= 0 {
			return errors.New("instance healthy threshold must be positive")
		}
		if healthy.Check != nil && !networkCheck(healthy.Check, ports) {
			return ErrUnsupportedPlan
		}
	}
	for _, port := range ports {
		if port.Number < 1 || port.Number > 65535 {
			return errors.New("instance port number is invalid")
		}
	}

	session, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	var workers sync.WaitGroup
	targets := make(chan Target, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		err := s.Resolver.Observe(session, request.Instance, ports, func(target Target) {
			select {
			case targets <- target:
			case <-session.Done():
			}
		})
		stop(err)
	}()
	var last probe.Readiness // Written by the readiness worker; read only after Wait.
	select {
	case <-session.Done():
	case target := <-targets:
		if session.Err() == nil {
			firstReady := make(chan struct{})
			attempt := func(ctx context.Context, check manifest.Probe) error {
				return s.Network.Run(ctx, target.Network, check)
			}
			workers.Add(2)
			go func() {
				defer workers.Done()
				signaled := false
				err := probe.MonitorReadinessWithHistory(session, target.ProcessStartedAt, config, request.EverReady, attempt, func(state probe.Readiness) {
					if session.Err() != nil {
						return
					}
					last = state
					onChange(state)
					if state.EverReady && !signaled {
						close(firstReady)
						signaled = true
					}
				})
				if err != nil {
					stop(err)
				}
			}()
			go func() {
				defer workers.Done()
				if err := probe.MonitorHealthy(session, firstReady, config, attempt); err != nil {
					stop(err)
				}
			}()
		}
	}
	<-session.Done()
	workers.Wait()
	if last.Ready {
		last.Ready = false
		onChange(last)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.Cause(session)
}
