package workspace

import (
	"strings"
	"testing"
)

// A workspace id is a directory name under the worker's root and nothing
// else: no separator, no dot, nothing a path or a shell reads specially.
func TestValidID_AdmitsOnlyAPlainName(t *testing.T) {
	for _, id := range []string{"ws1", "a", "A-b_c", "0123456789abcdef", strings.Repeat("x", MaxIDLen)} {
		if !ValidID(id) {
			t.Errorf("%q refused", id)
		}
	}
	for _, id := range []string{"", ".", "..", "../x", "a/b", `a\b`, "-rf", "a.b", "a b", "a\x00b", "é", strings.Repeat("x", MaxIDLen+1), "/abs"} {
		if ValidID(id) {
			t.Errorf("%q admitted", id)
		}
	}
}

// An engine this build does not know satisfies no requirement; container is
// stronger than host.
func TestEngineRank_OrdersByIsolation(t *testing.T) {
	if !(EngineRank(EngineContainer) > EngineRank(EngineHost) && EngineRank(EngineHost) > EngineRank("") && EngineRank("vm") == 0) {
		t.Fatal("engine ranks out of order")
	}
}

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
