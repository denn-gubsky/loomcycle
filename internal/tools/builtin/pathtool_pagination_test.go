package builtin

import (
	"fmt"
	"strings"
	"testing"
)

// Pagination for `path op=ls` (RFC CV OQ2).
//
// A subject-homed fact store makes a directory exactly as large as the tenant's
// entity count, so `ls /facts` on a ten-thousand-subject tenant returned ten
// thousand entries in ONE response. The op accepted no limit and no cursor at
// all, so a caller had no way to ask for less. Postgres serves that listing; an
// agent's context window does not.

// seedSubjects creates n document dirents directly under one parent, in the
// fixture's agent scope. Zero-padded names so lexical order is numeric order —
// these tests are about page boundaries, not sort surprises.
func seedSubjects(t *testing.T, p *Path, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		seedAgent(t, p.Store, "/facts/", fmt.Sprintf("subj-%04d", i), "document")
	}
}

func lsNames(t *testing.T, out map[string]any) (names []string, truncated bool, next string) {
	t.Helper()
	if e, ok := out["error"].(string); ok && e != "" {
		t.Fatalf("ls refused: %s", e)
	}
	ents, _ := out["entries"].([]any)
	for _, e := range ents {
		m, _ := e.(map[string]any)
		n, _ := m["name"].(string)
		names = append(names, n)
	}
	truncated, _ = out["truncated"].(bool)
	next, _ = out["next_cursor"].(string)
	return names, truncated, next
}

// TestPathLs_DefaultLimitBoundsTheResponseAndSaysSo is the regression: an
// unbounded listing is what the RFC found. FAILS on the unfixed tool, which
// returns all 600 entries and reports no truncation.
func TestPathLs_DefaultLimitBoundsTheResponseAndSaysSo(t *testing.T) {
	p, ctx, _ := pathFixture(t)
	seedSubjects(t, p, 600)

	out, _ := pathExec(t, p, ctx, `{"op":"ls","path":"/facts"}`)
	names, truncated, next := lsNames(t, out)

	// The default cap is spelled as a literal here, not as lsDefaultLimit, so this
	// test COMPILES against the pre-pagination tool and fails on its behaviour
	// (600 entries, no truncation) rather than on a missing identifier. A
	// fail-before that is really a build error demonstrates nothing.
	const wantDefaultCap = 500
	if len(names) != wantDefaultCap {
		t.Errorf("ls returned %d entries, want the %d default cap — an unbounded listing is what "+
			"blows the caller's context", len(names), wantDefaultCap)
	}
	if !truncated {
		t.Errorf("truncated flag not set; a bound the caller cannot see is a lie rather than a bound")
	}
	if next == "" {
		t.Errorf("no next_cursor on a truncated listing, so the caller can never reach the rest")
	}
}

// TestPathLs_CursorWalksEveryEntryExactlyOnce — the property that makes the
// directory usable rather than merely bounded.
func TestPathLs_CursorWalksEveryEntryExactlyOnce(t *testing.T) {
	p, ctx, _ := pathFixture(t)
	const total = 250
	seedSubjects(t, p, total)

	seen := map[string]int{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatalf("cursor did not terminate after %d pages — a non-advancing cursor loops forever", pages)
		}
		body := `{"op":"ls","path":"/facts","limit":40}`
		if cursor != "" {
			body = `{"op":"ls","path":"/facts","limit":40,"cursor":"` + cursor + `"}`
		}
		out, _ := pathExec(t, p, ctx, body)
		names, truncated, next := lsNames(t, out)
		for _, n := range names {
			seen[n]++
		}
		if !truncated {
			break
		}
		cursor = next
	}
	if len(seen) != total {
		t.Errorf("walked %d distinct entries, want %d — the cursor skipped or lost some", len(seen), total)
	}
	for n, c := range seen {
		if c != 1 {
			t.Errorf("entry %s returned %d times, want exactly once — a page boundary that repeats "+
				"an entry makes the walk non-terminating in the caller's eyes", n, c)
		}
	}
}

// TestPathLs_ExplicitLimitIsHonouredAndCapped.
func TestPathLs_ExplicitLimitIsHonouredAndCapped(t *testing.T) {
	p, ctx, _ := pathFixture(t)
	seedSubjects(t, p, 30)

	out, _ := pathExec(t, p, ctx, `{"op":"ls","path":"/facts","limit":10}`)
	names, truncated, _ := lsNames(t, out)
	if len(names) != 10 || !truncated {
		t.Errorf("limit=10 gave %d entries (truncated=%v), want 10 and truncated", len(names), truncated)
	}

	// Over the max: clamped, not refused — a caller asking for too much gets the
	// most the tool will give rather than an error it has to special-case.
	out, _ = pathExec(t, p, ctx, `{"op":"ls","path":"/facts","limit":99999}`)
	names, truncated, _ = lsNames(t, out)
	if len(names) != 30 || truncated {
		t.Errorf("limit over the cap gave %d entries (truncated=%v), want all 30 and no truncation",
			len(names), truncated)
	}
}

// TestPathLs_SmallDirectoryIsUnchanged — the compatibility floor. Below the
// default cap a listing must look exactly as it did: no truncated flag, no
// cursor, same entries.
func TestPathLs_SmallDirectoryIsUnchanged(t *testing.T) {
	p, ctx, _ := pathFixture(t)
	seedSubjects(t, p, 3)

	out, _ := pathExec(t, p, ctx, `{"op":"ls","path":"/facts"}`)
	names, truncated, next := lsNames(t, out)
	if len(names) != 3 || truncated || next != "" {
		t.Errorf("small listing = %v truncated=%v next=%q, want the pre-pagination shape exactly",
			names, truncated, next)
	}
}

// TestPathLs_RecursiveListingPaginatesOnFullPath. A recursive listing is flat, so
// names repeat across directories and only the full path is a stable key. Before
// this change the recursive branch did not sort at all, so any cursor over it
// would have been a bet on the store's row order.
func TestPathLs_RecursiveListingPaginatesOnFullPath(t *testing.T) {
	p, ctx, _ := pathFixture(t)
	// The same NAME under two parents — indistinguishable by name alone.
	for i := 0; i < 40; i++ {
		seedAgent(t, p.Store, "/facts/a/", fmt.Sprintf("subj-%04d", i), "document")
		seedAgent(t, p.Store, "/facts/b/", fmt.Sprintf("subj-%04d", i), "document")
	}

	seen := map[string]int{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatalf("recursive cursor did not terminate after %d pages", pages)
		}
		body := `{"op":"ls","path":"/facts","recursive":true,"limit":15}`
		if cursor != "" {
			body = `{"op":"ls","path":"/facts","recursive":true,"limit":15,"cursor":"` + cursor + `"}`
		}
		out, _ := pathExec(t, p, ctx, body)
		if e, ok := out["error"].(string); ok && e != "" {
			t.Fatalf("recursive ls refused: %s", e)
		}
		ents, _ := out["entries"].([]any)
		for _, e := range ents {
			m, _ := e.(map[string]any)
			fp, _ := m["full_path"].(string)
			seen[fp]++
		}
		truncated, _ := out["truncated"].(bool)
		if !truncated {
			break
		}
		cursor, _ = out["next_cursor"].(string)
	}
	if len(seen) != 80 {
		t.Errorf("walked %d distinct full paths, want 80 — paginating a flat listing on NAME would "+
			"collapse the two directories' identical names", len(seen))
	}
}

// TestPathLs_AHandBuiltCursorIsRefused. The cursor is opaque so its encoding can
// change (or move into the store as a keyset predicate) without a wire change —
// which only holds if a caller cannot successfully invent one.
func TestPathLs_AHandBuiltCursorIsRefused(t *testing.T) {
	p, ctx, _ := pathFixture(t)
	seedSubjects(t, p, 5)

	_, res := pathExec(t, p, ctx, `{"op":"ls","path":"/facts","cursor":"subj-0002"}`)
	if !res.IsError {
		t.Fatalf("a hand-built cursor was ACCEPTED; the cursor is only opaque if one cannot be invented")
	}
	if !strings.Contains(res.Text, "not a value this tool issued") {
		t.Errorf("refusal = %q, want it to name the fix (pass back next_cursor verbatim)", res.Text)
	}
}
