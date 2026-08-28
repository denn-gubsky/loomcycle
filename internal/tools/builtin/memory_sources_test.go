package builtin

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// TestParseSources_AcceptsEverySourceInItsOwnSchema.
//
// The bug this pins: `notes` was in this op's input-schema enum and absent from
// the parser's switch. Unknown values are DROPPED by design (documented on
// parseSources, and right — rejecting would break an older runtime against a
// newer value name), so an explicit `sources:["notes"]` became NO selector, and
// that failed in two opposite directions from one cause: `search` widened to
// everything and looked correct, while `recall` fell through to its facts-only
// default and returned nothing.
//
// So the invariant is: the parser must accept every value the schema advertises.
// Driven off the schema text rather than a hand-written list, because a
// hand-written list is the same kind of second copy that broke in the first place.
func TestParseSources_AcceptsEverySourceInItsOwnSchema(t *testing.T) {
	enum := regexp.MustCompile(`"sources":\s*\{[^}]*"enum":\s*\[([^\]]*)\]`).
		FindStringSubmatch(memoryInputSchema)
	if enum == nil {
		t.Fatal("could not find the sources enum in memoryInputSchema — this test asserts nothing until the pattern matches again")
	}
	var advertised []string
	for _, raw := range strings.Split(enum[1], ",") {
		if v := strings.Trim(strings.TrimSpace(raw), `"`); v != "" {
			advertised = append(advertised, v)
		}
	}
	if len(advertised) < 3 {
		t.Fatalf("expected at least facts/notes/documents in the schema enum, got %v", advertised)
	}
	for _, v := range advertised {
		got := parseSources([]string{v})
		if len(got) != 1 || string(got[0]) != v {
			t.Errorf("parseSources([%q]) = %v — the schema advertises %q, so a caller passing it "+
				"gets it SILENTLY DROPPED, which turns an explicit selector into no selector", v, got, v)
		}
	}
}

// TestParseSources_MatchesTheHTTPParser is the drift guard. Two hand-maintained
// copies of the same vocabulary exist — this one for the in-band tool and
// parseMemorySources for POST /v1/_memory/search — and they DID drift: the HTTP
// one handled notes, this one did not, so the same selector meant different things
// depending on which surface a caller used. Compared through the shared Source
// constants so adding a fourth kind fails here until both are updated.
func TestParseSources_MatchesTheHTTPParser(t *testing.T) {
	for _, s := range []memrank.Source{memrank.SourceFacts, memrank.SourceNotes, memrank.SourceDocuments} {
		got := parseSources([]string{string(s)})
		if len(got) != 1 || got[0] != s {
			t.Errorf("parseSources([%q]) = %v, want [%q] — the HTTP parser accepts it and this one must too",
				s, got, s)
		}
	}
	// Casing and padding are normalised, matching the HTTP side.
	if got := parseSources([]string{"  NOTES  "}); len(got) != 1 || got[0] != memrank.SourceNotes {
		t.Errorf("parseSources([\"  NOTES  \"]) = %v, want [notes]", got)
	}
	// An unknown value is still dropped rather than rejected — deliberate, and the
	// reason the missing case was invisible. Kept so the fix does not change it.
	if got := parseSources([]string{"facts", "not-a-source"}); len(got) != 1 || got[0] != memrank.SourceFacts {
		t.Errorf("parseSources with an unknown value = %v, want just [facts]", got)
	}
}

// TestMemoryInputSchema_SourcesEnumIsValidJSON guards the guard above: if the
// schema stops being parseable the first test fails loudly rather than skipping.
func TestMemoryInputSchema_SourcesEnumIsValidJSON(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(memoryInputSchema), &doc); err != nil {
		t.Fatalf("memoryInputSchema is not valid JSON: %v", err)
	}
}
