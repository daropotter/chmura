package k3s

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
)

// ErrNoRestartEvidence means retained history supplies no supported automatic
// restart decision. Startup timeout remains a failure, not an implicit retry.
var ErrNoRestartEvidence = errors.New("process history has no supported automatic restart evidence")

// ProcessRestartRequest joins an owner-verified retained process snapshot and
// exact instance to the original creation intent and an explicit successor.
// The owner authorizes scope/intent, checks current desired state, serializes
// execution and drains reporters. History alone grants no execution permission.
type ProcessRestartRequest struct {
	Process  state.Process
	Instance Instance
	Previous InstanceRequest
	Next     InstanceRequest
}

// PrepareProcessRestart recovers an executor request from canonical private v1
// exit/healthy evidence without a live supervisor error or I/O. The returned
// requests are independent snapshots. Preparation neither admits a successor
// nor authorizes Restarter.Ensure: retain/authorize the operation separately and
// recheck its currency before every execution. Historical reads are not leases.
func PrepareProcessRestart(request ProcessRestartRequest) (RestartRequest, error) {
	p := request.Process
	if p.Terminal == nil || (p.Terminal.Cause != state.ProcessExited && p.Terminal.Cause != state.ProcessUnhealthy) {
		return RestartRequest{}, ErrNoRestartEvidence
	}
	var evidence processTerminalEvidence
	if err := json.Unmarshal(p.Terminal.Evidence, &evidence); err != nil {
		return RestartRequest{}, fmt.Errorf("%w: decode terminal evidence: %w", ErrNoRestartEvidence, err)
	}
	if evidence.Version != 1 || evidence.Engine != "k3s" {
		return RestartRequest{}, ErrNoRestartEvidence
	}
	for _, current := range []*InstanceRequest{&request.Previous, &request.Next} {
		current.Plan.Ports = maps.Clone(current.Plan.Ports)
		current.Plan.Health = *current.Plan.Health.Effective()
	}
	decision := &RestartNeededError{instance: request.Instance, plan: request.Previous.Plan, startedAt: p.StartedAt}
	switch p.Terminal.Cause {
	case state.ProcessExited:
		if evidence.Exit == nil {
			return RestartRequest{}, ErrNoRestartEvidence
		}
		exit := evidence.Exit
		decision.cause = &InstanceExitError{Instance: request.Instance, ResourceVersion: exit.ResourceVersion,
			ExitCode: exit.ExitCode, Reason: exit.Reason, StartedAt: exit.StartedAt, FinishedAt: exit.FinishedAt}
	case state.ProcessUnhealthy:
		if evidence.Unhealthy == nil || request.Previous.Plan.Health.Healthy == nil || request.Previous.Plan.Health.Healthy.Check == nil {
			return RestartRequest{}, ErrNoRestartEvidence
		}
		decision.cause = probe.ErrUnhealthy
	}
	// Reuse the terminal encoder's identity/start/readiness/health checks. This
	// validation key is never stored or used as an operation/terminal receipt.
	prepared, err := PrepareProcessTerminal(ProcessTerminalRequest{Process: p, Instance: request.Instance, Plan: request.Previous.Plan,
		Cause: decision, IdempotencyKey: "validate-retained-restart", ObservedAt: p.Terminal.OccurredAt})
	if err != nil {
		return RestartRequest{}, err
	}
	if prepared.Terminal.Cause != p.Terminal.Cause || !prepared.Terminal.OccurredAt.Equal(p.Terminal.OccurredAt) || !bytes.Equal(prepared.Terminal.Evidence, p.Terminal.Evidence) {
		// Exact canonical bytes reject extra/duplicate fields, mixed causes and
		// changed check fingerprints; unknown encodings require reconciliation.
		return RestartRequest{}, fmt.Errorf("%w: terminal evidence does not match retained facts and requested health rule", state.ErrConflict)
	}
	return decision.Request(request.Previous, request.Next)
}
