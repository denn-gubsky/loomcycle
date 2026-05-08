// Package auth holds shared authentication helpers used by both
// the HTTP and gRPC wire surfaces.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
)

// CompareBearer reports whether got equals want, in time independent
// of either argument's length and content.
//
// Why hash before compare: subtle.ConstantTimeCompare alone is NOT
// length-independent. The Go stdlib documents that
// ConstantTimeCompare "returns 0 immediately" when the two slices
// have different lengths, which leaks the expected token's length
// to a network-adjacent attacker probing with varied-length inputs.
// Hashing both arguments to a fixed-length sha256 digest before the
// constant-time compare collapses that side channel: the compare
// always operates on 32-byte slices regardless of input size.
//
// Used by the HTTP authMiddleware and the gRPC checkBearer
// interceptor to validate `Authorization: Bearer <token>`.
func CompareBearer(got, want string) bool {
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}
