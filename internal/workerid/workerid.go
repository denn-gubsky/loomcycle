// Package workerid builds the part of a workspace worker's id that more than
// the dispatcher reads (RFC GW P13). Stdlib only, at the bottom of the layer
// map, so the dispatcher (runtime) and the snapshot restore check (storage)
// build a personal worker's key from one function and cannot drift.
package workerid

import (
	"crypto/sha256"
	"encoding/hex"
)

// PersonalGrantKey is the grant part of a personal worker's id (RFC GW P13,
// D-P13-5): "user:<tenant>/" and the first 16 hex of SHA-256 of the user. A
// subject may hold bytes an id may not, and the key names the grant, never
// the token, so a rotated token's worker keeps its rows. The worker's id is
// the key, "/" and the id it declares.
func PersonalGrantKey(tenantID, userID string) string {
	h := sha256.Sum256([]byte(userID))
	return "user:" + tenantID + "/" + hex.EncodeToString(h[:])[:16]
}
