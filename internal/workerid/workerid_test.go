package workerid

import "testing"

// A personal worker's grant key is what both the dispatcher (the id a worker
// attaches under) and the snapshot restore (whose rows may name it) build:
// the tenant and the first 16 hex of SHA-256 of the user, never the user.
func TestPersonalGrantKey_IsTheTenantAndTheUsersHash(t *testing.T) {
	if got, want := PersonalGrantKey("acme", "alice"), "user:acme/2bd806c97f0e00af"; got != want {
		t.Errorf("PersonalGrantKey = %q, want %q", got, want)
	}
	if PersonalGrantKey("acme", "alice") == PersonalGrantKey("acme", "bob") {
		t.Error("two users share a grant key")
	}
}
