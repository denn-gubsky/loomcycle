package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// RFC GB A1.2 — find_duplicates: the copies in a set of documents, by content
// hash and by title, each pointed at its original. Every fixture here is
// invented.

// dupDocIn writes a user-scope document at path with one chunk per body, and
// returns its id and root chunk id.
func dupDocIn(t *testing.T, d *Document, ctx context.Context, path, title, typ string, bodies ...string) (string, string) {
	t.Helper()
	typeField := ""
	if typ != "" {
		typeField = fmt.Sprintf(`,"type":%q`, typ)
	}
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"user","title":%q,"path":%q%s}`, title, path, typeField))
	if r.IsError {
		t.Fatalf("create_document %s: %s", path, r.Text)
	}
	doc, root := asStr(out["document_id"]), asStr(out["root_chunk_id"])
	for i, b := range bodies {
		bj, _ := json.Marshal(b)
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"part %d","body":%s}`, doc, i+1, bj)); r.IsError {
			t.Fatalf("create_chunk %s: %s", path, r.Text)
		}
	}
	return doc, root
}

// findDups runs find_duplicates and returns its answer, failing on a refusal.
func findDups(t *testing.T, d *Document, ctx context.Context, body string) map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, body)
	if r.IsError {
		t.Fatalf("%s: %s", body, r.Text)
	}
	return out
}

func dupPairs(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	list, _ := out["pairs"].([]any)
	pairs := make([]map[string]any, len(list))
	for i, p := range list {
		pairs[i] = p.(map[string]any)
	}
	return pairs
}

func dupCount(out map[string]any, name string) int {
	c, _ := out["counts"].(map[string]any)
	n, _ := c[name].(float64)
	return int(n)
}

// duplicateStatements reads every duplicate_of row in the user scope, whatever
// its status, as "from>to:status".
func duplicateStatements(t *testing.T, d *Document, ctx context.Context) []string {
	t.Helper()
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.query(ctx, key, `SELECT from_id, to_id, status FROM chunk_edges WHERE kind = 'duplicate_of' ORDER BY from_id, to_id`)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range res.Rows {
		out = append(out, asStr(r[0])+">"+asStr(r[1])+":"+asStr(r[2]))
	}
	return out
}

func TestDuplicateTitleKey_StripsEveryCopyMarkerRepeatedly(t *testing.T) {
	for in, want := range map[string]struct {
		key    string
		marked bool
	}{
		"Trip plan":                            {"trip plan", false},
		"  TRIP Plan ":                         {"trip plan", false},
		"Copy of Trip plan":                    {"trip plan", true},
		"copy of Trip plan":                    {"trip plan", true},
		"Копия Trip plan":                      {"trip plan", true},
		"Копия - Trip plan":                    {"trip plan", true},
		"Kopie von Trip plan":                  {"trip plan", true},
		"Copie de Trip plan":                   {"trip plan", true},
		"Trip plan (1)":                        {"trip plan", true},
		"Trip plan (27)":                       {"trip plan", true},
		"Trip plan - Copy":                     {"trip plan", true},
		"Copy of Copy of Trip plan (2) - Copy": {"trip plan", true},
		"Копия Копия - План поездки (3)": {"план поездки", true},
		"Trip plan (draft)": {"trip plan (draft)", false},
	} {
		key, marked := duplicateTitleKey(in)
		if key != want.key || marked != want.marked {
			t.Errorf("duplicateTitleKey(%q) = %q, %v; want %q, %v", in, key, marked, want.key, want.marked)
		}
	}
}

// TestDuplicateTitleKey_OnlyACopyNumberIsAMarker: a trailing number in
// parentheses is a copy marker only in the form a file manager or editor
// writes, one or two digits. A year is part of the title, so two budgets of
// different years are not copies of each other.
func TestDuplicateTitleKey_OnlyACopyNumberIsAMarker(t *testing.T) {
	for in, want := range map[string]struct {
		key    string
		marked bool
	}{
		"Budget (2)":    {"budget", true},
		"Budget (99)":   {"budget", true},
		"Budget (100)":  {"budget (100)", false},
		"Budget (2023)": {"budget (2023)", false},
	} {
		key, marked := duplicateTitleKey(in)
		if key != want.key || marked != want.marked {
			t.Errorf("duplicateTitleKey(%q) = %q, %v; want %q, %v", in, key, marked, want.key, want.marked)
		}
	}

	d, ctx, _ := documentFixture(t)
	dupDocIn(t, d, ctx, "/years/a", "Budget (2023)", "", "rent")
	dupDocIn(t, d, ctx, "/years/b", "Budget (2024)", "", "rent and food")
	if pairs := dupPairs(t, findDups(t, d, ctx, `{"op":"find_duplicates","scope":"user","under_path":"/years","basis":["title"]}`)); len(pairs) != 0 {
		t.Errorf("budgets of two years were paired as copies: %v", pairs)
	}
}

func TestDuplicateContentHash_IgnoresWhitespaceAndRootTitleButNotAWord(t *testing.T) {
	a := duplicateContentHash([]string{"", "alpha  beta\n\n gamma "})
	if a == "" {
		t.Fatal("a document with text has no hash")
	}
	if b := duplicateContentHash([]string{"alpha beta", "gamma"}); b != a {
		t.Error("whitespace runs and the split between chunks changed the hash")
	}
	if c := duplicateContentHash([]string{"alpha beta delta"}); c == a {
		t.Error("one changed word left the hash the same")
	}
	if s := duplicateContentHash([]string{"", " \n\t"}); s != "" {
		t.Errorf("a stub's hash = %q, want none", s)
	}

	// Through the op: the root title is not part of what is hashed.
	d, ctx, _ := documentFixture(t)
	one, _ := dupDocIn(t, d, ctx, "/dups/one", "Field notes", "", "alpha  beta\n gamma")
	two, _ := dupDocIn(t, d, ctx, "/dups/two", "Season summary", "", "alpha beta", "gamma")
	dupDocIn(t, d, ctx, "/dups/three", "Field notes", "", "alpha beta delta")
	dupDocIn(t, d, ctx, "/dups/stub1", "Empty one", "")
	dupDocIn(t, d, ctx, "/dups/stub2", "Empty two", "")
	out := findDups(t, d, ctx, `{"op":"find_duplicates","scope":"user","under_path":"/dups","basis":["sha256"]}`)
	pairs := dupPairs(t, out)
	if len(pairs) != 1 {
		t.Fatalf("pairs = %v, want only the two documents with the same text", pairs)
	}
	p := pairs[0]
	if p["basis"] != "sha256" || p["confidence"] != 1.0 {
		t.Errorf("pair = %v, want basis sha256 at confidence 1", p)
	}
	if ends := map[string]bool{asStr(p["copy_id"]): true, asStr(p["original_id"]): true}; !ends[one] || !ends[two] {
		t.Errorf("pair joins %v and %v, want %s and %s", p["copy_id"], p["original_id"], one, two)
	}
	if dupCount(out, "documents") != 5 || dupCount(out, "sha256") != 1 || dupCount(out, "title") != 0 {
		t.Errorf("counts = %v", out["counts"])
	}
}

func TestDuplicateOriginal_PrefersTheUnmarkedThenTheEarlierThenTheLowerID(t *testing.T) {
	mk := func(id string, created int64, marked bool) *dupDoc {
		return &dupDoc{id: id, createdAt: created, marked: marked}
	}
	for name, c := range map[string]struct {
		members []*dupDoc
		marker  bool
		want    string
	}{
		"the unmarked side, though later":  {[]*dupDoc{mk("a", 1, true), mk("b", 2, false)}, true, "b"},
		"both marked: the earlier":         {[]*dupDoc{mk("a", 2, true), mk("b", 1, true)}, true, "b"},
		"neither marked: the earlier":      {[]*dupDoc{mk("a", 2, false), mk("b", 1, false)}, true, "b"},
		"markers not applied: the earlier": {[]*dupDoc{mk("a", 2, false), mk("b", 3, true)}, false, "a"},
		"a tie: the lower id":              {[]*dupDoc{mk("b", 5, false), mk("a", 5, false)}, true, "a"},
		"of three, among the unmarked":     {[]*dupDoc{mk("a", 1, true), mk("c", 3, false), mk("b", 2, false)}, true, "b"},
	} {
		if got := duplicateOriginal(c.members, c.marker); got.id != c.want {
			t.Errorf("%s: original = %s, want %s", name, got.id, c.want)
		}
	}
}

func TestFindDuplicates_TitleMatchPointsTheMarkedCopyAtTheOriginal(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	// The copy is created first: the marker decides, not the date.
	cp, _ := dupDocIn(t, d, ctx, "/trip/copy", "Copy of Trip plan", "note", "pack the tent")
	orig, _ := dupDocIn(t, d, ctx, "/trip/orig", "Trip plan", "note", "pack the stove")
	// Same title, another kind: not a duplicate.
	dupDocIn(t, d, ctx, "/trip/report", "Trip plan (2)", "report", "costs")
	out := findDups(t, d, ctx, `{"op":"find_duplicates","scope":"user","under_path":"/trip"}`)
	pairs := dupPairs(t, out)
	if len(pairs) != 1 {
		t.Fatalf("pairs = %v, want the one same-kind title match", pairs)
	}
	p := pairs[0]
	if p["copy_id"] != cp || p["original_id"] != orig || p["basis"] != "title" || p["confidence"] != 0.7 {
		t.Errorf("pair = %v, want %s → %s by title at 0.7", p, cp, orig)
	}
	if p["copy_title"] != "Copy of Trip plan" || p["original_title"] != "Trip plan" {
		t.Errorf("titles = %v / %v", p["copy_title"], p["original_title"])
	}
}

func TestFindDuplicates_SameTextAndTitleIsReportedOnceAsSHA256(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	dupDocIn(t, d, ctx, "/kit/a", "Kit list", "", "rope and pegs")
	dupDocIn(t, d, ctx, "/kit/b", "Kit list (1)", "", "rope and pegs")
	out := findDups(t, d, ctx, `{"op":"find_duplicates","scope":"user","under_path":"/kit"}`)
	pairs := dupPairs(t, out)
	if len(pairs) != 1 || pairs[0]["basis"] != "sha256" {
		t.Fatalf("pairs = %v, want one pair, by sha256", pairs)
	}
	if dupCount(out, "pairs") != 1 || dupCount(out, "sha256") != 1 || dupCount(out, "title") != 0 {
		t.Errorf("counts = %v", out["counts"])
	}
}

func TestFindDuplicates_GroupOfThreeIsAStarOnTheEarliest(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	first, _ := dupDocIn(t, d, ctx, "/star/1", "Recipe A", "", "two eggs, one cup of flour")
	dupDocIn(t, d, ctx, "/star/2", "Recipe B", "", "two eggs, one cup of flour")
	dupDocIn(t, d, ctx, "/star/3", "Recipe C", "", "two eggs, one cup of flour")
	pairs := dupPairs(t, findDups(t, d, ctx, `{"op":"find_duplicates","scope":"user","under_path":"/star"}`))
	if len(pairs) != 2 {
		t.Fatalf("pairs = %v, want two copies", pairs)
	}
	for _, p := range pairs {
		if p["original_id"] != first {
			t.Errorf("a copy points at %v, not at the group's original %s (a chain, not a star)", p["original_id"], first)
		}
	}
}

func TestFindDuplicates_ReportModeWritesNothing(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	dupDocIn(t, d, ctx, "/rep/a", "Log", "", "day one")
	dupDocIn(t, d, ctx, "/rep/b", "Log - Copy", "", "day one")
	out := findDups(t, d, ctx, `{"op":"find_duplicates","scope":"user","under_path":"/rep"}`)
	pairs := dupPairs(t, out)
	if len(pairs) != 1 || pairs[0]["statement_id"] != nil {
		t.Fatalf("pairs = %v, want one reported pair and no statement", pairs)
	}
	if got := duplicateStatements(t, d, ctx); len(got) != 0 {
		t.Errorf("a report wrote statements: %v", got)
	}
	if dupCount(out, "proposed") != 0 {
		t.Errorf("counts = %v", out["counts"])
	}
}

func TestFindDuplicates_ProposesFromTheCopysRootToTheOriginalsRoot(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, origRoot := dupDocIn(t, d, ctx, "/p/a", "Menu", "", "soup of the day")
	_, copyRoot := dupDocIn(t, d, ctx, "/p/b", "Copy of Menu", "", "soup of the day")
	out := findDups(t, d, ctx, `{"op":"find_duplicates","scope":"user","under_path":"/p","propose":true}`)
	pairs := dupPairs(t, out)
	if len(pairs) != 1 || asStr(pairs[0]["statement_id"]) == "" {
		t.Fatalf("pairs = %v, want one proposed pair", pairs)
	}
	got := duplicateStatements(t, d, ctx)
	if want := copyRoot + ">" + origRoot + ":proposed"; len(got) != 1 || got[0] != want {
		t.Fatalf("statements = %v, want %s", got, want)
	}
	if dupCount(out, "proposed") != 1 {
		t.Errorf("counts = %v", out["counts"])
	}
	e := copyExec(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, pairs[0]["statement_id"]))
	stmt, _ := e["statement"].(map[string]any)
	if stmt == nil {
		stmt = e
	}
	attrs, _ := stmt["attributes"].(map[string]any)
	if attrs["basis"] != "sha256" || stmt["confidence"] != 1.0 {
		t.Errorf("statement = %v, want basis sha256 at confidence 1", stmt)
	}
	// Run again: the pair now has a statement, so it is skipped.
	again := findDups(t, d, ctx, `{"op":"find_duplicates","scope":"user","under_path":"/p","propose":true}`)
	if dupCount(again, "skipped_existing") != 1 || dupCount(again, "proposed") != 0 {
		t.Errorf("second run counts = %v", again["counts"])
	}
	if got := duplicateStatements(t, d, ctx); len(got) != 1 {
		t.Errorf("a second run wrote again: %v", got)
	}
}

// TestFindDuplicates_SkipsAPairWithAStatementInEitherDirectionAndAnyStatus: a
// pair already said to be duplicates (or refused as such) is a decision made;
// filing it again would re-open what a reviewer closed.
func TestFindDuplicates_SkipsAPairWithAStatementInEitherDirectionAndAnyStatus(t *testing.T) {
	for _, c := range []struct {
		name    string
		reverse bool
		status  string
		expire  bool
	}{
		{"proposed, copy to original", false, edgeStatusProposed, false},
		{"asserted, original to copy", true, edgeStatusAsserted, false},
		{"rejected, copy to original", false, edgeStatusRejected, false},
		{"rejected, original to copy", true, edgeStatusRejected, false},
		{"expired", false, edgeStatusAsserted, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, ctx, _ := documentFixture(t)
			_, origRoot := dupDocIn(t, d, ctx, "/s/a", "Budget", "", "rent and food")
			_, copyRoot := dupDocIn(t, d, ctx, "/s/b", "Copy of Budget", "", "rent and food")
			key, mscope, err := d.resolveScope(ctx, "user")
			if err != nil {
				t.Fatal(err)
			}
			from, to := copyRoot, origRoot
			if c.reverse {
				from, to = origRoot, copyRoot
			}
			seedStatus := c.status
			if seedStatus == edgeStatusRejected {
				seedStatus = edgeStatusProposed // rejected below, as a ruling leaves it
			}
			w := edgeWrite{from: from, to: to, kind: "duplicate_of", method: edgeMethodManual, status: seedStatus}
			res, refuse, err := d.writeStatement(ctx, key, mscope, w)
			if err != nil || refuse != nil {
				t.Fatalf("seed: %v %v", err, refuse)
			}
			if c.status == edgeStatusRejected {
				if err := d.exec(ctx, key, `UPDATE chunk_edges SET status = ?, judge_reason = 'not a copy' WHERE id = ?`, edgeStatusRejected, res.ID); err != nil {
					t.Fatal(err)
				}
			}
			if c.expire {
				if err := d.exec(ctx, key, `UPDATE chunk_edges SET expired_at = 1 WHERE id = ?`, res.ID); err != nil {
					t.Fatal(err)
				}
			}
			out := findDups(t, d, ctx, `{"op":"find_duplicates","scope":"user","under_path":"/s","propose":true}`)
			pairs := dupPairs(t, out)
			if len(pairs) != 1 || pairs[0]["skipped"] != "exists" || pairs[0]["statement_id"] != nil {
				t.Fatalf("pairs = %v, want the one pair skipped as exists", pairs)
			}
			if dupCount(out, "skipped_existing") != 1 || dupCount(out, "proposed") != 0 {
				t.Errorf("counts = %v", out["counts"])
			}
			if got := duplicateStatements(t, d, ctx); len(got) != 1 {
				t.Errorf("statements = %v, want only the seeded one", got)
			}
		})
	}
}

// TestFindDuplicates_TheSeededKindIsAcceptedUnderAConfirmedRelationsSection: a
// tenant whose confirmed ontology declares Relations (without naming
// duplicate_of) has the seed kind in force, so the proposal is valid, with its
// basis qualifier.
func TestFindDuplicates_TheSeededKindIsAcceptedUnderAConfirmedRelationsSection(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, relationsOntologyMD, memrank.OntologyConfirmedStatus)
	dupDocIn(t, d, ctx, "/o/a", "Minutes", "", "the board met")
	dupDocIn(t, d, ctx, "/o/b", "Minutes (1)", "", "the board met twice")
	out := findDups(t, d, ctx, `{"op":"find_duplicates","scope":"user","under_path":"/o","propose":true}`)
	pairs := dupPairs(t, out)
	if len(pairs) != 1 || asStr(pairs[0]["statement_id"]) == "" {
		t.Fatalf("pairs = %v, want one proposed title pair", pairs)
	}
	if pairs[0]["ontology_valid"] != true {
		t.Errorf("the seeded kind was annotated invalid under a confirmed Relations section: %v", pairs[0])
	}
}

func TestFindDuplicates_RefusesASetOverTheCap(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ids := make([]string, findDuplicatesMaxDocuments+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("%032x", i+1)
	}
	b, _ := json.Marshal(ids)
	_, r := docExec(t, d, ctx, `{"op":"find_duplicates","scope":"user","document_ids":`+string(b)+`}`)
	if !r.IsError || !strings.Contains(r.Text, fmt.Sprint(findDuplicatesMaxDocuments)) {
		t.Fatalf("a set over the cap was not refused: %s", r.Text)
	}
	if findDuplicatesMaxDocuments != 5000 {
		t.Errorf("cap = %d, want 5000", findDuplicatesMaxDocuments)
	}
}

func TestFindDuplicates_MethodNeedsTheStatementMethodsGrant(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	dupDocIn(t, d, ctx, "/m/a", "Atlas", "", "maps")
	_, r := docExec(t, d, ctx, `{"op":"find_duplicates","scope":"user","under_path":"/m","propose":true,"method":"rule:dups"}`)
	if !r.IsError || !strings.Contains(r.Text, "method_not_granted") {
		t.Fatalf("an ungranted method was not refused: %s", r.Text)
	}
}
