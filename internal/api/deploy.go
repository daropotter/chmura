package api

// DeployRequest submits YAML with an explicitly supplied base version. Version
// zero and an empty base revision ID specify an existing project with no head.
type DeployRequest struct {
	BaseVersion    *int64 `json:"base-version"`
	BaseRevisionID string `json:"base-revision-id,omitempty"`
	Manifest       string `json:"manifest"`
}

// DeployAccepted is the durable operation receipt returned on acceptance or retry.
type DeployAccepted struct {
	OperationID string `json:"operation-id"`
	Status      string `json:"status"`
}
