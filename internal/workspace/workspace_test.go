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
