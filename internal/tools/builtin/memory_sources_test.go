package builtin

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// advertisedSources reads the `sources` enum out of the tool's own input schema.
// Driven off the schema text rather than a hand-written list ON PURPOSE: a
// hand-written list is the same kind of second copy that caused the bug, and the
// guard this replaces proved it — it asserted over a literal
// {facts, notes, documents} and therefore went on passing when `traces` was added
// to the schema and never reached the HTTP parser. A guard containing a copy of
// the thing it guards drifts with it.
func advertisedSources(t *testing.T) []string {
	t.Helper()
	enum := regexp.MustCompile(`"sources":\s*\{[^}]*"enum":\s*\[([^\]]*)\]`).
		FindStringSubmatch(memoryInputSchema)
	if enum == nil {
		t.Fatal("could not find the sources enum in memoryInputSchema — this test asserts nothing until the pattern matches again")
	}
	var out []string
	for _, raw := range strings.Split(enum[1], ",") {
		if v := strings.Trim(strings.TrimSpace(raw), `"`); v != "" {
			out = append(out, v)
		}
	}
	if len(out) < 4 {
		t.Fatalf("expected at least facts/notes/documents/traces in the schema enum, got %v", out)
	}
	return out
}

// TestParseSources_AcceptsEverySourceInItsOwnSchema.
//
// The invariant: the parser must accept every value the schema advertises. A
// schema-advertised value that the parser drops turns an explicit selector into NO
// selector, and that fails in two opposite directions from one cause — `search`
// widens to every plane and looks correct, while `recall` falls back to its
// facts-only default and returns nothing.
//
// Both known instances were exactly this. `notes` was missing from the tool's copy;
// `traces` was missing from the HTTP copy, so a trace search came back quietly full
// of facts. There is now ONE parser, which is what makes this single assertion
// cover every surface.
func TestParseSources_AcceptsEverySourceInItsOwnSchema(t *testing.T) {
	for _, v := range advertisedSources(t) {
		got, err := memrank.ParseSources([]string{v})
		if err != nil {
			t.Errorf("ParseSources([%q]) errored: %v — the schema advertises it", v, err)
			continue
		}
		if len(got) != 1 || string(got[0]) != v {
			t.Errorf("ParseSources([%q]) = %v — the schema advertises %q, so a caller passing it "+
				"gets it SILENTLY DROPPED, which turns an explicit selector into no selector", v, got, v)
		}
	}
}

// TestParseSources_IsTheOnlyCopy pins the structural fix. Two hand-maintained
// copies of a predicate that decides which rows get DROPPED is the shape of this
// bug, and it drifted twice before the copies were merged. If a surface grows its
// own mapping again this is the note that says why not to.
func TestParseSources_IsTheOnlyCopy(t *testing.T) {
	for _, v := range advertisedSources(t) {
		got, err := memrank.ParseSources([]string{strings.ToUpper("  " + v + "  ")})
		if err != nil || len(got) != 1 || string(got[0]) != v {
			t.Errorf("ParseSources did not normalise casing/padding for %q: got %v, err %v", v, got, err)
		}
	}
}

// TestParseSources_UnknownAloneIsRefusedButMixedIsDropped.
//
// The two cases differ in kind and that is the whole point of the change. An
// unknown value ALONGSIDE a known one is dropped, so a newer caller's additive
// value cannot break an older runtime. An unknown value on its OWN leaves an empty
// selector, which every op reads as "no selector" and answers with its DEFAULT —
// so the caller that typed one wrong name gets a WIDER result set while believing
// it filtered. That is refused.
func TestParseSources_UnknownAloneIsRefusedButMixedIsDropped(t *testing.T) {
	got, err := memrank.ParseSources([]string{"facts", "not-a-source"})
	if err != nil {
		t.Errorf("a recognised value alongside an unknown one must still parse, got %v", err)
	} else if len(got) != 1 || got[0] != memrank.SourceFacts {
		t.Errorf("ParseSources([facts, not-a-source]) = %v, want just [facts]", got)
	}

	if _, err := memrank.ParseSources([]string{"trace"}); !errors.Is(err, memrank.ErrSourcesUnrecognised) {
		t.Errorf("a selector where NOTHING is recognised must be refused, not silently widened to "+
			"the op default; got err=%v", err)
	}

	// Empty stays empty: no selector is a legitimate request meaning "the default".
	got, err = memrank.ParseSources(nil)
	if err != nil || got != nil {
		t.Errorf("ParseSources(nil) = %v, %v — an absent selector is not an error", got, err)
	}
}

// TestMemoryInputSchema_SourcesEnumIsValidJSON guards the guard: if the schema
// stops being parseable the tests above fail loudly rather than skipping.
func TestMemoryInputSchema_SourcesEnumIsValidJSON(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(memoryInputSchema), &doc); err != nil {
		t.Fatalf("memoryInputSchema is not valid JSON: %v", err)
	}
}
