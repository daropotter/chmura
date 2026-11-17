package k3s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/daropotter/chmura/internal/deployment"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
)

// ErrNoTerminalEvidence means the outcome cannot establish a terminal failure.
// The owner retains the original cause for cancellation or reconciliation.
var ErrNoTerminalEvidence = errors.New("supervision outcome has no terminal failure evidence")

// ProcessTerminalRequest joins a completed supervisor outcome to an acknowledged
// exact process binding. The owner authorizes scope/plan, joins monitors and
// reporters, and flushes first readiness before preparing terminal history.
// Capture ObservedAt once on receiving the outcome; retain the prepared request
// for retries. Preparation is not execution ownership or restart permission.
type ProcessTerminalRequest struct {
	Process        state.Process
	Instance       Instance
	Plan           deployment.InstancePlan
	Cause          error
	IdempotencyKey string
	ObservedAt     time.Time
}

type processTerminalEvidence struct {
	Version   int              `json:"version"`
	Engine    string           `json:"engine"`
	Exit      *terminalExit    `json:"runtime-exit,omitempty"`
	Startup   *terminalStartup `json:"startup-timeout,omitempty"`
	Unhealthy *terminalHealthy `json:"unhealthy,omitempty"`
}

type terminalExit struct {
	ResourceVersion string    `json:"resource-version"`
	ExitCode        int32     `json:"exit-code"`
	Reason          string    `json:"reason"`
	StartedAt       time.Time `json:"started-at"`
	FinishedAt      time.Time `json:"finished-at"`
}

type terminalStartup struct {
	TimeoutNS      int64              `json:"timeout-ns"`
	ReadySuccesses manifest.Threshold `json:"ready-successes"`
	CheckSHA256    string             `json:"check-sha256"`
}

type terminalHealthy struct {
	Failures    manifest.Threshold `json:"unhealthy-failures"`
	CheckSHA256 string             `json:"check-sha256"`
}

// PrepareProcessTerminal converts verified exit/startup/healthy outcomes into a
// canonical private v1 receipt payload without I/O. Record the returned value
// through state storage outside callbacks. Storage still checks current-head
// admission, immutable terminal facts and exact historical retries; a candidate
// prepared from an old snapshot grants no permission to resume or restart it.
func PrepareProcessTerminal(request ProcessTerminalRequest) (state.ProcessTerminalObservation, error) {
	p := request.Process
	if strings.TrimSpace(p.Scope) == "" || strings.TrimSpace(p.SlotID) == "" || strings.TrimSpace(p.GenerationID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" || p.StartedAt.IsZero() {
		return state.ProcessTerminalObservation{}, errors.New("terminal preparation requires scope, slot, generation, key and a bound process start")
	}
	if _, err := p.StartedAt.UTC().MarshalText(); err != nil {
		return state.ProcessTerminalObservation{}, fmt.Errorf("encode terminal process start: %w", err)
	}
	identity, err := EncodeProcessIdentity(request.Instance)
	if err != nil {
		return state.ProcessTerminalObservation{}, err
	}
	if !bytes.Equal(identity, p.Identity) {
		return state.ProcessTerminalObservation{}, state.ErrConflict
	}
	var decision *RestartNeededError
	if errors.As(request.Cause, &decision) && (decision == nil || decision.cause == nil) {
		return state.ProcessTerminalObservation{}, ErrNoTerminalEvidence
	}
	var exit *InstanceExitError
	hasExit := errors.As(request.Cause, &exit)
	startup, unhealthy := errors.Is(request.Cause, probe.ErrStartupTimeout), errors.Is(request.Cause, probe.ErrUnhealthy)
	if request.Cause == nil || (hasExit && exit == nil) || errors.Is(request.Cause, context.Canceled) || errors.Is(request.Cause, context.DeadlineExceeded) ||
		errors.Is(request.Cause, ErrObservationLost) || errors.Is(request.Cause, ErrStaleInstance) || (startup && unhealthy) || (hasExit && (startup || unhealthy)) {
		return state.ProcessTerminalObservation{}, ErrNoTerminalEvidence
	}
	evidence := processTerminalEvidence{Version: 1, Engine: "k3s"}
	var cause state.ProcessTerminalCause
	var occurred time.Time
	switch {
	case hasExit:
		if exit.Instance != request.Instance || !exit.StartedAt.Equal(p.StartedAt) {
			return state.ProcessTerminalObservation{}, state.ErrConflict
		}
		if strings.TrimSpace(exit.ResourceVersion) == "" || !utf8.ValidString(exit.ResourceVersion) || !utf8.ValidString(exit.Reason) || exit.Reason == "ContainerStatusUnknown" || exit.FinishedAt.IsZero() || exit.FinishedAt.Before(exit.StartedAt) {
			return state.ProcessTerminalObservation{}, ErrNoTerminalEvidence
		}
		cause, occurred = state.ProcessExited, exit.FinishedAt.UTC()
		evidence.Exit = &terminalExit{exit.ResourceVersion, exit.ExitCode, exit.Reason, exit.StartedAt.UTC(), occurred}
	case errors.Is(request.Cause, ErrInstanceUnavailable):
		return state.ProcessTerminalObservation{}, ErrNoTerminalEvidence
	case startup:
		if p.EverReady {
			return state.ProcessTerminalObservation{}, state.ErrConflict
		}
		config := request.Plan.Health.Effective()
		if request.Plan.Readiness != deployment.ReadinessChecked || config.StartupTimeout <= 0 || config.Ready.Successes <= 0 || config.Ready.UnreadyFailures <= 0 {
			return state.ProcessTerminalObservation{}, ErrUnsupportedPlan
		}
		observed, err := terminalObservedAt(request)
		if err != nil {
			return state.ProcessTerminalObservation{}, err
		}
		occurred = p.StartedAt.UTC().Add(time.Duration(config.StartupTimeout))
		if observed.Before(occurred) {
			return state.ProcessTerminalObservation{}, ErrNoTerminalEvidence
		}
		digest, err := terminalCheckDigest(config.Check, request.Plan)
		if err != nil {
			return state.ProcessTerminalObservation{}, err
		}
		cause = state.ProcessStartupTimeout
		evidence.Startup = &terminalStartup{int64(config.StartupTimeout), config.Ready.Successes, digest}
	case unhealthy:
		if decision == nil {
			return state.ProcessTerminalObservation{}, ErrNoTerminalEvidence
		}
		original, supplied := decision.plan, request.Plan
		original.Health, supplied.Health = *original.Health.Effective(), *supplied.Health.Effective()
		if !p.EverReady || decision.instance != request.Instance || !decision.startedAt.Equal(p.StartedAt) || !reflect.DeepEqual(original, supplied) {
			return state.ProcessTerminalObservation{}, state.ErrConflict
		}
		if original.Health.Healthy == nil || original.Health.Healthy.UnhealthyFailures <= 0 || !errors.Is(decision.cause, probe.ErrUnhealthy) {
			return state.ProcessTerminalObservation{}, ErrNoTerminalEvidence
		}
		occurred, err = terminalObservedAt(request)
		if err != nil {
			return state.ProcessTerminalObservation{}, err
		}
		digest, err := terminalCheckDigest(original.Health.Healthy.Check, original)
		if err != nil {
			return state.ProcessTerminalObservation{}, err
		}
		cause = state.ProcessUnhealthy
		evidence.Unhealthy = &terminalHealthy{original.Health.Healthy.UnhealthyFailures, digest}
	default:
		return state.ProcessTerminalObservation{}, ErrNoTerminalEvidence
	}
	if _, err := occurred.MarshalText(); err != nil {
		return state.ProcessTerminalObservation{}, fmt.Errorf("encode terminal occurrence: %w", err)
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return state.ProcessTerminalObservation{}, fmt.Errorf("encode terminal evidence: %w", err)
	}
	return state.ProcessTerminalObservation{Scope: p.Scope, SlotID: p.SlotID, GenerationID: p.GenerationID, IdempotencyKey: request.IdempotencyKey,
		Identity: identity, StartedAt: p.StartedAt.UTC(), Terminal: state.ProcessTerminal{Cause: cause, OccurredAt: occurred, Evidence: encoded}}, nil
}

func terminalObservedAt(request ProcessTerminalRequest) (time.Time, error) {
	at := request.ObservedAt.UTC()
	if at.IsZero() || at.Before(request.Process.StartedAt) {
		return time.Time{}, errors.New("terminal outcome requires its original observation time at/after process start")
	}
	if _, err := at.MarshalText(); err != nil {
		return time.Time{}, fmt.Errorf("encode terminal observation time: %w", err)
	}
	return at, nil
}

// Fingerprint only the effective check/endpoint used for the decision. Raw paths,
// commands and probe error strings are not retained in terminal evidence.
func terminalCheckDigest(check *manifest.Probe, plan deployment.InstancePlan) (string, error) {
	if !networkCheck(check, plan.Ports) || check.Interval <= 0 || check.Timeout <= 0 {
		return "", ErrUnsupportedPlan
	}
	fields := map[string]any{"interval-ns": int64(check.Interval), "timeout-ns": int64(check.Timeout)}
	var port string
	if check.HTTP != nil {
		port = check.HTTP.Port
		if !utf8.ValidString(check.HTTP.Path) {
			return "", ErrUnsupportedPlan
		}
		fields["kind"], fields["path"], fields["expect-status"] = "http", check.HTTP.Path, check.HTTP.ExpectStatus
	} else {
		port, fields["kind"] = check.TCP.Port, "tcp"
	}
	endpoint := plan.Ports[port]
	if endpoint.Number < 1 || endpoint.Number > 65535 || !utf8.ValidString(port) || !utf8.ValidString(endpoint.Protocol) {
		return "", ErrUnsupportedPlan
	}
	fields["port-name"], fields["port-number"], fields["protocol"] = port, endpoint.Number, endpoint.Protocol
	// All values have fixed JSON representations; map keys are sorted by encoding/json.
	encoded, _ := json.Marshal(fields)
	return fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}
