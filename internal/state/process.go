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
