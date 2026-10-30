package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// document_definitional_test.go — RFC FP R8: chunk reads say which chunks sit in a
// definitions passage, so a linker running outside Go never evaluates the RE2 pattern.

// definitionalDoc is a document of class docType laid out against sectionedOntologyMD's
// NormativeAct pattern `(?i)^Стаття 1\b|визначення термінів`:
//
//	root      "Закон про визначення термінів"   (matches, but the root is the document, not a passage)
//	├─ art1   "Стаття 1. Визначення термінів"   (matches)
//	│  └─ item "1) персональні дані — …"        (under art1)
//	└─ art10  "Стаття 10. Сфера дії"             (no match: \b stops "Стаття 1" at "10")
type definitionalDoc struct {
	docID, root, art1, item, art10 string
}

func newDefinitionalDoc(t *testing.T, d *Document, ctx context.Context, docType string) definitionalDoc {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"user","title":"Закон про визначення термінів","type":%q}`, docType))
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	doc := definitionalDoc{docID: out["document_id"].(string), root: out["root_chunk_id"].(string)}
	chunk := func(parent, title string) string {
		t.Helper()
		c, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"parent_id":%q,"title":%q,"body":"text"}`,
			doc.docID, parent, title))
		if r.IsError {
			t.Fatalf("create_chunk %q: %s", title, r.Text)
		}
		return c["id"].(string)
	}
	doc.art1 = chunk(doc.root, "Стаття 1. Визначення термінів")
	doc.item = chunk(doc.art1, "1) персональні дані — відомості про особу")
	doc.art10 = chunk(doc.root, "Стаття 10. Сфера дії")
	return doc
}

// getChunkDefinitional reads one chunk and reports whether its RAW response carries the
// key at all, and whether it is literally true.
func getChunkDefinitional(t *testing.T, d *Document, ctx context.Context, id string) (present, value bool) {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, id))
	if r.IsError {
		t.Fatalf("get_chunk %s: %s", id, r.Text)
	}
	v, has := out["definitional"]
	if has != strings.Contains(r.Text, `"definitional"`) {
		t.Fatalf("decoded and raw responses disagree on the key: %s", r.Text)
	}
	b, _ := v.(bool)
	return has, b
}

// queryChunksDefinitional runs a structured query_chunks and maps each returned chunk id
// to whether its row carries the key (and the key's value must then be true).
func queryChunksDefinitional(t *testing.T, d *Document, ctx context.Context, filter string) map[string]bool {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"query_chunks","scope":"user"`+filter+`}`)
	if r.IsError {
		t.Fatalf("query_chunks: %s", r.Text)
	}
	rows, _ := out["chunks"].([]any)
	got := map[string]bool{}
	for _, raw := range rows {
		row := raw.(map[string]any)
		v, has := row["definitional"]
		if has && v != true {
			t.Errorf("chunk %v: definitional = %#v; the key is present only as true", row["id"], v)
		}
		got[row["id"].(string)] = has
	}
	return got
}

// TestDocumentDefinitional_PassageAndItsDescendantsAreFlagged: in a Law (which inherits
// @definitions from NormativeAct), the matching article and the item under it carry
// definitional:true; the sibling article and the root carry no key at all.
func TestDocumentDefinitional_PassageAndItsDescendantsAreFlagged(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, sectionedOntologyMD, memrank.OntologyConfirmedStatus)
	doc := newDefinitionalDoc(t, d, ctx, "Law")

	for _, c := range []struct {
		name, id string
		want     bool
	}{
		{"art1 (title matches)", doc.art1, true},
		{"item (under art1)", doc.item, true},
		{"art10 (sibling)", doc.art10, false},
		{"root (the document itself)", doc.root, false},
	} {
		present, value := getChunkDefinitional(t, d, ctx, c.id)
		if c.want && !(present && value) {
			t.Errorf("get_chunk %s: definitional present=%v value=%v, want true", c.name, present, value)
		}
		if !c.want && present {
			t.Errorf("get_chunk %s: carries the definitional key; want it omitted", c.name)
		}
	}

	got := queryChunksDefinitional(t, d, ctx, fmt.Sprintf(`,"document_id":%q`, doc.docID))
	want := map[string]bool{doc.root: false, doc.art1: true, doc.item: true, doc.art10: false}
	for id, w := range want {
		has, listed := got[id]
		if !listed {
			t.Errorf("query_chunks did not return chunk %s", id)
			continue
		}
		if has != w {
			t.Errorf("query_chunks chunk %s: definitional present=%v, want %v", id, has, w)
		}
	}
}

// TestDocumentDefinitional_ClassWithoutDirectiveIsNeverFlagged: the same titles in a
// document whose class declares (and inherits) no @definitions carry no key — also when
// one query_chunks page mixes it with a Law, so the pattern is chosen per document.
func TestDocumentDefinitional_ClassWithoutDirectiveIsNeverFlagged(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, sectionedOntologyMD, memrank.OntologyConfirmedStatus)
	plain := newDefinitionalDoc(t, d, ctx, "Document")
	law := newDefinitionalDoc(t, d, ctx, "Law")

	for _, id := range []string{plain.root, plain.art1, plain.item, plain.art10} {
		if present, _ := getChunkDefinitional(t, d, ctx, id); present {
			t.Errorf("get_chunk %s of a Document: carries the definitional key", id)
		}
	}
	got := queryChunksDefinitional(t, d, ctx, "")
	for _, id := range []string{plain.root, plain.art1, plain.item, plain.art10} {
		if got[id] {
			t.Errorf("query_chunks chunk %s of a Document: carries the definitional key", id)
		}
	}
	if !got[law.art1] || !got[law.item] || got[law.art10] {
		t.Errorf("query_chunks over both documents: law art1=%v item=%v art10=%v, want true true false",
			got[law.art1], got[law.item], got[law.art10])
	}
}

// TestDocumentDefinitional_DraftOntologyFlagsNothing: an unconfirmed ontology governs
// nothing, so even a matching passage of a Law carries no key on either read.
func TestDocumentDefinitional_DraftOntologyFlagsNothing(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, sectionedOntologyMD, memrank.OntologyDraftStatus)
	doc := newDefinitionalDoc(t, d, ctx, "Law")

	for _, id := range []string{doc.art1, doc.item} {
		if present, _ := getChunkDefinitional(t, d, ctx, id); present {
			t.Errorf("get_chunk %s under a draft ontology: carries the definitional key", id)
		}
	}
	for id, has := range queryChunksDefinitional(t, d, ctx, fmt.Sprintf(`,"document_id":%q`, doc.docID)) {
		if has {
			t.Errorf("query_chunks chunk %s under a draft ontology: carries the definitional key", id)
		}
	}
}

// TestDocumentDefinitional_FactUnderAPassageIsNotFlagged: a fact chunk (one with a
// chunk_memory_meta sidecar) is a claim about a subject, not text of the passage it
// sits under, so neither read flags it — while its non-fact sibling still is.
func TestDocumentDefinitional_FactUnderAPassageIsNotFlagged(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := installTenantOntology(t, d, base, sectionedOntologyMD, memrank.OntologyConfirmedStatus)
	doc := newDefinitionalDoc(t, d, ctx, "Law")
	c, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"parent_id":%q,"title":"a fact","body":"text"}`,
		doc.docID, doc.art1))
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	fact := c["id"].(string)
	if err := sidecarInsert(t, d, ctx, fact, ""); err != nil {
		t.Fatalf("sidecar: %v", err)
	}

	if present, _ := getChunkDefinitional(t, d, ctx, fact); present {
		t.Error("get_chunk of a fact under the definitions article carries the definitional key")
	}
	got := queryChunksDefinitional(t, d, ctx, fmt.Sprintf(`,"document_id":%q`, doc.docID))
	if got[fact] {
		t.Error("query_chunks row of a fact under the definitions article carries the definitional key")
	}
	if !got[doc.item] {
		t.Error("the non-fact item under the same article lost its flag")
	}
}
