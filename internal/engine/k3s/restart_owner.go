package k3s

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/daropotter/chmura/internal/supervision"
)

// RestartOwnerStore provides the existing admission, execution, binding and
// supervision contracts. sqlite.Store implements it; snapshots are not leases.
type RestartOwnerStore interface {
	RestartAdmissionStore
	RestartJournal
	RestartSuccessorStore
	DurableSupervisionStore
}

// RestartOwner performs one explicitly authorized restart and successor
// supervision under a shared process-local slot runner. Configure Supervisor
// before use and share Slots with all related owner actions. Independent
// processes/direct adapter calls are not fenced. This is not a scheduler.
type RestartOwner struct {
	Slots      *supervision.SlotRunner
	Supervisor Supervisor
}

type RestartOwnerRequest struct {
	Admission   ProcessRestartAdmission
	HistoryKey  string
	TerminalKey string
}

// RestartOwnerOptions supplies the explicit delay between confirmed Retiring
// steps and the existing bounded supervision/reporting settings. Optional runtime
// publication requires a fresh session key per invocation, including recovery;
// admission/history/terminal request keys stay stable on exact retries.
type RestartOwnerOptions struct {
	RetireInterval time.Duration
	Supervision    DurableSupervisionOptions
}

// Run admits the explicit restart, waits for old absence, acknowledges creation,
// binds the exact successor and runs its durable supervision. The same slot is
// held across all phases, waits and joined reporter cleanup. Only Retiring is
// polled; API/storage errors and pending/unavailable discovery return to the
// caller for reconciliation and exact retry, never an implicit new generation.
// A newly supervised terminal outcome is exposed only after acknowledgement;
// an already closed binding needs explicit history reconciliation rather than
// replaying a fresh decision. Every method error exposes zero result, including
// cancellation.
// The caller authorizes scope/intent, drains any other reporters and chooses
// future placement/retry/failure policy. No later restart is admitted implicitly.
func (o RestartOwner) Run(ctx context.Context, history RestartOwnerStore, request RestartOwnerRequest, options RestartOwnerOptions) (DurableSupervisionResult, error) {
	if err := ctx.Err(); err != nil {
		return DurableSupervisionResult{}, err
	}
	settings := options.Supervision
	if o.Slots == nil || history == nil || o.Supervisor.Resolver.Client == nil || options.RetireInterval <= 0 ||
		strings.TrimSpace(request.HistoryKey) == "" || strings.TrimSpace(request.TerminalKey) == "" ||
		settings.Writes.AttemptTimeout <= 0 || settings.Writes.RetryInterval <= 0 || settings.FinalizeTimeout <= 0 {
		return DurableSupervisionResult{}, errors.New("restart owner requires shared slots, history, a runtime client, session/terminal keys and positive retirement/supervision bounds")
	}
	if publication := settings.Readiness; publication != nil && (strings.TrimSpace(publication.SessionKey) == "" || publication.Writes.AttemptTimeout <= 0 || publication.Writes.RetryInterval <= 0) {
		return DurableSupervisionResult{}, errors.New("restart owner readiness publication requires a fresh session key and positive write bounds")
	}
	if _, err := prepareSupervisionPlan(request.Admission.Next.Plan); err != nil {
		return DurableSupervisionResult{}, err
	}
	var result DurableSupervisionResult
	err := o.Slots.Run(ctx, request.Admission.Scope, request.Admission.SlotID, func(ctx context.Context) error {
		selection, err := AdmitProcessRestart(ctx, history, request.Admission)
		if err != nil {
			return err
		}
		for {
			progress, err := (Restarter{Client: o.Supervisor.Resolver.Client}).EnsureAdmitted(ctx, history, selection)
			if err != nil {
				return err
			}
			if !progress.Retiring {
				break
			}
			timer := time.NewTimer(options.RetireInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		resume, err := o.Supervisor.Resolver.BindRestartSuccessor(ctx, history, selection)
		if err != nil {
			return err
		}
		result, err = o.Supervisor.ResumeDurable(ctx, history, DurableSupervisionRequest{Resume: resume, HistoryKey: request.HistoryKey, TerminalKey: request.TerminalKey}, settings)
		return err
	})
	if err != nil {
		return DurableSupervisionResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return DurableSupervisionResult{}, err
	}
	return result, nil
}
