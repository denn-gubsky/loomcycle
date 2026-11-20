package worker

import (
	"os"
	"testing"
)

// A worker's default id is the same at every start: the core keys its
// workspaces on it, and a new id would read every directory it holds as
// unknown and remove it.
func TestDefaultID_IsStableAcrossStarts(t *testing.T) {
	a, b := defaultID(), defaultID()
	if a != b || a == "" {
		t.Fatalf("default ids %q and %q", a, b)
	}
	if host, _ := os.Hostname(); host != "" && len(host) <= 128 && a != host && !validLooking(a) {
		t.Fatalf("default id %q is neither the host name nor a valid id", a)
	}
}

func validLooking(id string) bool {
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
