package builtin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// The annotation leg of Document op=search.

type annSearchCorpus struct {
	widgets, gadgets, pricing string
}

func seedAnnSearch(t *testing.T) (*Document, context.Context, context.Context, annSearchCorpus) {
	t.Helper()
	d, base, _ := documentFixture(t)
	d.Store = &ftsStore{newVectorStore(d.Store)}
	d.Embedder = newFakeEmbedder("fake", "m1", "widget", "gadget", "pricing", "install", "catalogue")
	op := annOperator(base)
	var c annSearchCorpus
	mk := func(title, path, body string) string {
		out, r := docExec(t, d, op, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":%q,"type":"Guide"}`, title, path))
		if r.IsError {
			t.Fatal(r.Text)
		}
		doc := out["document_id"].(string)
		if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"Body","body":%q}`, doc, body)); r.IsError {
			t.Fatal(r.Text)
		}
		return doc
	}
	c.widgets = mk("Widget guide", "/kb/products/widgets", "how to install the widget")
	c.gadgets = mk("Gadget guide", "/kb/products/gadgets", "how to install the gadget")
	c.pricing = mk("Price list", "/kb/products/pricing", "the price of each item")
	write := func(docID, brief, abstract string) {
		in := annInputs(t, d, op, `"document_id":"`+docID+`"`)
		if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","document_id":%q,"brief":%q,"abstract":%q,"inputs_sha256":%q}`,
			docID, brief, abstract, in["inputs_sha256"])); r.IsError {
			t.Fatal(r.Text)
		}
	}
	write(c.widgets, "installing the widget", "steps to install the widget")
	write(c.gadgets, "installing the gadget", "steps to install the gadget")
	write(c.pricing, "pricing for every product", "the pricing of the widget and the gadget")
	folderWrite(t, d, op, "/kb/products", "the product catalogue")
	return d, op, base, c
}

func searchRows(t *testing.T, d *Document, ctx context.Context, q string) []map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, q)
	if r.IsError {
		t.Fatalf("%s: %s", q, r.Text)
	}
	var rows []map[string]any
	for _, e := range out["chunks"].([]any) {
		rows = append(rows, e.(map[string]any))
	}
	return rows
}

func TestSearchAnnotations_OnlyAnswersFromAnnotations(t *testing.T) {
	d, op, _, c := seedAnnSearch(t)
	rows := searchRows(t, d, op, `{"op":"search","scope":"tenant","query":"pricing","annotations":"only"}`)
	if len(rows) == 0 || rows[0]["document_id"] != c.pricing {
		t.Fatalf("rows = %v, want the price list first", rows)
	}
	ma, _ := rows[0]["matched_annotation"].(map[string]any)
	sm, _ := rows[0]["summary"].(map[string]any)
	if ma["level"] != "document" || ma["text"] != "pricing for every product" || sm["abstract"] == nil {
		t.Fatalf("first row = %v", rows[0])
	}
	for _, r := range rows {
		if r["matched_annotation"] == nil {
			t.Fatalf("an only-search returned a passage: %v", r)
		}
	}
	// A corpus question at folder level answers with the folder.
	rows = searchRows(t, d, op, `{"op":"search","scope":"tenant","query":"catalogue","annotations":"only","annotation_levels":["path"]}`)
	if len(rows) != 1 || rows[0]["folder"] != "/kb/products" {
		t.Fatalf("folder rows = %v", rows)
	}
}

func TestSearchAnnotations_IncludeFusesOneRowPerTarget(t *testing.T) {
	d, op, _, c := seedAnnSearch(t)
	rows := searchRows(t, d, op, `{"op":"search","scope":"tenant","query":"install widget","annotations":"include"}`)
	seen := map[string]int{}
	foundPassage, foundAnnotation := false, false
	for _, r := range rows {
		seen[r["chunk_id"].(string)]++
		if r["matched_annotation"] != nil {
			foundAnnotation = true
		} else {
			foundPassage = true
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("chunk %s takes %d slots", id, n)
		}
	}
	if !foundPassage || !foundAnnotation {
		t.Fatalf("include rows = %v, want passages and annotation targets", rows)
	}
	_ = c
}

// Without the option, search is what it was: the annotation rows live apart.
func TestSearchAnnotations_NoneIsUnchanged(t *testing.T) {
	d, base, _ := documentFixture(t)
	d.Store = &ftsStore{newVectorStore(d.Store)}
	d.Embedder = newFakeEmbedder("fake", "m1", "widget", "install")
	op := annOperator(base)
	c := seedAnnCorpus(t, d, op)
	// The same hits with the same scores. Not the same order: a search bumps the
	// access counts its ties are broken by, so a repeated search reorders ties.
	q := `{"op":"search","scope":"tenant","query":"install widget"}`
	hits := func() string {
		rows := searchRows(t, d, op, q)
		var keys []string
		for _, r := range rows {
			if r["summary"] != nil || r["matched_annotation"] != nil {
				t.Fatalf("a search without annotations carries one: %v", r)
			}
			keys = append(keys, fmt.Sprintf("%v=%v", r["chunk_id"], r["score"]))
		}
		sort.Strings(keys)
		return strings.Join(keys, ",")
	}
	before := hits()
	annWrite(t, d, op, idTarget(c.setup), "install the widget")
	annWrite(t, d, op, `"document_id":"`+c.doc+`"`, "widget install guide")
	if after := hits(); before != after {
		t.Fatalf("search changed after annotating:\n%s\n%s", before, after)
	}
	// Memory's own search over the documents' scope does not see them either.
	if _, r := docExec(t, d, op, `{"op":"search","scope":"tenant","query":"x","annotation_levels":["path"]}`); !r.IsError {
		t.Fatal("annotation_levels without annotations was taken")
	}
}

func TestSearchAnnotations_FollowTheRecords(t *testing.T) {
	d, op, base, c := seedAnnSearch(t)
	if _, r := docExec(t, d, op, `{"op":"delete_annotation","scope":"tenant","document_id":"`+c.pricing+`","level":"document"}`); r.IsError {
		t.Fatal(r.Text)
	}
	for _, r := range searchRows(t, d, op, `{"op":"search","scope":"tenant","query":"pricing","annotations":"only","annotation_levels":["document"]}`) {
		if r["document_id"] == c.pricing {
			t.Fatalf("a deleted annotation still found its target: %v", r)
		}
	}
	// A member who may not read the price list is not shown the folder that
	// covers it, nor the price list, through the leg.
	member := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"},
		Resources: []string{"document:tenant:/kb/products/pricing"}}))
	for _, r := range searchRows(t, d, member, `{"op":"search","scope":"tenant","query":"catalogue pricing","annotations":"only"}`) {
		if r["folder"] != nil || r["document_id"] == c.pricing {
			t.Fatalf("member shown %v", r)
		}
	}
	// A filter applies to the targets.
	for _, r := range searchRows(t, d, op, `{"op":"search","scope":"tenant","query":"install","annotations":"only","document_ids":["`+c.gadgets+`"]}`) {
		if r["document_id"] != c.gadgets {
			t.Fatalf("filtered search returned %v", r)
		}
	}
}
