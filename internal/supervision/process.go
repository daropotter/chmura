// Package supervision coordinates portable process-history reporting for owners.
package supervision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/daropotter/chmura/internal/probe"
	"github.com/daropotter/chmura/internal/state"
)

// ProcessRecorder accepts owner-verified facts and returns their durable snapshot.
// A nil error acknowledges a committed write or exact receipt replay, including
// when the boolean reports no new receipt. sqlite.Store implements this contract.
type ProcessRecorder interface {
	RecordProcess(context.Context, state.ProcessObservation) (state.Process, bool, error)
}

// ReporterOptions sets write-attempt deadlines and pauses between failed attempts.
// The backing store may impose additional cancellation latency (SQLite busy wait).
type ReporterOptions struct {
	AttemptTimeout time.Duration
	RetryInterval  time.Duration
}

// ProcessReporter binds one exact process and retains first readiness outside
// probe callbacks. Construct with NewProcessReporter and Run once. Its bounded
// pending state contains only the initial fact and a possible false→true update.
// This is not current traffic readiness, a worker lease or restart authorization.
type ProcessReporter struct {
	writer  ProcessRecorder
	initial state.ProcessObservation
	options ReporterOptions

	mu        sync.Mutex
	everReady bool
	sequence  uint64
	accepted  uint64
	started   bool
	ended     bool
	runErr    error
	lastErr   error
	wake      chan struct{}
	progress  chan struct{}
}

// NewProcessReporter freezes an authorized process observation with a session
// key and positive attempt/retry bounds. Initial EverReady, if true, must already
// be owner-verified for this process. Keys are derived from the session key;
// ambiguous recovery must repeat the same initial request/session. No I/O runs.
func NewProcessReporter(writer ProcessRecorder, request state.ProcessObservation, options ReporterOptions) (*ProcessReporter, error) {
	if writer == nil || strings.TrimSpace(request.Scope) == "" || strings.TrimSpace(request.SlotID) == "" || strings.TrimSpace(request.GenerationID) == "" ||
		strings.TrimSpace(request.IdempotencyKey) == "" || len(request.Identity) == 0 || request.StartedAt.IsZero() || options.AttemptTimeout <= 0 || options.RetryInterval <= 0 {
		return nil, errors.New("process reporter requires a writer, scope, slot, generation, session key, exact identity/start and positive bounds")
	}
	request.StartedAt = request.StartedAt.UTC()
	if _, err := request.StartedAt.MarshalText(); err != nil {
		return nil, fmt.Errorf("encode process reporter start: %w", err)
	}
	request.Identity = bytes.Clone(request.Identity)
	request.IdempotencyKey = fmt.Sprintf("process-history/v1/%x/initial", sha256.Sum256([]byte(request.IdempotencyKey)))
	return &ProcessReporter{writer: writer, initial: request, options: options, everReady: request.EverReady, sequence: 1,
		wake: make(chan struct{}, 1), progress: make(chan struct{})}, nil
}

// Observe retains only EverReady, monotonically. It performs no I/O and never
// waits for a write. Ready/StartupFailed do not invent historical facts or a
// terminal decision. Call only with Chmura-verified readiness for this process.
func (r *ProcessReporter) Observe(value probe.Readiness) {
	r.mu.Lock()
	if !value.EverReady || r.everReady {
		r.mu.Unlock()
		return
	}
	r.everReady = true
	r.sequence++
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Flush acknowledges the initial binding and facts observed at entry, not future
// observations. Keep Run alive and use a bounded flush context. Cancellation or
// permanent worker failure wakes unacknowledged flushes. A flush timeout retains
// the last storage failure, if any, alongside its context error. Acknowledgement
// proves persistence only, never current readiness, ownership or runtime permission.
func (r *ProcessReporter) Flush(ctx context.Context) error {
	r.mu.Lock()
	target := r.sequence
	r.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			r.mu.Lock()
			last := r.lastErr
			r.mu.Unlock()
			if last != nil {
				return errors.Join(err, last)
			}
			return err
		}
		r.mu.Lock()
		accepted, ended, cause, progress := r.accepted >= target, r.ended, r.runErr, r.progress
		r.mu.Unlock()
		if accepted {
			return nil
		}
		if ended {
			return cause
		}
		select {
		case <-ctx.Done():
		case <-progress:
		}
	}
}

// Run serializes initial binding before the optional first-ready receipt. Every
// retry preserves the exact request/key, even after an ambiguous commit. State
// conflict/not-found and terminal snapshots stop the worker; other errors retry
// within explicit bounds until cancellation. Stop/join observers, Flush while
// Run is alive, then cancel/join Run before recording a terminal cause elsewhere.
func (r *ProcessReporter) Run(ctx context.Context) (err error) {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return errors.New("process reporter session has already run")
	}
	r.started = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.ended, r.runErr = true, err
		close(r.progress)
		r.mu.Unlock()
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		accepted, sequence := r.accepted, r.sequence
		r.mu.Unlock()
		if accepted == sequence {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-r.wake:
			}
			continue
		}
		request, next := r.initial, uint64(1)
		if accepted > 0 {
			request.EverReady, next = true, 2
			request.IdempotencyKey += "/first-ready"
		}
		attempt, cancel := context.WithTimeout(ctx, r.options.AttemptTimeout)
		process, _, outcome := r.writer.RecordProcess(attempt, request)
		cancel()
		if err := ctx.Err(); err != nil {
			return err
		}
		if outcome == nil && process.Terminal != nil {
			return fmt.Errorf("%w: process reporting is terminal", state.ErrConflict)
		}
		if outcome == nil {
			r.mu.Lock()
			r.accepted = next
			r.lastErr = nil
			close(r.progress)
			r.progress = make(chan struct{})
			r.mu.Unlock()
			continue
		}
		r.mu.Lock()
		r.lastErr = outcome
		r.mu.Unlock()
		if errors.Is(outcome, state.ErrConflict) || errors.Is(outcome, state.ErrNotFound) {
			return outcome
		}
		timer := time.NewTimer(r.options.RetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
