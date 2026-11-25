package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC HA: path op=ls with_counts and the listing filters (name_contains, type,
// status) over a tenant-scope tree of documents.

// listingDoc creates a tenant document at path and returns its id.
func listingDoc(t *testing.T, d *Document, ctx context.Context, path, title, typ, status string) string {
	t.Helper()
	q := fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":%q`, title, path)
	if typ != "" {
		q += fmt.Sprintf(`,"type":%q`, typ)
	}
	if status != "" {
		q += fmt.Sprintf(`,"status":%q`, status)
	}
	out, r := docExec(t, d, ctx, q+`}`)
	if r.IsError {
		t.Fatalf("create_document %s: %s", path, r.Text)
	}
	return out["document_id"].(string)
}

// alsoName files the document already named at from under a second name, as
// a set_path from another folder would leave it.
func alsoName(t *testing.T, st store.Store, from, to string) {
	t.Helper()
	ctx := context.Background()
	fp, fn, _ := splitPath(from)
	row, err := st.DirentGet(ctx, "tnt", "tenant", "", fp, fn)
	if err != nil {
		t.Fatalf("dirent %s: %v", from, err)
	}
	tp, tn, _ := splitPath(to)
	if _, err := st.DirentCreate(ctx, store.DirentRow{TenantID: "tnt", Scope: "tenant", ParentPath: tp, Name: tn,
		Kind: "document", ResourceRef: row.ResourceRef}); err != nil {
		t.Fatalf("second name %s: %v", to, err)
	}
}

type lsAnswer struct {
	Path       string            `json:"path"`
	Entries    []map[string]any  `json:"entries"`
	Truncated  bool              `json:"truncated"`
	NextCursor string            `json:"next_cursor"`
	Filters    map[string]string `json:"filters"`
	Expanded   []string          `json:"type_expanded_to"`
}

func lsCall(t *testing.T, p *Path, ctx context.Context, body string) lsAnswer {
	t.Helper()
	res, err := p.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Path hard error: %v", err)
	}
	if res.IsError {
		t.Fatalf("%s: %s", body, res.Text)
	}
	var out lsAnswer
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode %s: %v", res.Text, err)
	}
	return out
}

func (a lsAnswer) entry(name string) map[string]any {
	for _, e := range a.Entries {
		if e["name"] == name {
			return e
		}
	}
	return nil
}

func (a lsAnswer) names() []string {
	out := make([]string, 0, len(a.Entries))
	for _, e := range a.Entries {
		out = append(out, e["name"].(string))
	}
	return out
}

// count is an entry's documents, -1 when it carries none.
func (a lsAnswer) count(t *testing.T, name string) int {
	t.Helper()
	e := a.entry(name)
	if e == nil {
		t.Fatalf("ls %s has no entry %q: %v", a.Path, name, a.names())
	}
	n, ok := e["documents"].(float64)
	if !ok {
		return -1
	}
	return int(n)
}

// TestPathListing_CountsEachReadableDocumentUnderAFolderOnce covers the count's
// shape on both tiers: nested folders, a document with children (counted
// without itself), a document named twice under one folder (once), an empty
// folder (0) and a leaf document (no count).
func TestPathListing_CountsEachReadableDocumentUnderAFolderOnce(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		ctx := tenantDocCtx(base, nil)
		for _, p := range []string{"/kb/a/x", "/kb/a/y", "/kb/a/deep/z", "/kb/b/w", "/kb/c", "/kb/c/k1", "/kb/c/sub/k2", "/kb/leaf"} {
			listingDoc(t, d, ctx, p, "t "+p, "", "")
		}
		alsoName(t, d.Store, "/kb/a/x", "/kb/a/deep/x2")
		p := &Path{Store: d.Store, Documents: d}
		if r := pathACLExec(t, p, ctx, `{"op":"mkdir","scope":"tenant","path":"/kb/empty"}`); r.IsError {
			t.Fatalf("mkdir: %s", r.Text)
		}

		got := lsCall(t, p, ctx, `{"op":"ls","scope":"tenant","path":"/kb","with_counts":true}`)
		for name, want := range map[string]int{"a": 3, "b": 1, "c": 2, "empty": 0, "leaf": -1} {
			if n := got.count(t, name); n != want {
				t.Errorf("/kb/%s documents = %d, want %d (%v)", name, n, want, got.entry(name))
			}
		}
		if _, ok := got.entry("a")["documents_capped"]; ok {
			t.Errorf("a small count must not say capped: %v", got.entry("a"))
		}
		// x is named under deep too, so deep counts it there.
		if n := lsCall(t, p, ctx, `{"op":"ls","scope":"tenant","path":"/kb/a","with_counts":true}`).count(t, "deep"); n != 2 {
			t.Errorf("/kb/a/deep documents = %d, want 2", n)
		}
		if got.Filters != nil {
			t.Errorf("an unfiltered listing echoed filters: %v", got.Filters)
		}
	})
}

// TestPathListing_UnreadableDocumentsNeitherCountNorSurfaceAFolder: a document
// the caller may not read — at any of its names — is absent from the count and
// cannot make a folder appear under a filter.
func TestPathListing_UnreadableDocumentsNeitherCountNorSurfaceAFolder(t *testing.T) {
	d, base, st := documentFixture(t)
	seed := tenantDocCtx(base, nil)
	listingDoc(t, d, seed, "/kb/open/p1", "Public one", "", "")
	listingDoc(t, d, seed, "/kb/open/p3", "Public three", "", "")
	alsoName(t, st, "/kb/open/p3", "/kb/closed/p3alias") // unreadable there, so unreadable
	listingDoc(t, d, seed, "/kb/mixed/p2", "Public two", "", "")
	listingDoc(t, d, seed, "/kb/mixed/h1", "Hidden one", "", "")
	listingDoc(t, d, seed, "/kb/closed/h2", "Hidden two", "", "")
	p := &Path{Store: st, Documents: d}
	member := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"document:*", "memory:*", "sqlmem:*"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/closed/**", "document:tenant:/kb/mixed/h1"}}))

	got := lsCall(t, p, member, `{"op":"ls","scope":"tenant","path":"/kb","with_counts":true}`)
	if n := got.count(t, "open"); n != 1 {
		t.Errorf("open = %d, want 1 (p3 is unreadable at its other name)", n)
	}
	if n := got.count(t, "mixed"); n != 1 {
		t.Errorf("mixed = %d, want 1", n)
	}
	if e := got.entry("closed"); e != nil && e["documents"] != float64(0) {
		t.Errorf("closed holds only unreadable documents, counted %v", e)
	}
	if hidden := lsCall(t, p, member, `{"op":"ls","scope":"tenant","path":"/kb","name_contains":"hidden","with_counts":true}`); len(hidden.Entries) != 0 {
		t.Errorf("only unreadable documents match, yet folders surfaced: %v", hidden.Entries)
	}
	pub := lsCall(t, p, member, `{"op":"ls","scope":"tenant","path":"/kb","name_contains":"public","with_counts":true}`)
	if names := pub.names(); strings.Join(names, ",") != "mixed,open" {
		t.Errorf("name_contains=public folders = %v, want mixed,open", names)
	}
	if n := pub.count(t, "open"); n != 1 {
		t.Errorf("public under open = %d, want 1", n)
	}
	// A filter does not probe the metadata of an unreadable document named
	// directly in the listed folder.
	if got := lsCall(t, p, member, `{"op":"ls","scope":"tenant","path":"/kb/mixed","name_contains":"hidden"}`); len(got.Entries) != 0 {
		t.Errorf("an unreadable document matched a filter: %v", got.Entries)
	}
	// p3's name in /kb/open is listed (that name is readable), but the document
	// is not readable at its other name: its title must not match.
	if plain := lsCall(t, p, member, `{"op":"ls","scope":"tenant","path":"/kb/open"}`); plain.entry("p3") == nil {
		t.Fatalf("fixture: p3's readable name is not listed: %v", plain.names())
	}
	if got := lsCall(t, p, member, `{"op":"ls","scope":"tenant","path":"/kb/open","name_contains":"three"}`); len(got.Entries) != 0 {
		t.Errorf("a document unreadable at another name matched a filter by its title: %v", got.Entries)
	}
	// The same tree, read by a caller with no rules, counts everything.
	all := lsCall(t, p, seed, `{"op":"ls","scope":"tenant","path":"/kb","with_counts":true}`)
	if all.count(t, "open") != 2 || all.count(t, "mixed") != 2 || all.count(t, "closed") != 2 {
		t.Errorf("unrestricted counts = %v", all.Entries)
	}
}

// TestPathListing_AnotherTenantsDocumentsNeverCount: tenant isolation holds for
// counts and filters.
func TestPathListing_AnotherTenantsDocumentsNeverCount(t *testing.T) {
	d, base, st := documentFixture(t)
	ctx := tenantDocCtx(base, nil)
	other := tenantDocCtx(tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a", UserID: "u9", TenantID: "other"}), nil)
	listingDoc(t, d, ctx, "/kb/a/mine", "Mine", "", "")
	listingDoc(t, d, other, "/kb/a/theirs", "Theirs", "", "")
	listingDoc(t, d, other, "/kb/theirs-only/t2", "Theirs too", "", "")
	p := &Path{Store: st, Documents: d}

	got := lsCall(t, p, ctx, `{"op":"ls","scope":"tenant","path":"/kb","with_counts":true}`)
	if n := got.count(t, "a"); n != 1 {
		t.Errorf("a = %d, want 1 (the other tenant's document counted)", n)
	}
	if got.entry("theirs-only") != nil {
		t.Errorf("another tenant's folder surfaced: %v", got.names())
	}
	if f := lsCall(t, p, ctx, `{"op":"ls","scope":"tenant","path":"/kb","name_contains":"theirs"}`); len(f.Entries) != 0 {
		t.Errorf("another tenant's document matched a filter: %v", f.Entries)
	}
}

// TestPathListing_FiltersMatchTitleOrNameTypeAndStatus covers each filter alone
// and combined on both tiers, the literal % and _, a folder's own name not
// matching, the echo, and a filtered count.
func TestPathListing_FiltersMatchTitleOrNameTypeAndStatus(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		ctx := tenantDocCtx(base, nil)
		listingDoc(t, d, ctx, "/kb/notes/arch-v2", "Architecture", "Guide", "draft")
		listingDoc(t, d, ctx, "/kb/notes/plan", "Roadmap 100%", "", "done")
		listingDoc(t, d, ctx, "/kb/other/x_y", "Misc", "", "done")
		listingDoc(t, d, ctx, "/kb/deep/a/b/c", "Deep architecture", "Guide", "done")
		p := &Path{Store: d.Store, Documents: d}
		folders := func(args string) lsAnswer {
			t.Helper()
			return lsCall(t, p, ctx, `{"op":"ls","scope":"tenant","path":"/kb",`+args+`}`)
		}
		for args, want := range map[string]string{
			`"name_contains":"ARCHITECTURE"`:           "deep,notes", // title, any case
			`"name_contains":"x_y"`:                    "other",      // a Path name
			`"name_contains":"%"`:                      "notes",      // literal %, not a wildcard
			`"name_contains":"_"`:                      "other",      // literal _
			`"name_contains":"notes"`:                  "",           // a folder's own name is not matched
			`"status":"done"`:                          "deep,notes,other",
			`"status":"draft"`:                         "notes",
			`"status":"Done"`:                          "", // exact
			`"type":"Guide"`:                           "deep,notes",
			`"type":"Guide","status":"done"`:           "deep",
			`"name_contains":"arch","status":"draft"`:  "notes",
			`"name_contains":"roadmap","type":"Guide"`: "",
		} {
			if got := strings.Join(folders(args).names(), ","); got != want {
				t.Errorf("ls /kb {%s} = %q, want %q", args, got, want)
			}
		}
		got := folders(`"name_contains":"arch","status":"done","with_counts":true`)
		if got.Filters["name_contains"] != "arch" || got.Filters["status"] != "done" || len(got.Filters) != 2 {
			t.Errorf("filters echo = %v", got.Filters)
		}
		if n := got.count(t, "deep"); n != 1 {
			t.Errorf("filtered count deep = %d, want 1", n)
		}
		if got.entry("notes") != nil {
			t.Errorf("notes has no done architecture document: %v", got.names())
		}
		// One level down: a document entry is kept when it matches, by its Path
		// name as well as its title, and says so.
		notes := lsCall(t, p, ctx, `{"op":"ls","scope":"tenant","path":"/kb/notes","name_contains":"arch-v"}`)
		if names := notes.names(); len(names) != 1 || names[0] != "arch-v2" || notes.entry("arch-v2")["matched"] != true {
			t.Errorf("ls /kb/notes name_contains=arch-v = %v", notes.Entries)
		}
	})
}

// TestPathListing_DocumentWithChildrenIsKeptForAMatchBelowIt: a document that
// heads a section stays reachable when only something under it matches, and
// is marked matched only when it matches itself.
func TestPathListing_DocumentWithChildrenIsKeptForAMatchBelowIt(t *testing.T) {
	d, base, st := documentFixture(t)
	ctx := tenantDocCtx(base, nil)
	listingDoc(t, d, ctx, "/kb/head", "Head", "", "draft")
	listingDoc(t, d, ctx, "/kb/head/child", "Child", "", "done")
	p := &Path{Store: st, Documents: d}
	got := lsCall(t, p, ctx, `{"op":"ls","scope":"tenant","path":"/kb","status":"done","with_counts":true}`)
	head := got.entry("head")
	if head == nil || head["documents"] != float64(1) || head["matched"] != nil {
		t.Errorf("head kept for its child = %v", head)
	}
	got = lsCall(t, p, ctx, `{"op":"ls","scope":"tenant","path":"/kb","status":"draft","with_counts":true}`)
	if head := got.entry("head"); head == nil || head["matched"] != true || head["documents"] != float64(0) {
		t.Errorf("head matching itself = %v", head)
	}
}

// TestPathListing_TypeFilterIncludesSubtypes: the type filter reads the tenant
// ontology's subtypes, as the Document tool's type filters do, and says so.
func TestPathListing_TypeFilterIncludesSubtypes(t *testing.T) {
	d, ctx := ontologyRetrievalFixture(t, true)
	for _, c := range []struct{ path, typ string }{
		{"/logs/outages/o", "outage"}, {"/logs/incidents/i", "incident"}, {"/logs/people/ada", "person"},
	} {
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"user","title":"t","type":%q,"path":%q}`, c.typ, c.path)); r.IsError {
			t.Fatalf("create_document %s: %s", c.path, r.Text)
		}
	}
	p := &Path{Store: d.Store, Documents: d}
	got := lsCall(t, p, ctx, `{"op":"ls","scope":"user","path":"/logs","type":"event","with_counts":true}`)
	if names := strings.Join(got.names(), ","); names != "incidents,outages" {
		t.Errorf("type=event folders = %q, want incidents,outages", names)
	}
	if len(got.Expanded) != 3 || got.Expanded[0] != "event" {
		t.Errorf("type_expanded_to = %v", got.Expanded)
	}
	if leaf := lsCall(t, p, ctx, `{"op":"ls","scope":"user","path":"/logs","type":"outage"}`); strings.Join(leaf.names(), ",") != "outages" || leaf.Expanded != nil {
		t.Errorf("type=outage = %v (expanded %v)", leaf.names(), leaf.Expanded)
	}
}

// TestPathListing_FilteredPagesWalkEveryMatchOnce: limit and cursor page a
// filtered listing as they page a plain one.
func TestPathListing_FilteredPagesWalkEveryMatchOnce(t *testing.T) {
	d, base, st := documentFixture(t)
	ctx := tenantDocCtx(base, nil)
	for _, f := range []string{"a", "b", "c", "d", "e"} {
		status := "done"
		if f == "c" {
			status = "draft"
		}
		listingDoc(t, d, ctx, "/kb/"+f+"/doc", "T", "", status)
	}
	p := &Path{Store: st, Documents: d}
	var seen []string
	cursor := ""
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("the cursor did not advance")
		}
		q := `{"op":"ls","scope":"tenant","path":"/kb","status":"done","with_counts":true,"limit":2`
		if cursor != "" {
			q += `,"cursor":"` + cursor + `"`
		}
		got := lsCall(t, p, ctx, q+`}`)
		if len(got.Entries) > 2 {
			t.Fatalf("page of %d entries over limit 2", len(got.Entries))
		}
		seen = append(seen, got.names()...)
		if got.Filters["status"] != "done" {
			t.Errorf("page %d lost the filter echo: %v", page, got.Filters)
		}
		if !got.Truncated {
			break
		}
		cursor = got.NextCursor
	}
	if strings.Join(seen, ",") != "a,b,d,e" {
		t.Errorf("filtered pages = %v, want a,b,d,e once each", seen)
	}
}

// TestPathListing_RowCapDoesNotShortenACount: SQL Memory caps the rows one
// query returns; a count reads past the cap rather than stopping at it.
func TestPathListing_RowCapDoesNotShortenACount(t *testing.T) {
	d, base := docFixtureWithRowCap(t, 2)
	ctx := tenantDocCtx(base, nil)
	for i := range 5 {
		listingDoc(t, d, ctx, fmt.Sprintf("/kb/many/d%d", i), "Doc", "", "done")
	}
	p := &Path{Store: d.Store, Documents: d}
	got := lsCall(t, p, ctx, `{"op":"ls","scope":"tenant","path":"/kb","with_counts":true,"status":"done"}`)
	if n := got.count(t, "many"); n != 5 {
		t.Errorf("many = %d under a row cap of 2, want 5", n)
	}
}

// TestPathListing_WithoutTheNewArgumentsIsByteIdentical: wiring the count
// source changes nothing for a listing that does not ask for it.
func TestPathListing_WithoutTheNewArgumentsIsByteIdentical(t *testing.T) {
	d, base, st := documentFixture(t)
	ctx := tenantDocCtx(base, nil)
	listingDoc(t, d, ctx, "/kb/a/x", "X", "Guide", "done")
	listingDoc(t, d, ctx, "/kb/b", "B", "", "")
	listingDoc(t, d, ctx, "/kb/b/c", "C", "", "")
	plain := &Path{Store: st}
	wired := &Path{Store: st, Documents: d}
	for _, q := range []string{
		`{"op":"ls","scope":"tenant","path":"/kb"}`,
		`{"op":"ls","scope":"tenant","path":"/kb","with_counts":false}`,
		`{"op":"ls","scope":"tenant","path":"/kb","limit":1}`,
		`{"op":"ls","scope":"tenant","path":"/kb","recursive":true}`,
		`{"op":"ls","scope":"tenant","path":"/kb","kind_filter":"directory"}`,
	} {
		a, b := pathACLExec(t, plain, ctx, q), pathACLExec(t, wired, ctx, q)
		if a.Text != b.Text || a.IsError || strings.Contains(b.Text, `"documents"`) {
			t.Errorf("%s changed:\n%s\n%s", q, a.Text, b.Text)
		}
	}
	// The shape today, pinned.
	got := pathACLExec(t, wired, ctx, `{"op":"ls","scope":"tenant","path":"/kb","kind_filter":"directory"}`).Text
	if want := `{"entries":[{"name":"a","kind":"directory","full_path":"/kb/a"}],"path":"/kb"}`; got != want {
		t.Errorf("ls = %s\nwant %s", got, want)
	}
}

// TestPathListing_CountsAbsentWhereNotComputable: with no queryable document
// metadata the count is absent (never 0) and a filter is refused.
func TestPathListing_CountsAbsentWhereNotComputable(t *testing.T) {
	d, base, st := documentFixture(t)
	ctx := tenantDocCtx(base, nil)
	listingDoc(t, d, ctx, "/kb/a/x", "X", "", "")
	for name, p := range map[string]*Path{
		"no source":     {Store: st},
		"no SQL Memory": {Store: st, Documents: &Document{Store: st}},
	} {
		got := lsCall(t, p, ctx, `{"op":"ls","scope":"tenant","path":"/kb","with_counts":true}`)
		if _, ok := got.entry("a")["documents"]; ok || len(got.Entries) != 1 {
			t.Errorf("%s: a count where none can be computed: %v", name, got.Entries)
		}
		r := pathACLExec(t, p, ctx, `{"op":"ls","scope":"tenant","path":"/kb","status":"done"}`)
		if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryBusiness {
			t.Errorf("%s: a filter with no metadata = %+v, want a business refusal", name, r)
		}
	}
}

// TestPathListing_RefusesWhatItDoesNotTake: the counted listing is one level
// deep, and a filter value is bounded.
func TestPathListing_RefusesWhatItDoesNotTake(t *testing.T) {
	p, ctx, _ := pathFixture(t)
	for _, q := range []string{
		`{"op":"ls","path":"/","recursive":true,"with_counts":true}`,
		`{"op":"ls","path":"/","recursive":true,"status":"done"}`,
		`{"op":"ls","path":"/","name_contains":"` + strings.Repeat("x", 257) + `"}`,
	} {
		if _, r := pathExec(t, p, ctx, q); !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryValidation {
			t.Errorf("%.80s = %+v, want a validation refusal", q, r)
		}
	}
}

// --- a synthetic tree, for the cap and the work budget ---

// syntheticTree serves DirentListUnder from rows in memory, so a folder of ten
// thousand documents costs no inserts.
type syntheticTree struct {
	store.Store
	rows []store.DirentRow
}

func (s *syntheticTree) DirentListUnder(_ context.Context, tenantID, scope, scopeID, prefix string) ([]store.DirentRow, error) {
	var out []store.DirentRow
	for _, r := range s.rows {
		if r.TenantID == tenantID && r.Scope == scope && r.ScopeID == scopeID && strings.HasPrefix(r.ParentPath, prefix) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *syntheticTree) add(folder string, ids ...string) {
	for _, id := range ids {
		ref, _ := json.Marshal(map[string]string{"document_id": id})
		s.rows = append(s.rows, store.DirentRow{TenantID: "tnt", Scope: "agent", ScopeID: "agent-1",
			ParentPath: "/" + folder + "/", Name: id, Kind: "document", ResourceRef: ref})
	}
}

// fakeDocs answers for every id as a document titled by its id, hiding the
// ids in hidden, and counts the metadata it is asked for.
type fakeDocs struct {
	hidden  map[string]bool
	fetched int
}

func (f *fakeDocs) PathDocuments(context.Context, string) (PathDocumentLookup, bool) { return f, true }

func (f *fakeDocs) Readable(_ context.Context, ids []string) ([]string, error) {
	var out []string
	for _, id := range ids {
		if !f.hidden[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fakeDocs) Facts(_ context.Context, ids []string) (map[string]PathDocumentFact, error) {
	out := map[string]PathDocumentFact{}
	for _, id := range ids {
		if f.hidden[id] {
			panic("metadata asked of a document the caller may not read: " + id)
		}
		out[id] = PathDocumentFact{Title: id, Status: "done"}
	}
	f.fetched += len(ids)
	return out, nil
}

func (f *fakeDocs) ExpandType(_ context.Context, typ string) []string { return []string{typ} }

func idsN(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%05d", prefix, i)
	}
	return out
}

// TestPathListing_CountIsExactToTenThousandThenCapped is RFC HA decision 1.
func TestPathListing_CountIsExactToTenThousandThenCapped(t *testing.T) {
	p, ctx, s := pathFixture(t)
	tree := &syntheticTree{Store: s}
	tree.add("exact", idsN("e", lsCountCap)...)
	tree.add("over", idsN("o", lsCountCap+1)...)
	p.Store, p.Documents = tree, &fakeDocs{}
	got := lsCall(t, p, ctx, `{"op":"ls","path":"/","with_counts":true}`)
	if e := got.entry("exact"); e["documents"] != float64(lsCountCap) || e["documents_capped"] != nil {
		t.Errorf("a folder of exactly the cap = %v, want 10000 uncapped", e)
	}
	if e := got.entry("over"); e["documents"] != float64(lsCountCap) || e["documents_capped"] != true {
		t.Errorf("a folder past the cap = %v, want 10000 capped", e)
	}
}

// walkPages follows next_cursor to the end and returns each page's entry
// names and every entry with its count.
func walkPages(t *testing.T, p *Path, ctx context.Context, args string) (pages [][]string, counts map[string]int) {
	t.Helper()
	counts = map[string]int{}
	cursor := ""
	for i := 0; ; i++ {
		if i > 200 {
			t.Fatal("the cursor did not advance")
		}
		q := `{"op":"ls","path":"/",` + args
		if cursor != "" {
			q += `,"cursor":"` + cursor + `"`
		}
		got := lsCall(t, p, ctx, q+`}`)
		pages = append(pages, got.names())
		for _, e := range got.Entries {
			name := e["name"].(string)
			if _, dup := counts[name]; dup {
				t.Fatalf("%s listed twice", name)
			}
			counts[name] = -1
			if n, ok := e["documents"].(float64); ok {
				counts[name] = int(n)
			}
		}
		if !got.Truncated {
			return pages, counts
		}
		cursor = got.NextCursor
	}
}

// TestPathListing_WorkBudgetTruncatesAndResumesInsideAFolder: a page stops
// when its budget of metadata reads runs out — also part way through one
// folder — reports truncated, and the cursor resumes there with nothing lost
// or counted twice.
func TestPathListing_WorkBudgetTruncatesAndResumesInsideAFolder(t *testing.T) {
	p, ctx, s := pathFixture(t)
	tree := &syntheticTree{Store: s}
	tree.add("f1", idsN("a", 2)...)
	tree.add("f2", idsN("b", 7)...)
	tree.add("f3", idsN("c", 1)...)
	docs := &fakeDocs{}
	p.Store, p.Documents, p.listingBudget = tree, docs, 3
	pages, counts := walkPages(t, p, ctx, `"with_counts":true`)
	if counts["f1"] != 2 || counts["f2"] != 7 || counts["f3"] != 1 || len(counts) != 3 {
		t.Errorf("counts across pages = %v", counts)
	}
	if len(pages) < 3 {
		t.Errorf("a budget of 3 over 10 documents fit in %d pages: %v", len(pages), pages)
	}
	if docs.fetched != 10 {
		t.Errorf("metadata read %d times for 10 documents: a resumed page re-read some", docs.fetched)
	}
	// A filter that matches only late in a large folder still finds it.
	_, counts = walkPages(t, p, ctx, `"name_contains":"b00006"`)
	if len(counts) != 1 || counts["f2"] != -1 {
		t.Errorf("filtered walk = %v, want f2 alone", counts)
	}
}

// TestPathListing_UnreadableDocumentsDoNotShapeThePages: the budget counts
// only documents the caller may read, so the pages a reader is given are the
// same whether or not documents it cannot read lie among them. Were it spent
// on hidden documents, where a page stops would count them.
func TestPathListing_UnreadableDocumentsDoNotShapeThePages(t *testing.T) {
	walk := func(withHidden bool) [][]string {
		p, ctx, s := pathFixture(t)
		tree := &syntheticTree{Store: s}
		docs := &fakeDocs{hidden: map[string]bool{}}
		for i, f := range []string{"f1", "f2", "f3", "f4"} {
			tree.add(f, idsN(f+"v", 2+i)...)
			if withHidden {
				hidden := idsN(f+"h", 5)
				tree.add(f, hidden...)
				for _, h := range hidden {
					docs.hidden[h] = true
				}
			}
		}
		p.Store, p.Documents, p.listingBudget = tree, docs, 4
		pages, _ := walkPages(t, p, ctx, `"with_counts":true`)
		return pages
	}
	a, b := walk(false), walk(true)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("hidden documents moved the page boundaries:\n%v\n%v", a, b)
	}
}

// TestPathListing_CursorFormIsInterchangeable: a part-way cursor handed to a
// plain listing resumes just after the last entry decided, and a plain cursor
// works for a counted one.
func TestPathListing_CursorFormIsInterchangeable(t *testing.T) {
	c := encodeListingCursor(listingCursor{After: "f1", Partial: &listingPartial{Entry: "f2", After: "b00002", N: 3}})
	back, err := decodeListingCursor(c)
	if err != nil || back.After != "f1" || back.Partial == nil || back.Partial.N != 3 || back.Partial.Entry != "f2" {
		t.Fatalf("round trip = %+v %v", back, err)
	}
	raw, _ := decodeLsCursor(c)
	names := []string{"f1", "f2", "f3"}
	if i := sort.SearchStrings(names, raw); names[i] != "f2" {
		t.Errorf("a plain ls would resume at %s, want f2", names[i])
	}
	if plain, err := decodeListingCursor(encodeLsCursor("f1")); err != nil || plain.After != "f1" || plain.Partial != nil {
		t.Errorf("a plain cursor = %+v %v", plain, err)
	}
	if _, err := decodeListingCursor(encodeLsCursor("f1\x00not json")); err == nil {
		t.Error("a forged part-way cursor was taken")
	}
}
