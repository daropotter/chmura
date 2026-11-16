package state

import "time"

// Process retains one exact runtime identity and first-readiness history for a
// desired generation. Identity is a canonical private engine reference, never
// manifest data or credentials. EverReady is not current traffic readiness.
type Process struct {
	Scope        string
	SlotID       string
	GenerationID string
	Identity     []byte
	StartedAt    time.Time
	EverReady    bool
	Terminal     *ProcessTerminal
}

// ProcessTerminalCause distinguishes a confirmed runtime exit from Chmura's
// startup and independent healthy failures. Observation loss, replacement,
// unavailability and owner cancellation are not terminal failure evidence.
type ProcessTerminalCause string

const (
	ProcessExited         ProcessTerminalCause = "runtime-exit"
	ProcessStartupTimeout ProcessTerminalCause = "startup-timeout"
	ProcessUnhealthy      ProcessTerminalCause = "unhealthy"
)

// ProcessTerminal permanently closes supervision for one exact process. It is
// not proof of physical termination, a restart authorization or current status.
// Evidence is owner-verified canonical private data, with no credentials. The
// owner retains runtime exit facts (including exit code zero) or the relevant
// health decision here; the store does not interpret engine-specific evidence.
type ProcessTerminal struct {
	Cause      ProcessTerminalCause
	OccurredAt time.Time
	Evidence   []byte
}

// ProcessTerminalObservation supplies the first terminal decision for an already
// bound current generation. The owner must stop/drain its reporters and retain
// first readiness before recording this decision, outside probe callbacks.
type ProcessTerminalObservation struct {
	Scope          string
	SlotID         string
	GenerationID   string
	IdempotencyKey string
	Identity       []byte
	StartedAt      time.Time
	Terminal       ProcessTerminal
}

// ProcessObservation records owner-verified facts for the current generation.
// EverReady may only come from Chmura's full readiness threshold for this exact
// process. The owner authorizes the scope and validates the engine reference.
type ProcessObservation struct {
	Scope          string
	SlotID         string
	GenerationID   string
	IdempotencyKey string
	Identity       []byte
	StartedAt      time.Time
	EverReady      bool
}
