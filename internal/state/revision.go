package state

// Revision is an immutable manifest snapshot for an existing project. Its
// opaque server-assigned ID does not imply activation, ordering or deployment.
type Revision struct {
	ID        string
	ProjectID string
	Manifest  []byte
}

// CreateRevisionRequest stores caller-validated canonical manifest bytes. The
// caller authorizes the project within SpaceID, checks the manifest's project
// identity and validates secret references rather than secret values. Storage
// does not select an active revision or admit a deploy against a base revision.
type CreateRevisionRequest struct {
	SpaceID        string
	ProjectID      string
	IdempotencyKey string
	Manifest       []byte
}
