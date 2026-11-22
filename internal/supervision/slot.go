package supervision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/daropotter/chmura/internal/state"
)

// SlotRunner serializes cooperative work per exact scope/slot inside one process.
// Its zero value is ready to use. Share one runner across all owner actions; do
// not copy it after first use. Independent runners/processes do not coordinate.
// This is not durable ownership, authorization, a distributed lease or fencing.
type SlotRunner struct {
	mu     sync.Mutex
	active map[slotKey]struct{}
}

type slotKey struct {
	scope string
	slot  string
}

// Run calls work synchronously with the supplied context. An occupied slot
// returns state.ErrConflict without calling work or queueing a later action.
// Other scopes/slots can progress independently. Cancellation does not release
// the slot: work must stop/join its monitors and drain reporters before returning.
// Run holds the slot through that return (including callback defer unwinding),
// preserves work's error unchanged and releases on return or panic unwinding.
// Releasing the slot acknowledges neither durable history nor execution safety;
// the owner must inspect the outcome and recheck authorized current state.
func (r *SlotRunner) Run(ctx context.Context, scope, slot string, work func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil || strings.TrimSpace(scope) == "" || strings.TrimSpace(slot) == "" || !utf8.ValidString(scope) || !utf8.ValidString(slot) || work == nil {
		return errors.New("slot work requires a runner, UTF-8 scope/slot and a callback")
	}
	key := slotKey{scope, slot}
	r.mu.Lock()
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return err
	}
	if _, occupied := r.active[key]; occupied {
		r.mu.Unlock()
		return fmt.Errorf("%w: slot work is already running", state.ErrConflict)
	}
	if r.active == nil {
		r.active = make(map[slotKey]struct{})
	}
	r.active[key] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.active, key)
		r.mu.Unlock()
	}()
	return work(ctx)
}
