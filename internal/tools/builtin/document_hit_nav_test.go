package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// byDocumentsCounter counts reverse-index name lookups.
type byDocumentsCounter struct {
	store.Store
	n atomic.Int32
}

func (c *byDocumentsCounter) DirentListByDocuments(ctx context.Context, tenantID, scope, scopeID string, documentIDs []string) ([]store.DirentRow, error) {
	c.n.Add(1)
	return c.Store.DirentListByDocuments(ctx, tenantID, scope, scopeID, documentIDs)
}

type navHit struct {
	ChunkID    string  `json:"chunk_id"`
	DocumentID string  `json:"document_id"`
	Path       *string `json:"path"`
	HasBody    *bool   `json:"has_body"`
}

func navHits(t *testing.T, text, key string) map[string]navHit {
	t.Helper()
	var body map[string][]navHit
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("unmarshal: %v — %s", err, text)
	}
	out := map[string]navHit{}
	for _, h := range body[key] {
		out[h.ChunkID] = h
	}
	return out
}

func navChunk(t *testing.T, d *Document, ctx context.Context, docID, parent, title, body string) string {
	t.Helper()
	in := map[string]any{"op": "create_chunk", "document_id": docID, "title": title, "body": body}
	if parent != "" {
		in["parent_id"] = parent
	}
	b, _ := json.Marshal(in)
	out, res := docExec(t, d, ctx, string(b))
	if res.IsError {
		t.Fatalf("create_chunk %q: %s", title, res.Text)
	}
	return out["id"].(string)
}

func navDoc(t *testing.T, d *Document, ctx context.Context, title, path string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"op": "create_document", "title": title, "path": path})
	out, res := docExec(t, d, ctx, string(b))
	if res.IsError {
		t.Fatalf("create_document %q: %s", title, res.Text)
	}
	return out["document_id"].(string)
}

// TestDocumentSearch_HitPathIsTheFirstNameInSortOrder: a document filed under
// two names is reported by the first in plain sort order — the name
// list_statements groups it by — on search and related hits alike, even where
// another rule (shortest first) would pick the other name.
func TestDocumentSearch_HitPathIsTheFirstNameInSortOrder(t *testing.T) {
	d, _, ctx := mermaidDocFixture(t, "savepoint", "nesting")
	docID := navDoc(t, d, ctx, "Ops", "/b/c")
	if _, res := docExec(t, d, ctx, `{"op":"set_path","id":"`+docID+`","path":"/a/zzz/long"}`); res.IsError {
		t.Fatalf("set_path: %s", res.Text)
	}
	hit := navChunk(t, d, ctx, docID, "", "Tx", "savepoint nesting")
	other := navChunk(t, d, ctx, docID, "", "Tx2", "savepoint nesting again")

	_, res := docExec(t, d, ctx, `{"op":"search","query":"savepoint nesting"}`)
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	if h := navHits(t, res.Text, "chunks")[hit]; h.Path == nil || *h.Path != "/a/zzz/long" {
		t.Errorf("search hit path = %v, want /a/zzz/long: %s", h.Path, res.Text)
	}
	_, res = docExec(t, d, ctx, `{"op":"related","id":"`+hit+`"}`)
	if res.IsError {
		t.Fatalf("related: %s", res.Text)
	}
	if h := navHits(t, res.Text, "related")[other]; h.Path == nil || *h.Path != "/a/zzz/long" {
		t.Errorf("related hit path = %v, want /a/zzz/long: %s", h.Path, res.Text)
	}
}

// TestDocumentSearch_HitOfAnUnfiledDocumentCarriesNoPath: a document with no
// Path name carries no path field, not an empty one.
func TestDocumentSearch_HitOfAnUnfiledDocumentCarriesNoPath(t *testing.T) {
	d, _, ctx := mermaidDocFixture(t, "savepoint")
	docID := navDoc(t, d, ctx, "Loose", "/loose/doc")
	hit := navChunk(t, d, ctx, docID, "", "Tx", "savepoint")
	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := d.Store.DirentDelete(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), "/loose/", "doc"); err != nil || !ok {
		t.Fatalf("dirent delete: %v %v", ok, err)
	}
	_, res := docExec(t, d, ctx, `{"op":"search","query":"savepoint"}`)
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	h, ok := navHits(t, res.Text, "chunks")[hit]
	if !ok {
		t.Fatalf("hit missing: %s", res.Text)
	}
	if h.Path != nil {
		t.Errorf("unfiled document's hit carries path %q, want none", *h.Path)
	}
}

// TestDocumentSearch_HitPathsAreOneLookupPerPage: a page of hits from several
// documents costs one reverse-index lookup, not one per hit.
func TestDocumentSearch_HitPathsAreOneLookupPerPage(t *testing.T) {
	d, _, ctx := mermaidDocFixture(t, "savepoint")
	want := map[string]string{}
	for i := 0; i < 4; i++ {
		p := fmt.Sprintf("/kb/doc%d", i)
		doc := navDoc(t, d, ctx, fmt.Sprintf("Doc %d", i), p)
		want[navChunk(t, d, ctx, doc, "", "Tx", "savepoint")] = p
	}
	counter := &byDocumentsCounter{Store: d.Store}
	d.Store = counter
	_, res := docExec(t, d, ctx, `{"op":"search","query":"savepoint"}`)
	if res.IsError {
		t.Fatalf("search: %s", res.Text)
	}
	hits := navHits(t, res.Text, "chunks")
	for id, p := range want {
		if h := hits[id]; h.Path == nil || *h.Path != p {
			t.Errorf("hit %s path = %v, want %s", id, h.Path, p)
		}
	}
	if n := counter.n.Load(); n != 1 {
		t.Errorf("name lookups for one page = %d, want 1", n)
	}
}

// TestDocumentRelated_BodilessSectionHitIsMarked: a hit on a section whose own
// body is empty and whose text is in its children carries has_body:false; a
// chunk with a body, and an empty leaf, carry no has_body.
func TestDocumentRelated_BodilessSectionHitIsMarked(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := navDoc(t, d, ctx, "Spec", "/spec")
	from := navChunk(t, d, ctx, docID, "", "Intro", "see the results")
	section := navChunk(t, d, ctx, docID, "", "Results", "")
	navChunk(t, d, ctx, docID, section, "Run 1", "alpha")
	leaf := navChunk(t, d, ctx, docID, "", "Notes", "some notes")
	for _, to := range []string{section, leaf} {
		if _, res := docExec(t, d, ctx, `{"op":"link_chunks","from_id":"`+from+`","to_id":"`+to+`","kind":"cites"}`); res.IsError {
			t.Fatalf("link_chunks: %s", res.Text)
		}
	}
	_, res := docExec(t, d, ctx, `{"op":"related","related_to":"`+from+`"}`)
	if res.IsError {
		t.Fatalf("related: %s", res.Text)
	}
	hits := navHits(t, res.Text, "linked")
	if h, ok := hits[section]; !ok || h.HasBody == nil || *h.HasBody {
		t.Errorf("section hit has_body = %v (present %v), want false: %s", h.HasBody, ok, res.Text)
	}
	if h, ok := hits[leaf]; !ok || h.HasBody != nil {
		t.Errorf("leaf with a body: has_body = %v (present %v), want absent", h.HasBody, ok)
	}
	if h := hits[section]; h.Path == nil || *h.Path != "/spec" {
		t.Errorf("linked hit path = %v, want /spec", h.Path)
	}
}

// TestDocumentGetChunk_EmptySectionListsItsChildren: get_chunk on a section
// with no body of its own lists its children (id, title) in document order,
// capped with a truncated flag; a chunk with a body lists none.
func TestDocumentGetChunk_EmptySectionListsItsChildren(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := navDoc(t, d, ctx, "Spec", "/spec")
	section := navChunk(t, d, ctx, docID, "", "Results", "")
	a := navChunk(t, d, ctx, docID, section, "A", "alpha")
	b := navChunk(t, d, ctx, docID, section, "B", "beta")
	navChunk(t, d, ctx, docID, a, "A.1", "nested") // one level only

	type child struct{ ID, Title string }
	var got struct {
		Body      string  `json:"body"`
		Children  []child `json:"children"`
		Truncated bool    `json:"children_truncated"`
	}
	_, res := docExec(t, d, ctx, `{"op":"get_chunk","id":"`+section+`"}`)
	if res.IsError {
		t.Fatalf("get_chunk: %s", res.Text)
	}
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Children) != 2 || got.Children[0] != (child{a, "A"}) || got.Children[1] != (child{b, "B"}) || got.Truncated {
		t.Errorf("children = %+v truncated=%v, want [A B]", got.Children, got.Truncated)
	}

	got.Children = nil
	_, res = docExec(t, d, ctx, `{"op":"get_chunk","id":"`+a+`"}`)
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatal(err)
	}
	if got.Children != nil {
		t.Errorf("a chunk with a body listed children: %+v", got.Children)
	}

	big := navChunk(t, d, ctx, docID, "", "Big", "")
	for i := 0; i <= chunkOutlineCap; i++ {
		navChunk(t, d, ctx, docID, big, fmt.Sprintf("c%02d", i), "x")
	}
	got.Children = nil
	_, res = docExec(t, d, ctx, `{"op":"get_chunk","id":"`+big+`"}`)
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Children) != chunkOutlineCap || !got.Truncated || got.Children[0].Title != "c00" {
		t.Errorf("capped outline: %d children truncated=%v first=%+v, want %d true c00", len(got.Children), got.Truncated, got.Children, chunkOutlineCap)
	}
}
