package k3s

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/supervision"
)

// DurableSupervisionStore retains the history of an already admitted/bound
// process. sqlite.Store implements it; records are not ownership or fencing.
type DurableSupervisionStore interface {
	SupervisionHistory
	supervision.ProcessRecorder
	RecordProcessTerminal(context.Context, state.ProcessTerminalObservation) (state.Process, bool, error)
}

type DurableSupervisionRequest struct {
	Resume      ResumeRequest
	HistoryKey  string
	TerminalKey string
}

// DurableSupervisionOptions bounds initial acknowledgement by Writes.AttemptTimeout.
// FinalizeTimeout is shared by the final flush/read/terminal-retry path.
// Store cancellation latency still applies; all started work is joined on return.
type DurableSupervisionOptions struct {
	Writes          supervision.ReporterOptions
	FinalizeTimeout time.Duration
}

// DurableSupervisionResult exposes the completed outcome only after terminal
// history was acknowledged. A nil method error is still not restart permission;
// the caller must authorize/serialize any later action and drain other reporters.
type DurableSupervisionResult struct {
	Process state.Process
	Outcome error
}

// ResumeDurable owns one monitor and its process-history worker. It requires an
// existing current binding and externally serialized ownership/authorized intent.
// It publishes no runtime readiness, invokes no external observer and performs no
// restart. Nonterminal stops retain first readiness but return no terminal result.
func (s Supervisor) ResumeDurable(ctx context.Context, history DurableSupervisionStore, request DurableSupervisionRequest, options DurableSupervisionOptions) (DurableSupervisionResult, error) {
	if err := ctx.Err(); err != nil {
		return DurableSupervisionResult{}, err
	}
	if history == nil || s.Resolver.Client == nil || strings.TrimSpace(request.HistoryKey) == "" || strings.TrimSpace(request.TerminalKey) == "" || options.FinalizeTimeout <= 0 || options.Writes.AttemptTimeout <= 0 || options.Writes.RetryInterval <= 0 {
		return DurableSupervisionResult{}, errors.New("durable supervision requires history, a runtime client, session/terminal keys and positive write/finalization bounds")
	}
	plan, err := prepareSupervisionPlan(request.Resume.Plan)
	if err != nil {
		return DurableSupervisionResult{}, err
	}
	request.Resume.Plan = plan
	process, err := retainedSupervisionProcess(ctx, history, request.Resume)
	if err != nil {
		return DurableSupervisionResult{}, err
	}
	// Keep the initial receipt replayable across recovery with the same session.
	// False cannot erase retained true; Resume reloads that committed history.
	reporter, err := supervision.NewProcessReporter(history, state.ProcessObservation{Scope: process.Scope, SlotID: process.SlotID, GenerationID: process.GenerationID,
		IdempotencyKey: request.HistoryKey, Identity: process.Identity, StartedAt: process.StartedAt}, options.Writes)
	if err != nil {
		return DurableSupervisionResult{}, err
	}
	// Persistence survives monitor/parent cancellation long enough to drain known
	// first readiness. It is explicitly stopped/joined on every path after startup.
	workerCtx, stopWorker := context.WithCancel(context.WithoutCancel(ctx))
	workerDone := make(chan error, 1)
	go func() { workerDone <- reporter.Run(workerCtx) }()
	initial, stopInitial := context.WithTimeout(ctx, options.Writes.AttemptTimeout)
	err = reporter.Flush(initial)
	stopInitial()
	if err != nil {
		stopWorker()
		<-workerDone
		return DurableSupervisionResult{}, fmt.Errorf("acknowledge initial process history: %w", err)
	}
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	monitorDone := make(chan error, 1)
	go func() { monitorDone <- s.Resume(monitorCtx, history, request.Resume, reporter.Observe) }()
	var outcome, workerErr error
	workerEnded := false
	select {
	case outcome = <-monitorDone:
	case workerErr = <-workerDone:
		workerEnded = true
		stopMonitor()
		outcome = <-monitorDone
	}
	observedAt := time.Now().UTC()
	stopMonitor()
	final, stopFinal := context.WithTimeout(context.WithoutCancel(ctx), options.FinalizeTimeout)
	defer stopFinal()
	flushErr := reporter.Flush(final)
	stopWorker()
	if !workerEnded {
		workerErr = <-workerDone
	}
	if flushErr != nil {
		return DurableSupervisionResult{}, errors.Join(ctx.Err(), fmt.Errorf("flush process history: %w", flushErr))
	}
	if workerEnded || !errors.Is(workerErr, context.Canceled) {
		if workerErr == nil {
			workerErr = errors.New("process history worker stopped unexpectedly")
		}
		return DurableSupervisionResult{}, errors.Join(ctx.Err(), workerErr)
	}
	if err := ctx.Err(); err != nil {
		return DurableSupervisionResult{}, err
	}
	process, err = history.GetProcess(final, process.Scope, process.SlotID, process.GenerationID)
	if err != nil {
		return DurableSupervisionResult{}, fmt.Errorf("read drained process history: %w", err)
	}
	terminal, err := PrepareProcessTerminal(ProcessTerminalRequest{Process: process, Instance: request.Resume.Instance, Plan: plan,
		Cause: outcome, IdempotencyKey: request.TerminalKey, ObservedAt: observedAt})
	if errors.Is(err, ErrNoTerminalEvidence) {
		if outcome == nil {
			outcome = errors.New("supervision stopped without a terminal outcome")
		}
		return DurableSupervisionResult{}, outcome
	}
	if err != nil {
		return DurableSupervisionResult{}, err
	}
	process, err = recordSupervisionTerminal(ctx, final, history, terminal, options.Writes)
	if err != nil {
		return DurableSupervisionResult{}, err
	}
	return DurableSupervisionResult{Process: process, Outcome: outcome}, nil
}

func recordSupervisionTerminal(ctx, final context.Context, history DurableSupervisionStore, request state.ProcessTerminalObservation, options supervision.ReporterOptions) (state.Process, error) {
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return state.Process{}, err
		}
		if err := final.Err(); err != nil {
			return state.Process{}, errors.Join(err, last)
		}
		attempt, cancel := context.WithTimeout(final, options.AttemptTimeout)
		stopParent := context.AfterFunc(ctx, cancel)
		process, _, err := history.RecordProcessTerminal(attempt, request)
		stopParent()
		cancel()
		if cause := ctx.Err(); cause != nil {
			return state.Process{}, cause
		}
		if err == nil {
			if process.Terminal == nil {
				return state.Process{}, fmt.Errorf("%w: terminal storage returned no acknowledgement", state.ErrConflict)
			}
			return process, nil
		}
		if errors.Is(err, state.ErrConflict) || errors.Is(err, state.ErrNotFound) {
			return state.Process{}, err
		}
		last = err
		timer := time.NewTimer(options.RetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-final.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
