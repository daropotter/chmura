package state

// RevisionSelection is the current admitted desired revision of a project.
// Version orders selections, including a return to a retained revision. This
// records intent, not rollout success, runtime currency or execution ownership.
type RevisionSelection struct {
	ProjectID      string
	RevisionID     string
	OperationID    string
	Version        int64
	BaseRevisionID string
}

// RevisionAdmission selects an existing snapshot for an existing project under
// a running operation whose scope is ProjectID. The caller authorizes access and
// validates the operation's kind/intent and target manifest. The first selection
// requires ExpectedVersion 0 and an empty ExpectedRevisionID; later selections
// require both values read from CurrentRevision in the same command.
type RevisionAdmission struct {
	SpaceID            string
	ProjectID          string
	RevisionID         string
	OperationID        string
	IdempotencyKey     string
	ExpectedVersion    int64
	ExpectedRevisionID string
}
