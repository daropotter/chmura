package state

// Admin identifies the installation's first administrator.
type Admin struct {
	ID   string
	Name string
}

// BootstrapAdminRequest binds the first administrator and its token digest
// to an installation-scoped idempotency key. Only local bootstrap should call it.
type BootstrapAdminRequest struct {
	Name           string
	IdempotencyKey string
	TokenDigest    [32]byte
}
