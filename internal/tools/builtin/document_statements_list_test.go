package builtin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// list_statements selection: the filters a maintainer narrows the queue with, the
// two orders and their cursors, and the access-control rule that a filter never
// shows a caller more than the unfiltered listing would.

// listStmts runs a list_statements call and returns the statement ids in order.
func listStmts(t *testing.T, d *Document, ctx context.Context, body string) ([]string, map[string]any) {
	t.Helper()
	out, r := docExec(t, d, ctx, body)
	if r.IsError {
		t.Fatalf("%s: %s", body, r.Text)
	}
	raw, _ := out["statements"].([]any)
	ids := make([]string, 0, len(raw))
	for _, e := range raw {
		ids = append(ids, e.(map[string]any)["id"].(string))
	}
	return ids, out
}

// linkStmt asserts a relation in scope and returns its statement id. evidence is
// a raw JSON evidence array, or "".
func linkStmt(t *testing.T, d *Document, ctx context.Context, scope, from, to, kind, evidence string) string {
	t.Helper()
	body := fmt.Sprintf(`{"op":"link_chunks","scope":%q,"from_id":%q,"to_id":%q,"kind":%q`, scope, from, to, kind)
	if evidence != "" {
		body += `,"evidence":` + evidence
	}
	out, r := docExec(t, d, ctx, body+"}")
	if r.IsError {
		t.Fatalf("link %s: %s", kind, r.Text)
	}
	return out["id"].(string)
}

// stmtSQL writes a statement column the ops do not set, straight into the
// scope's SQL Memory.
func stmtSQL(t *testing.T, d *Document, ctx context.Context, scope, q string, args ...any) {
	t.Helper()
	key, _, err := d.resolveScope(ctx, scope)
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	if err := d.exec(ctx, key, q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// pathDoc makes a document named at path with one chunk carrying body.
func pathDoc(t *testing.T, d *Document, ctx context.Context, path, title, body string) (docID, chunkID string) {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"user","title":%q,"path":%q}`, title, path))
	if r.IsError {
		t.Fatalf("create_document %s: %s", path, r.Text)
	}
	docID = out["document_id"].(string)
	out, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":%q,"body":%q}`, docID, title+" part", body))
	if r.IsError {
		t.Fatalf("create_chunk %s: %s", path, r.Text)
	}
	return docID, out["id"].(string)
}

func sameIDs(got []string, want ...string) bool {
	g := append([]string{}, got...)
	w := append([]string{}, want...)
	sort.Strings(g)
	sort.Strings(w)
	return strings.Join(g, ",") == strings.Join(w, ",")
}

// document_id keeps a statement when EITHER end is a chunk of the document, and
// every row carries auto.
func TestListStatements_DocumentIDKeepsEitherEnd(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docA, a1, a2, _ := stmtDoc(t, d, ctx, "doc a")
		_, c1, c2, _ := stmtDoc(t, d, ctx, "doc c")
		out := linkStmt(t, d, ctx, "user", a1, c1, "cites", "")
		in := linkStmt(t, d, ctx, "user", c1, a2, "implements", "")
		linkStmt(t, d, ctx, "user", c1, c2, "defines", "")
		ids, page := listStmts(t, d, ctx, fmt.Sprintf(`{"op":"list_statements","scope":"user","status":"asserted","document_id":%q}`, docA))
		if !sameIDs(ids, out, in) {
			t.Fatalf("document_id: got %v, want [%s %s]", ids, out, in)
		}
		for _, e := range page["statements"].([]any) {
			if _, ok := e.(map[string]any)["auto"]; !ok {
				t.Errorf("a row without auto: %v", e)
			}
		}
		if all, _ := listStmts(t, d, ctx, `{"op":"list_statements","scope":"user","status":"asserted"}`); len(all) != 3 {
			t.Fatalf("without the filter: %v", all)
		}
	})
}

// evidence_document_id keeps a statement that has an evidence row citing a chunk
// of the document, whatever its ends.
func TestListStatements_EvidenceDocumentIDKeepsCitingStatements(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docX, x, _, _ := stmtDoc(t, d, ctx, "source x")
		_, y, _, _ := stmtDoc(t, d, ctx, "source y")
		_, p, q, _ := stmtDoc(t, d, ctx, "ends")
		citesX := linkStmt(t, d, ctx, "user", p, q, "cites", fmt.Sprintf(`[{"chunk_id":%q,"quote":"Article 12 of the Act"}]`, x))
		linkStmt(t, d, ctx, "user", p, q, "implements", fmt.Sprintf(`[{"chunk_id":%q,"quote":"Article 12 of the Act"}]`, y))
		linkStmt(t, d, ctx, "user", x, q, "defines", "")
		ids, _ := listStmts(t, d, ctx, fmt.Sprintf(`{"op":"list_statements","scope":"user","status":"asserted","evidence_document_id":%q}`, docX))
		if !sameIDs(ids, citesX) {
			t.Fatalf("evidence_document_id: got %v, want [%s]", ids, citesX)
		}
	})
}

// method_prefix is an exact, case-sensitive prefix: LIKE's wildcards in it are
// plain characters.
func TestListStatements_MethodPrefixIsAnExactPrefix(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, a, b, c := stmtDoc(t, d, ctx, "methods")
		ids := map[string]string{}
		for i, m := range []string{"rule:a_b", "rule:aXb", "Rule:a_b", "extractor:x", "manual", "rule%"} {
			kind := []string{"cites", "implements", "defines", "based_on", "related", "cites"}[i]
			from, to := a, b
			if i == 5 {
				from, to = b, c
			}
			id := linkStmt(t, d, ctx, "user", from, to, kind, "")
			stmtSQL(t, d, ctx, "user", `UPDATE chunk_edges SET method = ? WHERE id = ?`, m, id)
			ids[m] = id
		}
		for prefix, want := range map[string][]string{
			"rule:a_": {ids["rule:a_b"]},
			"rule:":   {ids["rule:a_b"], ids["rule:aXb"]},
			"rule%":   {ids["rule%"]},
			"%":       {},
			"extract": {ids["extractor:x"]},
		} {
			got, _ := listStmts(t, d, ctx, fmt.Sprintf(`{"op":"list_statements","scope":"user","status":"asserted","method_prefix":%q}`, prefix))
			if !sameIDs(got, want...) {
				t.Errorf("method_prefix %q: got %v, want %v", prefix, got, want)
			}
		}
	})
}

// under_path keeps a statement when either end's document is named at or under
// the path.
func TestListStatements_UnderPathKeepsEitherEndsDocument(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, in1 := pathDoc(t, d, ctx, "/kb/in/one", "One", "inside")
		_, out1 := pathDoc(t, d, ctx, "/kb/out/two", "Two", "outside")
		_, out2 := pathDoc(t, d, ctx, "/kb/out/three", "Three", "outside too")
		fromIn := linkStmt(t, d, ctx, "user", in1, out1, "cites", "")
		toIn := linkStmt(t, d, ctx, "user", out1, in1, "cites", "")
		linkStmt(t, d, ctx, "user", out1, out2, "cites", "")
		for _, p := range []string{"/kb/in", "/kb/in/one"} {
			got, _ := listStmts(t, d, ctx, fmt.Sprintf(`{"op":"list_statements","scope":"user","status":"asserted","under_path":%q}`, p))
			if !sameIDs(got, fromIn, toIn) {
				t.Errorf("under_path %s: got %v, want [%s %s]", p, got, fromIn, toIn)
			}
		}
		if got, _ := listStmts(t, d, ctx, `{"op":"list_statements","scope":"user","status":"asserted","under_path":"/kb/none"}`); len(got) != 0 {
			t.Errorf("under_path naming nothing: %v", got)
		}
		if _, r := docExec(t, d, ctx, `{"op":"list_statements","scope":"user","under_path":"relative"}`); !r.IsError {
			t.Errorf("a malformed under_path was accepted")
		}
	})
}

// A retired statement — expired, or already ended — is left out unless
// include_retired asks for it. One dated to take effect later is not retired: a
// proposal for a coming edition still belongs in the review queue.
func TestListStatements_IncludeRetiredReturnsExpiredAndOutOfForce(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, a, b, c := stmtDoc(t, d, ctx, "retired")
		now := time.Now().UnixNano()
		live := linkStmt(t, d, ctx, "user", a, b, "cites", "")
		expired := linkStmt(t, d, ctx, "user", a, b, "implements", "")
		ended := linkStmt(t, d, ctx, "user", a, b, "defines", "")
		future := linkStmt(t, d, ctx, "user", a, b, "based_on", "")
		stmtSQL(t, d, ctx, "user", `UPDATE chunk_edges SET expired_at = ? WHERE id = ?`, now-1000, expired)
		stmtSQL(t, d, ctx, "user", `UPDATE chunk_edges SET invalid_at = ? WHERE id = ?`, now-1000, ended)
		stmtSQL(t, d, ctx, "user", `UPDATE chunk_edges SET valid_at = ? WHERE id = ?`, now+int64(time.Hour), future)
		if got, _ := listStmts(t, d, ctx, `{"op":"list_statements","scope":"user","status":"asserted"}`); !sameIDs(got, live, future) {
			t.Errorf("default: got %v, want [%s %s]", got, live, future)
		}
		if got, _ := listStmts(t, d, ctx, `{"op":"list_statements","scope":"user","status":"asserted","include_retired":true}`); !sameIDs(got, live, expired, ended, future) {
			t.Errorf("include_retired: got %v", got)
		}
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","valid_at":%d}`, b, c, now+int64(time.Hour)))
		if r.IsError {
			t.Fatalf("propose: %s", r.Text)
		}
		if got, _ := listStmts(t, d, ctx, `{"op":"list_statements","scope":"user"}`); !sameIDs(got, out["id"].(string)) {
			t.Errorf("a proposal taking effect later is missing from the queue: got %v, want [%s]", got, out["id"])
		}
	})
}

// pageAll walks a listing one page at a time and returns the ids in order. The
// last page carries neither truncated nor next_cursor.
func pageAll(t *testing.T, d *Document, ctx context.Context, body string) []string {
	t.Helper()
	var all []string
	after := ""
	for i := 0; i < 20; i++ {
		b := body
		if after != "" {
			b = strings.TrimSuffix(body, "}") + fmt.Sprintf(`,"after":%q}`, after)
		}
		ids, out := listStmts(t, d, ctx, b)
		all = append(all, ids...)
		next, ok := out["next_cursor"].(string)
		if !ok {
			if _, tr := out["truncated"]; tr {
				t.Fatalf("truncated without a cursor: %v", out)
			}
			return all
		}
		if len(ids) == 0 {
			t.Fatalf("an empty page carries a cursor: %v", out)
		}
		after = next
	}
	t.Fatalf("paging did not end: %v", all)
	return nil
}

// orderFixture writes four statements with fixed created_at and verified_at:
// s1 and s2 never verified, s3 verified late, s4 early. Returns them by name.
func orderFixture(t *testing.T, d *Document, ctx context.Context) map[string]string {
	t.Helper()
	_, a, b, _ := stmtDoc(t, d, ctx, "order")
	s := map[string]string{}
	for i, kind := range []string{"cites", "implements", "defines", "based_on"} {
		s[fmt.Sprintf("s%d", i+1)] = linkStmt(t, d, ctx, "user", a, b, kind, "")
	}
	for name, v := range map[string][2]any{
		"s1": {int64(10), nil}, "s2": {int64(40), nil}, "s3": {int64(20), int64(300)}, "s4": {int64(30), int64(100)},
	} {
		stmtSQL(t, d, ctx, "user", `UPDATE chunk_edges SET created_at = ?, verified_at = ? WHERE id = ?`, v[0], v[1], s[name])
	}
	return s
}

// created (the default) is newest first; it pages across a boundary to the end.
func TestListStatements_CreatedOrderPagesNewestFirst(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		s := orderFixture(t, d, ctx)
		want := []string{s["s2"], s["s4"], s["s3"], s["s1"]}
		for _, body := range []string{
			`{"op":"list_statements","scope":"user","status":"asserted","limit":3}`,
			`{"op":"list_statements","scope":"user","status":"asserted","limit":1,"order":"created"}`,
		} {
			if got := pageAll(t, d, ctx, body); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s: got %v, want %v", body, got, want)
			}
		}
	})
}

// verified is least recently verified first, never-verified (NULL) before any,
// ties broken by id; it pages across the NULL group and out of it.
func TestListStatements_VerifiedOrderPagesNullFirst(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		s := orderFixture(t, d, ctx)
		nulls := []string{s["s1"], s["s2"]}
		sort.Strings(nulls)
		want := append(nulls, s["s4"], s["s3"])
		for _, limit := range []int{1, 3} {
			body := fmt.Sprintf(`{"op":"list_statements","scope":"user","status":"asserted","order":"verified","limit":%d}`, limit)
			if got := pageAll(t, d, ctx, body); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("limit %d: got %v, want %v", limit, got, want)
			}
		}
	})
}

// A cursor pages only the order that issued it; anything else is refused rather
// than read as a position in another sort.
func TestListStatements_CursorOfOneOrderIsRefusedUnderTheOther(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		orderFixture(t, d, ctx)
		cursorOf := func(order string) string {
			_, out := listStmts(t, d, ctx, fmt.Sprintf(`{"op":"list_statements","scope":"user","status":"asserted","limit":1,"order":%q}`, order))
			c, _ := out["next_cursor"].(string)
			if c == "" {
				t.Fatalf("%s: no cursor: %v", order, out)
			}
			return c
		}
		created, verified := cursorOf("created"), cursorOf("verified")
		for _, tc := range []struct{ order, after string }{
			{"verified", created}, {"created", verified}, {"", verified},
			{"created", "not-a-cursor"},
			{"created", encodeEdgeCursor(edgeCursor{Kind: "cites", CreatedAt: 1, ID: "x"})},
		} {
			_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"list_statements","scope":"user","status":"asserted","order":%q,"after":%q}`, tc.order, tc.after))
			if !r.IsError || !strings.Contains(r.Text, "after") {
				t.Errorf("order %q after %q: want a refusal naming after, got %s", tc.order, tc.after, r.Text)
			}
		}
		if _, r := docExec(t, d, ctx, `{"op":"list_statements","scope":"user","order":"newest"}`); !r.IsError {
			t.Errorf("an unknown order was accepted")
		}
	})
}

// Filters intersect.
func TestListStatements_CombinedFiltersIntersect(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docA, a1, a2, _ := stmtDoc(t, d, ctx, "combined a")
		docX, x, _, _ := stmtDoc(t, d, ctx, "combined x")
		_, c1, _, _ := stmtDoc(t, d, ctx, "combined c")
		ev := fmt.Sprintf(`[{"chunk_id":%q,"quote":"Article 12 of the Act"}]`, x)
		hit := linkStmt(t, d, ctx, "user", a1, c1, "cites", ev)
		linkStmt(t, d, ctx, "user", a2, c1, "cites", ev) // keeps method manual
		noEvidence := linkStmt(t, d, ctx, "user", c1, a1, "cites", "")
		wrongKind := linkStmt(t, d, ctx, "user", a1, c1, "implements", ev)
		otherDoc := linkStmt(t, d, ctx, "user", c1, c1, "cites", ev)
		for _, id := range []string{hit, noEvidence, wrongKind, otherDoc} {
			stmtSQL(t, d, ctx, "user", `UPDATE chunk_edges SET method = ? WHERE id = ?`, "rule:cite", id)
		}
		got, _ := listStmts(t, d, ctx, fmt.Sprintf(`{"op":"list_statements","scope":"user","status":"asserted",
			"document_id":%q,"evidence_document_id":%q,"method_prefix":"rule:","kind":"cites"}`, docA, docX))
		if !sameIDs(got, hit) {
			t.Fatalf("combined: got %v, want [%s]", got, hit)
		}
	})
}

// A filter never shows a confined reader more than the unfiltered listing does:
// selecting by a document it may not read finds nothing, with no page marker or
// count that would tell it statements there exist.
func TestListStatements_FilterOnAnUnreadableDocumentFindsNothing(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		open := tenantDocCtx(base, nil)
		s := seedACLDocs(t, d, open)
		var ids []string
		for _, kind := range []string{"cites", "implements"} {
			out, r := docExec(t, d, open, `{"op":"link_chunks","scope":"tenant","from_id":"`+s.publicChunk+`","to_id":"`+s.publicChunk+`","kind":"`+kind+`",
				"evidence":[{"chunk_id":"`+s.secretChunk+`","quote":"savepoint nesting secret"}]}`)
			if r.IsError {
				t.Fatal(r.Text)
			}
			ids = append(ids, out["id"].(string))
		}
		// Two relations into the secret document: enough that a page of one would
		// be marked truncated if the filter reached them.
		for _, kind := range []string{"defines", "based_on"} {
			if _, r := docExec(t, d, open, `{"op":"link_chunks","scope":"tenant","from_id":"`+s.publicChunk+`","to_id":"`+s.secretChunk+`","kind":"`+kind+`"}`); r.IsError {
				t.Fatal(r.Text)
			}
		}
		// The filter does select them for a reader who may see the source.
		filter := `{"op":"list_statements","scope":"tenant","status":"asserted","limit":1,"evidence_document_id":"` + s.secretDoc + `"}`
		if got, out := listStmts(t, d, open, filter); len(got) != 1 || out["next_cursor"] == nil {
			t.Fatalf("an unconfined reader: got %v %v", got, out)
		}
		denied := tenantDocCtx(base, aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
		// The first two join two readable ends, so the unfiltered listing shows
		// them; a filter on the secret document must find nothing.
		if got, _ := listStmts(t, d, denied, `{"op":"list_statements","scope":"tenant","status":"asserted"}`); !sameIDs(got, ids...) {
			t.Fatalf("the unfiltered listing: got %v, want %v", got, ids)
		}
		for _, body := range []string{
			filter,
			`{"op":"list_statements","scope":"tenant","status":"asserted","limit":1,"document_id":"` + s.secretDoc + `"}`,
			`{"op":"list_statements","scope":"tenant","status":"asserted","limit":1,"under_path":"/kb/secret"}`,
		} {
			out, r := docExec(t, d, denied, body)
			if r.IsError {
				t.Fatalf("%s: %s", body, r.Text)
			}
			if l, _ := out["statements"].([]any); len(l) != 0 {
				t.Errorf("%s: a confined reader got %v", body, l)
			}
			for _, k := range []string{"truncated", "next_cursor"} {
				if _, ok := out[k]; ok {
					t.Errorf("%s: %s tells a confined reader statements exist: %s", body, k, r.Text)
				}
			}
		}
	})
}

// Rows a confined reader may not see are withheld before the page is cut: a page
// of limit N holds N visible rows when that many exist, and truncated /
// next_cursor follow only visible rows — so a filter cannot tell the reader that
// hidden statements matching it exist past the last one it sees.
func TestListStatements_HiddenRowsNeverShapeThePage(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		open := tenantDocCtx(base, nil)
		s := seedACLDocs(t, d, open)
		link := func(to, kind string, createdAt int64) string {
			out, r := docExec(t, d, open, `{"op":"link_chunks","scope":"tenant","from_id":"`+s.publicChunk+`","to_id":"`+to+`","kind":"`+kind+`"}`)
			if r.IsError {
				t.Fatal(r.Text)
			}
			id := out["id"].(string)
			stmtSQL(t, d, open, "tenant", `UPDATE chunk_edges SET created_at = ?, method = ? WHERE id = ?`, createdAt, "rule:x", id)
			return id
		}
		// Newest first: visible, hidden, visible, hidden, visible, hidden.
		v1 := link(s.publicChunk, "cites", 60)
		link(s.secretChunk, "cites", 50)
		v2 := link(s.publicChunk, "implements", 40)
		link(s.secretChunk, "implements", 30)
		v3 := link(s.publicChunk, "defines", 20)
		link(s.secretChunk, "defines", 10)
		denied := tenantDocCtx(base, aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))

		// Only hidden rows lie past the last visible one: no page marker.
		_, out := listStmts(t, d, denied, `{"op":"list_statements","scope":"tenant","status":"asserted","method_prefix":"rule:","limit":3}`)
		if l, _ := out["statements"].([]any); len(l) != 3 {
			t.Fatalf("limit 3: want 3 visible rows, got %v", out)
		}
		for _, k := range []string{"truncated", "next_cursor"} {
			if _, ok := out[k]; ok {
				t.Errorf("limit 3: %s set though only hidden rows remain: %v", k, out)
			}
		}
		// Full pages of visible rows, in order, across the hidden ones.
		after := ""
		var pages [][]string
		for i := 0; i < 5; i++ {
			body := `{"op":"list_statements","scope":"tenant","status":"asserted","method_prefix":"rule:","limit":2`
			if after != "" {
				body += `,"after":"` + after + `"`
			}
			ids, out := listStmts(t, d, denied, body+"}")
			pages = append(pages, ids)
			next, ok := out["next_cursor"].(string)
			if !ok {
				break
			}
			after = next
		}
		if len(pages) != 2 || strings.Join(pages[0], ",") != v1+","+v2 || strings.Join(pages[1], ",") != v3 {
			t.Errorf("limit 2 pages: got %v, want [[%s %s] [%s]]", pages, v1, v2, v3)
		}
	})
}
