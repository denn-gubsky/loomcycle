package builtin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// interleavedDocs files six documents at /kb/d1 … /kb/d6 titled "Doc 1" …
// "Doc 6", so that both the Path tree's order and query_documents' (title, id)
// order run d1, d2, …, d6. It returns the odd ones (visible to hiddenEvenDocs'
// reader) and the even ones (hidden from it).
func interleavedDocs(t *testing.T, d *Document, open context.Context) (visible, hidden []string) {
	t.Helper()
	for i := 1; i <= 6; i++ {
		out, r := docExec(t, d, open, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":"Doc %d","path":"/kb/d%d"}`, i, i))
		if r.IsError {
			t.Fatalf("create_document d%d: %s", i, r.Text)
		}
		if i%2 == 1 {
			visible = append(visible, out["document_id"].(string))
		} else {
			hidden = append(hidden, out["document_id"].(string))
		}
	}
	return visible, hidden
}

// hiddenEvenDocs is a reader denied the even documents of interleavedDocs.
func hiddenEvenDocs(t *testing.T, base context.Context) context.Context {
	return tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/d2", "document:tenant:/kb/d4", "document:tenant:/kb/d6"}}))
}

// listingIDs runs body and returns the document ids of its "documents" array
// and the raw answer.
func listingIDs(t *testing.T, d *Document, ctx context.Context, body string) ([]string, string, map[string]any) {
	t.Helper()
	out, r := docExec(t, d, ctx, body)
	if r.IsError {
		t.Fatalf("%s: %s", body, r.Text)
	}
	raw, _ := out["documents"].([]any)
	ids := make([]string, 0, len(raw))
	for _, e := range raw {
		ids = append(ids, e.(map[string]any)["document_id"].(string))
	}
	return ids, r.Text, out
}

// deleteDocs removes ids, their Path-tree names with them.
func deleteDocs(t *testing.T, d *Document, open context.Context, ids []string) {
	t.Helper()
	for _, id := range ids {
		if _, r := docExec(t, d, open, `{"op":"delete_document","scope":"tenant","id":"`+id+`"}`); r.IsError {
			t.Fatalf("delete_document %s: %s", id, r.Text)
		}
	}
}

// Documents a confined reader may not read are withheld before query_documents
// cuts its answer at limit: the reader gets limit readable documents when that
// many exist, and the same bytes it would get if the hidden ones did not exist.
func TestQueryDocuments_HiddenDocumentsNeverShortenTheAnswer(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		open := tenantDocCtx(base, nil)
		visible, hidden := interleavedDocs(t, d, open)
		denied := hiddenEvenDocs(t, base)

		var bodies []string
		for _, under := range []string{"", `,"under_path":"/kb"`} {
			for _, limit := range []int{1, 2, 3, 4} {
				bodies = append(bodies, fmt.Sprintf(`{"op":"query_documents","scope":"tenant","limit":%d%s}`, limit, under))
			}
		}
		withHidden := map[string]string{}
		for _, body := range bodies {
			ids, text, _ := listingIDs(t, d, denied, body)
			withHidden[body] = text
			var limit int
			_, _ = fmt.Sscanf(body[strings.Index(body, `"limit":`)+len(`"limit":`):], "%d", &limit)
			want := visible[:min(limit, len(visible))]
			if strings.Join(ids, ",") != strings.Join(want, ",") {
				t.Errorf("%s: documents %v, want the first readable %v", body, ids, want)
			}
		}

		deleteDocs(t, d, open, hidden)
		for _, body := range bodies {
			_, text, _ := listingIDs(t, d, denied, body)
			if text != withHidden[body] {
				t.Errorf("%s: hidden documents change the answer:\nwith    %s\nwithout %s", body, withHidden[body], text)
			}
		}
	})
}

// More hidden documents than query_documents reads at once: it reads on from
// the last row it scanned, on the same title by id and then past the title,
// until the answer is full.
func TestQueryDocuments_ReadsPastABatchOfHiddenDocuments(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		open := tenantDocCtx(base, nil)
		pathOf := map[string]string{}
		var same []string
		for i := 0; i < 103; i++ {
			path := fmt.Sprintf("/kb/n%03d", i)
			out, r := docExec(t, d, open, `{"op":"create_document","scope":"tenant","title":"Same","path":"`+path+`"}`)
			if r.IsError {
				t.Fatalf("create_document %s: %s", path, r.Text)
			}
			id := out["document_id"].(string)
			pathOf[id] = path
			same = append(same, id)
		}
		out, r := docExec(t, d, open, `{"op":"create_document","scope":"tenant","title":"Zed","path":"/kb/zed"}`)
		if r.IsError {
			t.Fatalf("create_document /kb/zed: %s", r.Text)
		}
		zed := out["document_id"].(string)
		// In (title, id) order the first 101 are hidden: a whole first batch and
		// one row past it.
		sort.Strings(same)
		var denied []string
		for _, id := range same[:101] {
			denied = append(denied, "document:tenant:"+pathOf[id])
		}
		// A rule names at most 64 resources.
		reader := tenantDocCtx(base, aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: denied[:50]},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: denied[50:]}))
		visible := []string{same[101], same[102], zed}
		for _, limit := range []int{1, 2, 3, 4} {
			body := fmt.Sprintf(`{"op":"query_documents","scope":"tenant","limit":%d}`, limit)
			ids, _, _ := listingIDs(t, d, reader, body)
			if want := visible[:min(limit, len(visible))]; strings.Join(ids, ",") != strings.Join(want, ",") {
				t.Errorf("%s: documents %v, want %v", body, ids, want)
			}
		}
	})
}

// documents_summary bounds its answer at limit; a document a confined reader may
// not read neither spends that bound nor sets truncated. Named by under_path it
// is as absent as if it did not exist; named by document_ids it answers as an
// unknown id does.
func TestDocumentsSummary_HiddenDocumentsNeverSpendTheBound(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		open := tenantDocCtx(base, nil)
		visible, hidden := interleavedDocs(t, d, open)
		denied := hiddenEvenDocs(t, base)

		var bodies []string
		for _, limit := range []int{1, 2, 3, 4} {
			bodies = append(bodies, fmt.Sprintf(`{"op":"documents_summary","scope":"tenant","under_path":"/kb","limit":%d}`, limit))
		}
		named := fmt.Sprintf(`{"op":"documents_summary","scope":"tenant","document_ids":[%q,%q,%q],"limit":2}`, hidden[0], visible[0], visible[1])
		bodies = append(bodies, named)

		withHidden := map[string]string{}
		for _, body := range bodies {
			ids, text, out := listingIDs(t, d, denied, body)
			withHidden[body] = text
			if body == named {
				continue
			}
			var limit int
			_, _ = fmt.Sscanf(body[strings.Index(body, `"limit":`)+len(`"limit":`):], "%d", &limit)
			want := visible[:min(limit, len(visible))]
			if strings.Join(ids, ",") != strings.Join(want, ",") {
				t.Errorf("%s: documents %v, want the first readable %v", body, ids, want)
			}
			_, truncated := out["truncated"]
			if truncated != (limit < len(visible)) {
				t.Errorf("%s: truncated=%v, want it exactly when readable documents remain: %s", body, truncated, text)
			}
		}

		deleteDocs(t, d, open, hidden)
		for _, body := range bodies {
			_, text, _ := listingIDs(t, d, denied, body)
			if text != withHidden[body] {
				t.Errorf("%s: hidden documents change the answer:\nwith    %s\nwithout %s", body, withHidden[body], text)
			}
		}
	})
}

// A caller with no set, or a set that hides nothing, gets both listings as it
// always has: every document, cut at limit, and the same bytes either way.
func TestDocumentListings_UnrestrictedCallerAnswerUnchanged(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		open := tenantDocCtx(base, nil)
		visible, hidden := interleavedDocs(t, d, open)
		allowAll := tenantDocCtx(base, aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}}))
		all := []string{visible[0], hidden[0], visible[1], hidden[1], visible[2], hidden[2]}

		cases := []struct {
			body      string
			want      []string
			truncated bool
		}{
			{`{"op":"query_documents","scope":"tenant","under_path":"/kb","limit":3}`, all[:3], false},
			{`{"op":"query_documents","scope":"tenant","limit":10}`, all, false},
			{`{"op":"documents_summary","scope":"tenant","under_path":"/kb","limit":3}`, all[:3], true},
			{`{"op":"documents_summary","scope":"tenant","under_path":"/kb","limit":6}`, all, false},
			// An unknown id spends the bound: it always has.
			{fmt.Sprintf(`{"op":"documents_summary","scope":"tenant","document_ids":["0123456789abcdef0123456789abcdef",%q,%q],"limit":2}`, all[0], all[1]), all[:1], true},
		}
		for _, c := range cases {
			ids, text, out := listingIDs(t, d, open, c.body)
			if strings.Join(ids, ",") != strings.Join(c.want, ",") {
				t.Errorf("%s: documents %v, want %v", c.body, ids, c.want)
			}
			if _, truncated := out["truncated"]; truncated != c.truncated {
				t.Errorf("%s: truncated=%v, want %v", c.body, truncated, c.truncated)
			}
			_, allowText, _ := listingIDs(t, d, allowAll, c.body)
			if allowText != text {
				t.Errorf("%s: a set that hides nothing changes the answer:\nno set    %s\nallow-all %s", c.body, text, allowText)
			}
		}
	})
}

// The listings that read the whole scope have no page for hidden documents to
// shape: a set that hides any document refuses them outright.
func TestDocumentListings_WholeScopeListingsRefuseAConfinedReader(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	interleavedDocs(t, d, open)
	denied := hiddenEvenDocs(t, base)
	for _, body := range []string{
		`{"op":"query_chunks","scope":"tenant"}`,
		`{"op":"list_facts","scope":"tenant"}`,
		`{"op":"list_tags","scope":"tenant"}`,
	} {
		_, r := docExec(t, d, denied, body)
		refusedByACL(t, body, r)
	}
}
