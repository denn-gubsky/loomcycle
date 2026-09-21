package providers

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ⚠️ DERIVED FROM BOTH SIDES, holding a copy of neither.
//
// The existing parity guards are per-event: someone adding an event is supposed
// to add a matching test. Sixteen wire values proved that does not happen — they
// were on the wire and absent from the TS union, some for a dozen releases, and
// v1.85.0's `context_exhausted` reached an operator's terminal as a type no
// typed client could name.
//
// So this one enumerates the runtime's OWN vocabulary out of provider.go and
// requires each value to appear in the adapter's union. A list in this file
// would be a third copy to forget; the constants are the list.
//
// It checks the union MEMBERSHIP only — "can a consumer name this value". The
// payload for a given event stays a per-event guard, because whether a frame
// carries a struct is a question about that frame.
func TestEventType_EveryWireValueIsNameableInTheTSAdapter(t *testing.T) {
	src, err := os.ReadFile("provider.go")
	if err != nil {
		t.Fatalf("read provider.go: %v", err)
	}
	// Event<Name> EventType = "<value>" — the declaration form every event uses.
	decl := regexp.MustCompile(`Event[A-Za-z0-9_]*\s+EventType\s*=\s*"([a-z0-9_]+)"`)
	var wire []string
	seen := map[string]bool{}
	for _, m := range decl.FindAllStringSubmatch(string(src), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			wire = append(wire, m[1])
		}
	}
	if len(wire) < 20 {
		t.Fatalf("only found %d event types in provider.go — the declaration form "+
			"changed and this guard is reading nothing: %v", len(wire), wire)
	}

	ts, err := os.ReadFile("../../adapters/ts/src/types.ts")
	if err != nil {
		t.Skipf("adapters/ts/src/types.ts not readable from here: %v", err)
	}
	// Only the union's own MEMBER LINES count. Matching anywhere in the file
	// would let a doc comment or a field name stand in for coverage — and the
	// union is full of prose that mentions the very values being checked, so a
	// substring search over the region reads as covered when it is not. (It also
	// cannot end at the first ";": the comments contain those too.)
	member := regexp.MustCompile(`^\s*\|\s*"([a-z0-9_]+)"`)
	terminator := regexp.MustCompile(`^\s*\|\s*"[a-z0-9_]+";\s*$`)
	lines := strings.Split(string(ts), "\n")
	i := 0
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "export type EventType =") {
			break
		}
	}
	if i == len(lines) {
		t.Fatal("adapters/ts/src/types.ts has no `export type EventType =` union")
	}
	named := map[string]bool{}
	closed := false
	for i++; i < len(lines); i++ {
		if m := member.FindStringSubmatch(lines[i]); m != nil {
			named[m[1]] = true
		}
		if terminator.MatchString(lines[i]) {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("the EventType union has no terminating member line — this guard " +
			"would read an unbounded region and stop meaning anything")
	}

	var missing []string
	for _, v := range wire {
		if !named[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d wire event value(s) a typed TS consumer cannot name — add them to "+
			"the EventType union in adapters/ts/src/types.ts: %s",
			len(missing), strings.Join(missing, ", "))
	}
}
