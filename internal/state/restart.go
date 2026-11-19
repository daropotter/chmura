package state

// RestartOperationKind identifies an owner-validated automatic restart intent.
// Its opaque canonical intent must also be the admitted successor's intent.
const RestartOperationKind = "restart"

// RestartAdmissionRequest appends a successor to an explicitly selected closed
// predecessor. ExpectedVersion must be positive and identify that predecessor.
// The owner verifies engine evidence, intent and scope before admission.
type RestartAdmissionRequest struct {
	GenerationRequest
	PreviousGenerationID string
}

// Restart is one consistent snapshot of an admitted, still unbound current
// successor, its running operation and the predecessor's terminal process. This
// is not execution ownership or a lease; the owner must serialize runtime actions and
// recheck currency before every step. Intent is owner-validated private data.
type Restart struct {
	Generation Generation
	Previous   Generation
	Process    Process
	Operation  Operation
}
