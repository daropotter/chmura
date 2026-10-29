package probe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
)

// Runtime executes process and exec probes through callbacks bound to one
// runtime instance. Configure the callbacks before use and keep them immutable.
// ProcessRunning checks for the named process or executable in that instance.
// Exec passes the executable and literal arguments directly, without adding a
// shell. A nonzero exit status is an outcome, while an error means execution
// failed. Adapters must honor cancellation, cancel the entire operation and
// finish cleanup before returning. There is no default host implementation.
type Runtime struct {
	ProcessRunning func(context.Context, string) (bool, error)
	Exec           func(context.Context, []string) (int, error)
}

// ErrProcessNotRunning means the adapter found no matching running process.
var ErrProcessNotRunning = errors.New("named process is not running")

// ExitError reports a completed exec probe with a nonzero exit status.
type ExitError struct {
	ExitCode int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exec probe exited with status %d", e.ExitCode)
}

// Run executes one validated process or exec check with its effective timeout.
// Only the selected callback is required. Scheduling, readiness and restart
// decisions belong to the caller. The declared check is never modified.
func (r Runtime) Run(ctx context.Context, check manifest.Probe) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if check.HTTP != nil || check.TCP != nil || (check.Process == nil) == (check.Exec == nil) {
		return errors.New("runtime probe must select exactly one of process or exec")
	}
	effective := (&manifest.Health{Check: &check}).Effective().Check
	if effective.Timeout <= 0 {
		return errors.New("probe timeout must be positive")
	}
	if effective.Process != nil {
		if strings.TrimSpace(effective.Process.Name) == "" || r.ProcessRunning == nil {
			return errors.New("process probe requires a name and an instance process adapter")
		}
	} else if len(effective.Exec.Command) == 0 || strings.TrimSpace(effective.Exec.Command[0]) == "" || r.Exec == nil {
		return errors.New("exec probe requires a command and an instance exec adapter")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(effective.Timeout))
	defer cancel()
	if effective.Process != nil {
		running, err := r.ProcessRunning(ctx, effective.Process.Name)
		if err != nil {
			return attemptError(ctx, "process probe", err)
		}
		if err := contextError(ctx); err != nil {
			return err
		}
		if !running {
			return fmt.Errorf("%w: %s", ErrProcessNotRunning, effective.Process.Name)
		}
		return nil
	}
	code, err := r.Exec(ctx, effective.Exec.Command)
	if err != nil {
		return attemptError(ctx, "exec probe", err)
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{ExitCode: code}
	}
	return nil
}
