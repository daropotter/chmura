package probe

import (
	"context"
	"time"
)

// A deadline can pass before its cancellation callback runs. Classify completion
// by the actual deadline as well as Err, independently of timer/goroutine order.
func contextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
