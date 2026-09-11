package builtin

import (
	"fmt"
	"testing"
	"time"
)

// The PATHOLOGICAL directory (RFC CV OQ2).
//
// The cardinality question was answered from the schema: per-subject reads scale,
// because chunks_doc_parent_pos makes a dossier read an index lookup whose cost is
// independent of how many subjects exist. What was never measured is the one case
// the schema does not answer — a SINGLE directory with ten thousand children,
// which is exactly what `/facts` becomes on a ten-thousand-entity tenant once
// facts are subject-homed.
//
// The risk was never the row count. Postgres serves ten thousand dirents; the
// RESPONSE and the agent's context window do not. So what this measures is that
// the listing stays bounded at that width, and that a caller can still walk the
// whole directory — a bound that made the directory un-enumerable would have
// broken the entity directory that subject-homing exists to provide, which is
// precisely why a hard cap with a truncation flag was rejected in favour of a
// cursor.
func TestPathLs_TenThousandChildrenStayBoundedAndWalkable(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 10k dirents; skipped under -short")
	}
	p, ctx, _ := pathFixture(t)

	const n = 10000
	seeded := time.Now()
	for i := 0; i < n; i++ {
		seedAgent(t, p.Store, "/facts/", fmt.Sprintf("subj-%05d", i), "document")
	}
	t.Logf("seeded %d dirents in one directory in %s", n, time.Since(seeded).Round(time.Millisecond))

	// 1. A DEFAULT listing is bounded. This is the assertion the whole OQ is about:
	//    without it the response carries ten thousand entries.
	start := time.Now()
	out, r := pathExec(t, p, ctx, `{"op":"ls","scope":"agent","path":"/facts"}`)
	if r.IsError {
		t.Fatalf("ls: %s", r.Text)
	}
	first, truncated, next := lsNames(t, out)
	t.Logf("first page: %d entries in %s", len(first), time.Since(start).Round(time.Millisecond))
	if len(first) > lsDefaultLimit {
		t.Errorf("a default listing returned %d entries on a %d-child directory — the response "+
			"is unbounded, which is what the agent's context window cannot take", len(first), n)
	}
	if !truncated || next == "" {
		t.Fatalf("a %d-child directory did not report truncation (truncated=%v next=%q) — a "+
			"silent bound is indistinguishable from a directory that holds less than it does",
			n, truncated, next)
	}

	// 2. The whole directory is still WALKABLE, and every child appears exactly
	//    once. A cursor that skipped or repeated entries would make the entity
	//    directory unusable in a way no single page could reveal.
	seen := make(map[string]int, n)
	for _, name := range first {
		seen[name]++
	}
	pages, cursor := 1, next
	walk := time.Now()
	for cursor != "" {
		if pages > n/10+10 {
			t.Fatalf("walk did not terminate after %d pages — a cursor that does not advance "+
				"turns a bounded listing into an infinite one", pages)
		}
		out, r := pathExec(t, p, ctx, fmt.Sprintf(
			`{"op":"ls","scope":"agent","path":"/facts","cursor":%q}`, cursor))
		if r.IsError {
			t.Fatalf("ls page %d: %s", pages+1, r.Text)
		}
		names, _, nxt := lsNames(t, out)
		if len(names) == 0 && nxt != "" {
			t.Fatalf("page %d returned nothing but offered another cursor", pages+1)
		}
		for _, name := range names {
			seen[name]++
		}
		cursor = nxt
		pages++
	}
	t.Logf("walked %d pages in %s", pages, time.Since(walk).Round(time.Millisecond))

	if len(seen) != n {
		t.Errorf("the walk saw %d distinct children, want %d — a cursor that skips entries "+
			"hides facts that are really there", len(seen), n)
	}
	for name, count := range seen {
		if count != 1 {
			t.Errorf("%s appeared %d times — a repeating cursor inflates the directory", name, count)
			break
		}
	}
}
