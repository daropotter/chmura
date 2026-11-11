// Package state defines portable control-plane persistence records.
package state

import "errors"

var (
	ErrConflict = errors.New("operation state conflict")
	ErrNotFound = errors.New("operation not found")
)

// Submission binds an opaque key to a kind and canonical intent within an
// explicitly authorized scope. Intent must contain secret references, not values.
// The caller validates the domain request and produces its canonical encoding.
type Submission struct {
	Scope          string
	IdempotencyKey string
	Kind           string
	Intent         []byte
}

type OperationStatus string

const (
	OperationPending   OperationStatus = "pending"
	OperationRunning   OperationStatus = "running"
	OperationSucceeded OperationStatus = "succeeded"
	OperationFailed    OperationStatus = "failed"
	OperationCanceled  OperationStatus = "canceled"
)

// Operation snapshots progress while identity and accepted intent stay immutable.
// Status is a recorded fact, not an execution lease or authorization.
type Operation struct {
	ID      string
	Scope   string
	Kind    string
	Intent  []byte
	Status  OperationStatus
	Version int64
	Result  []byte
}

// OperationUpdate records one owner-authorized transition. Its opaque key is
// scoped to the operation. Result is canonical and nonempty for terminal states,
// absent for running; payloads contain secret references rather than values.
type OperationUpdate struct {
	Scope           string
	OperationID     string
	IdempotencyKey  string
	ExpectedVersion int64
	Status          OperationStatus
	Result          []byte
}
