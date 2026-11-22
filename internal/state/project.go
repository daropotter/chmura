package state

// Project is the registered identity of one system in a space, separately from
// its manifest/revisions. The opaque server-assigned ID is stable; the name is
// unique within that space.
type Project struct {
	ID      string
	Name    string
	SpaceID string
}

// CreateProjectRequest explicitly registers a project in an existing space.
// The caller authorizes creation in the space and validates the domain name.
// Registration creates no revision or runtime resources.
type CreateProjectRequest struct {
	Name           string
	SpaceID        string
	IdempotencyKey string
}
