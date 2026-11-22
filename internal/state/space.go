package state

// Space is a logical project/resource area bound to one installation cluster.
// Its opaque server-assigned ID is stable; names are installation-wide.
type Space struct {
	ID        string
	Name      string
	ClusterID string
}

// CreateSpaceRequest explicitly creates an empty space in an existing cluster.
// The caller authorizes space creation and access to the cluster, and validates
// the domain name. This creates no projects or runtime resources.
type CreateSpaceRequest struct {
	Name           string
	ClusterID      string
	IdempotencyKey string
}
