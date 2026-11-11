package state

// Generation retains immutable desired intent for one stable application slot.
// Version orders admissions in that slot; this is not observed runtime identity,
// readiness, a worker lease or proof that an older process stopped.
type Generation struct {
	Scope       string
	SlotID      string
	ID          string
	OperationID string
	Version     int64
	Intent      []byte
}

// GenerationRequest explicitly admits a desired generation. ExpectedVersion 0
// means create the slot's first generation; later admissions name the head's
// version. The owner authorizes the scope and validates the operation's meaning
// and canonical intent. Payloads contain secret references, never secret values.
type GenerationRequest struct {
	Scope           string
	SlotID          string
	ID              string
	OperationID     string
	IdempotencyKey  string
	ExpectedVersion int64
	Intent          []byte
}
