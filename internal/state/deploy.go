package state

// DeployRequest submits canonical manifest bytes for an existing project. The
// caller authorizes the project in SpaceID and validates the manifest, its
// project identity, and any secret references. The base pair is (0, "") for a
// project with no selection, or the version and ID read by the command.
type DeployRequest struct {
	SpaceID            string
	ProjectID          string
	IdempotencyKey     string
	ExpectedVersion    int64
	ExpectedRevisionID string
	Manifest           []byte
}

// DeployAcceptance pairs an accepted operation with its original desired
// revision selection. On retry, the operation reflects its current progress
// and the selection still names the revision admitted for that operation.
type DeployAcceptance struct {
	Operation Operation
	Selection RevisionSelection
}
