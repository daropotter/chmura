package k3s

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/daropotter/chmura/internal/probe"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

// ReporterOptions bounds each API attempt and the pause between failed attempts.
type ReporterOptions struct {
	AttemptTimeout time.Duration
	RetryInterval  time.Duration
}

// ReadinessReporter publishes the latest local readiness for one generation.
// Construct it with NewReadinessReporter and run it once per owner session.
// Observe is suitable for Supervisor.Monitor's prompt, I/O-free callback.
type ReadinessReporter struct {
	client  coreclient.CoreV1Interface
	ref     Instance
	session string
	options ReporterOptions

	mu       sync.Mutex
	ready    bool
	sequence uint64
	accepted uint64
	active   context.CancelFunc
	started  bool
	ended    bool
	runErr   error
	wake     chan struct{}
	progress chan struct{}
}

// NewReadinessReporter requires a complete generation, a unique owner-session
// key and explicit positive I/O/retry bounds. It performs no cluster I/O. The
// owner must never reuse the session key for a newly constructed reporter.
func NewReadinessReporter(client coreclient.CoreV1Interface, ref Instance, sessionKey string, options ReporterOptions) (*ReadinessReporter, error) {
	if client == nil || ref.Namespace == "" || ref.PodName == "" || ref.PodUID == "" || ref.ContainerName == "" || ref.ContainerID == "" || strings.TrimSpace(sessionKey) == "" || options.AttemptTimeout <= 0 || options.RetryInterval <= 0 {
		return nil, errors.New("readiness reporting requires a client, generation, session key and positive attempt/retry bounds")
	}
	return &ReadinessReporter{client: client, ref: ref, session: fmt.Sprintf("%x", sha256.Sum256([]byte(sessionKey))), options: options, wake: make(chan struct{}, 1), progress: make(chan struct{})}, nil
}

// Observe coalesces readiness values into one slot and cancels a superseded
// attempt. It does not publish or retain EverReady/StartupFailed history: the
// supervisor owner must preserve those facts separately for this generation.
func (r *ReadinessReporter) Observe(state probe.Readiness) {
	r.mu.Lock()
	if r.sequence != 0 && r.ready == state.Ready {
		r.mu.Unlock()
		return
	}
	r.ready = state.Ready
	r.sequence++
	cancel := r.active
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Flush waits for acceptance of the state observed at entry, or a coalesced
// newer state. Use a bounded context and keep Run alive while flushing the final
// unready decision. Worker cancellation/failure unblocks all pending flushes.
func (r *ReadinessReporter) Flush(ctx context.Context) error {
	r.mu.Lock()
	target := r.sequence
	r.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
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
			return ctx.Err()
		case <-progress:
		}
	}
}

// Run serializes publication until cancellation or a permanent error. A failed
// ambiguous write retains its exact key/base for replay. A version conflict
// forces a fresh snapshot/key for the current local decision; it never adopts
// a replacement generation. The reporter must have a single owner, not a lease.
func (r *ReadinessReporter) Run(ctx context.Context) (err error) {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return errors.New("readiness reporter session has already run")
	}
	r.started = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.ended, r.runErr = true, err
		close(r.progress)
		r.mu.Unlock()
	}()
	var pending *ReadinessUpdate
	var pendingSequence, nonce uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		sequence, ready, accepted := r.sequence, r.ready, r.accepted
		r.mu.Unlock()
		if sequence == accepted {
			if err := r.wait(ctx, sequence, 0); err != nil {
				return err
			}
			continue
		}
		if pendingSequence != sequence {
			pending = nil
		}
		attempt, cancel := context.WithTimeout(ctx, r.options.AttemptTimeout)
		r.mu.Lock()
		current := r.sequence == sequence
		if current {
			r.active = cancel
		}
		r.mu.Unlock()
		if !current {
			cancel()
			continue
		}
		var outcome error
		if pending == nil {
			var target Target
			target, outcome = (Resolver{Client: r.client}).Resolve(attempt, r.ref, nil)
			if outcome == nil && target.ResourceVersion == "" {
				outcome = fmt.Errorf("%w: runtime snapshot has no resource version", ErrObservationLost)
			}
			if outcome == nil {
				nonce++
				pending = &ReadinessUpdate{Instance: r.ref, ResourceVersion: target.ResourceVersion, IdempotencyKey: fmt.Sprintf("readiness/%s/%d/%d", r.session, sequence, nonce), Ready: ready}
				pendingSequence = sequence
			}
		}
		if outcome == nil && r.current(sequence) {
			_, outcome = (ReadinessWriter{Client: r.client}).Apply(attempt, *pending)
		}
		r.mu.Lock()
		r.active = nil
		r.mu.Unlock()
		cancel()
		if err := ctx.Err(); err != nil {
			return err
		}
		if !r.current(sequence) {
			continue
		}
		if outcome == nil {
			r.mu.Lock()
			r.accepted = sequence
			close(r.progress)
			r.progress = make(chan struct{})
			r.mu.Unlock()
			pending = nil
			continue
		}
		if permanentReportingError(outcome) {
			return outcome
		}
		if errors.Is(outcome, ErrReadinessConflict) {
			pending = nil
		}
		if err := r.wait(ctx, sequence, r.options.RetryInterval); err != nil {
			return err
		}
	}
}

func (r *ReadinessReporter) current(sequence uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sequence == sequence
}

func (r *ReadinessReporter) wait(ctx context.Context, sequence uint64, retry time.Duration) error {
	var elapsed <-chan time.Time
	if retry > 0 {
		timer := time.NewTimer(retry)
		defer timer.Stop()
		elapsed = timer.C
	}
	for r.current(sequence) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.wake:
		case <-elapsed:
			return nil
		}
	}
	return nil
}

func permanentReportingError(err error) bool {
	return errors.Is(err, ErrStaleInstance) || errors.Is(err, ErrInstanceUnavailable) || errors.Is(err, ErrInstanceConflict) || errors.Is(err, ErrObservationLost) ||
		apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsNotFound(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err)
}
