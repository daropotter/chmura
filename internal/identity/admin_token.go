// Package identity defines credentials shared by bootstrap and API authentication.
package identity

import (
	"crypto/sha256"
	"errors"
)

// AdminTokenLength is the encoded length of a built-in administrator token.
const AdminTokenLength = 64

// AdminTokenDigest validates the token text and computes the stored verifier.
// Bootstrap removes the file's line ending before calling this function.
func AdminTokenDigest(token []byte) ([32]byte, error) {
	invalid := errors.New("administrator token must contain exactly 64 lowercase hexadecimal characters")
	if len(token) != AdminTokenLength {
		return [32]byte{}, invalid
	}
	for _, ch := range token {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return [32]byte{}, invalid
		}
	}
	return sha256.Sum256(token), nil
}
