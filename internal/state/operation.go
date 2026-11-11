// Package state defines portable control-plane persistence records.
package state

import "errors"

var (
	ErrConflict = errors.New("operation key is already bound to another intent")
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

// Operation is an immutable acceptance record. Acceptance does not authorize
// execution or report completion; domain state and lifecycle follow separately.
type Operation struct {
	ID     string
	Scope  string
	Kind   string
	Intent []byte
}
