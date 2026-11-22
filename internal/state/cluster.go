package state

// Cluster is an explicitly registered, initially empty execution-resource pool.
// Its opaque server-assigned ID is stable; names are installation-wide.
type Cluster struct {
	ID   string
	Name string
}

// CreateClusterRequest registers an empty cluster in this installation. The
// caller authorizes installation administration and validates the domain name.
// No engine configuration, nodes, spaces or resources are created implicitly.
type CreateClusterRequest struct {
	Name           string
	IdempotencyKey string
}
