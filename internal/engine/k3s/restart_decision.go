package k3s

import (
	"errors"
	"fmt"

	"github.com/daropotter/chmura/internal/deployment"
	"github.com/daropotter/chmura/internal/probe"
)

// RestartNeededError is a supervisor decision for one generation and plan,
// returned only after its monitors have stopped. Unwrap retains the runtime
// exit facts or unhealthy cause. It does not authorize or execute runtime I/O.
type RestartNeededError struct {
	instance Instance
	plan     deployment.InstancePlan
	cause    error
}

func (e *RestartNeededError) Error() string {
	return fmt.Sprintf("instance needs restart: %v", e.cause)
}

func (e *RestartNeededError) Unwrap() error { return e.cause }

// Request binds the decision to the original creation request and an explicit
// successor key/allocation, validating both without I/O. The owner must drain
// and stop reporting, authorize/serialize the operation and retain it durably
// before passing this request to Restarter.Ensure. The decision is not a lease
// or a permanent operation receipt and must not revive an obsolete operation.
func (e *RestartNeededError) Request(previous, next InstanceRequest) (RestartRequest, error) {
	if e == nil || e.cause == nil {
		return RestartRequest{}, errors.New("restart request requires a supervisor decision")
	}
	original, err := retirementPayload(previous, e.instance)
	if err != nil {
		return RestartRequest{}, err
	}
	snapshot := previous
	snapshot.Plan = e.plan
	observed, err := PrepareInstance(snapshot)
	if err != nil {
		return RestartRequest{}, err
	}
	if original.Annotations[intentAnnotation] != observed.Annotations[intentAnnotation] {
		return RestartRequest{}, ErrInstanceConflict
	}
	request := RestartRequest{Previous: previous, Instance: e.instance, Next: next}
	if _, err := restartPayload(request); err != nil {
		return RestartRequest{}, err
	}
	return request, nil
}

func restartDecision(request SupervisionRequest, last probe.Readiness, cause error) error {
	var exit *InstanceExitError
	exited := errors.As(cause, &exit) && exit.Instance == request.Instance
	healthFailed := errors.Is(cause, probe.ErrUnhealthy) && last.EverReady && request.Plan.Health.Healthy != nil && request.Plan.Health.Healthy.Check != nil
	if !exited && !healthFailed {
		return cause
	}
	return &RestartNeededError{instance: request.Instance, plan: request.Plan, cause: cause}
}
