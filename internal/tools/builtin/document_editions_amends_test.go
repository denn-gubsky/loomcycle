package builtin

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// An amending act renumbers a work: an article is inserted, the two after it
// move down, and the last one is rewritten. Article 1 is untouched.
const (
	amendEd1 = "# Act\n\n## Article 1\n\nOne.\n\n## Article 2\n\nTwo.\n\n## Article 3\n\nThree.\n\n## Article 9\n\nNine.\n"
	amendEd2 = "# Act\n\n## Article 1\n\nOne.\n\n## Article 2\n\nInserted.\n\n## Article 3\n\nTwo.\n\n## Article 4\n\nThree.\n\n## Article 9\n\nNine, amended.\n"
)

func reimportAmending(t *testing.T, d *Document, ctx context.Context, md, wk, amending string, edition int64) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": md, "reimport": true,
		"work_key": wk, "edition_date": edition, "amending_document_id": amending})
	out, r := docExec(t, d, ctx, string(b))
	if r.IsError {
		t.Fatalf("reimport: %s", r.Text)
	}
	return out
}

type amendsRow struct {
	from, to, method, status string
	attrs                    map[string]any
	validAt                  int64
}

func amendsRows(t *testing.T, d *Document, ctx context.Context) []amendsRow {
	t.Helper()
	res, err := d.query(ctx, userKey(t, d, ctx), `SELECT from_id, to_id, method, status, COALESCE(attributes, ''), COALESCE(valid_at, 0)
		FROM chunk_edges WHERE kind = 'amends' ORDER BY to_id`)
	if err != nil {
		t.Fatal(err)
	}
	var out []amendsRow
	for _, r := range res.Rows {
		row := amendsRow{from: asStr(r[0]), to: asStr(r[1]), method: asStr(r[2]), status: asStr(r[3])}
		_ = json.Unmarshal([]byte(asStr(r[4])), &row.attrs)
		row.validAt, _ = asInt64(r[5])
		out = append(out, row)
	}
	return out
}

func amendingDoc(t *testing.T, d *Document, ctx context.Context) (docID, rootID string) {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Amending act"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	return out["document_id"].(string), out["root_chunk_id"].(string)
}

// Each part the amending act renumbered gets one `amends` statement from the
// act's root, dated by the edition; untouched, superseded and new parts get
// none, and running the same edition again adds nothing.
func TestReimport_RenumberingWritesOneAmendsPerMovedPart(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		const wk = "law:ua/amend-1"
		const ed2 = int64(2_000_000)
		docID := reimport(t, d, ctx, amendEd1, wk, 1_000_000)["document_id"].(string)
		v1 := currentPartsOf(t, d, ctx, docID, 1_000_001)
		amendID, amendRoot := amendingDoc(t, d, ctx)

		out := reimportAmending(t, d, ctx, amendEd2, wk, amendID, ed2)
		rep := out["reimport"].(map[string]any)
		for k, v := range map[string]float64{"unchanged": 1, "moved": 2, "superseded": 1, "created": 1, "closed": 0, "amends_written": 2} {
			if rep[k] != v {
				t.Errorf("%s = %v, want %v (report %v)", k, rep[k], v, rep)
			}
		}
		v2 := currentPartsOf(t, d, ctx, docID, ed2+1)
		moved := []string{v1["article-2"].id, v1["article-3"].id}
		if v2["article-3"].id != moved[0] || v2["article-4"].id != moved[1] {
			t.Fatalf("the renumbering was not read as two moves: v1=%+v v2=%+v", v1, v2)
		}

		rows := amendsRows(t, d, ctx)
		var to []string
		for _, r := range rows {
			to = append(to, r.to)
			if r.from != amendRoot || r.method != edgeMethodImport || r.status != edgeStatusAsserted ||
				r.attrs["mod_type"] != "renumbering" || r.validAt != ed2 {
				t.Errorf("amends statement = %+v, want from the amending root %s, method import, asserted, mod_type renumbering, valid_at %d", r, amendRoot, ed2)
			}
		}
		slices.Sort(to)
		slices.Sort(moved)
		if !slices.Equal(to, moved) {
			t.Errorf("amends statements point at %v, want exactly the moved parts %v (untouched %s, inserted %s, superseded %s/%s)",
				to, moved, v2["article-1"].id, v2["article-2"].id, v1["article-9"].id, v2["article-9"].id)
		}
		// The moved parts get no statement besides it.
		res, err := d.query(ctx, userKey(t, d, ctx), `SELECT count(*) FROM chunk_edges WHERE kind <> 'amends' AND (from_id IN (?, ?) OR to_id IN (?, ?))`,
			moved[0], moved[1], moved[0], moved[1])
		if err != nil || asInt(res.Rows[0][0]) != 0 {
			t.Errorf("a moved part got another statement: %v %v", res, err)
		}

		again := reimportAmending(t, d, ctx, amendEd2, wk, amendID, ed2)["reimport"].(map[string]any)
		if _, has := again["amends_written"]; has || again["unchanged"] != float64(5) {
			t.Errorf("a rerun of the same edition = %v", again)
		}
		if n := len(amendsRows(t, d, ctx)); n != 2 {
			t.Errorf("a rerun of the same edition left %d amends statements, want 2", n)
		}
	})
}

// The amending document is an endpoint, so the caller must read and link it.
// One it may not is refused exactly as one that does not exist, before anything
// is written, so the refusal is no oracle over the scope's documents.
func TestReimport_UnreadableAmendingDocumentIsRefusedLikeAMissingOne(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, open)
	b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "tenant", "markdown": amendEd1, "reimport": true,
		"work_key": "law:ua/amend-2", "edition_date": 1_000_000, "path": "/kb/public/act"})
	out, r := docExec(t, d, open, string(b))
	if r.IsError {
		t.Fatal(r.Text)
	}
	docID := out["document_id"].(string)
	parts := func(at int64) int {
		key, _, err := d.resolveScope(open, "tenant")
		if err != nil {
			t.Fatal(err)
		}
		res, err := d.query(open, key, `SELECT count(*) FROM chunks c JOIN documents dd ON dd.id = c.document_id
			WHERE c.document_id = ? AND c.id <> dd.root_chunk_id AND (c.invalid_at IS NULL OR c.invalid_at > ?)`, docID, at)
		if err != nil {
			t.Fatal(err)
		}
		return asInt(res.Rows[0][0])
	}
	before := parts(2_000_001)

	call := func(ctx context.Context, amending string) tools.Result {
		b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "tenant", "markdown": amendEd2, "reimport": true,
			"work_key": "law:ua/amend-2", "edition_date": 2_000_000, "amending_document_id": amending})
		_, r := docExec(t, d, ctx, string(b))
		return r
	}
	allowAll := authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}}
	missing := call(open, "0123456789abcdef0123456789abcdef")
	if !missing.IsError {
		t.Fatalf("a missing amending document was accepted")
	}
	for name, verbs := range map[string][]string{"read": {"read"}, "link": {"link"}} {
		denied := tenantDocCtx(base, aclSet(t, allowAll,
			authz.Rule{Effect: authz.Deny, Actions: verbs, Resources: []string{"document:tenant:/kb/secret/**"}}))
		r := call(denied, s.secretDoc)
		got := strings.ReplaceAll(r.Text, s.secretDoc, "X")
		want := strings.ReplaceAll(missing.Text, "0123456789abcdef0123456789abcdef", "X")
		if !r.IsError || got != want || r.Error == nil || missing.Error == nil || r.Error.Category != missing.Error.Category {
			t.Errorf("denied %s: refusal %q differs from a missing document's %q", name, got, want)
		}
	}
	if n := parts(2_000_001); n != before {
		t.Errorf("a refused reimport changed the work: %d parts in force, want %d", n, before)
	}
	// Control: a document the caller may read and link is accepted.
	if r := call(open, s.publicDoc); r.IsError {
		t.Errorf("control: %s", r.Text)
	}
}

// Where the tenant's vocabulary does not accept the statement, the reimport
// still completes: each refusal is counted by its code, never fatal.
func TestReimport_AmendsTheVocabularyRefusesIsCountedNotFatal(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		ctx = installTenantOntology(t, d, ctx, "# Tenant Ontology\n\n## Relations\n@section relations\n\n"+
			"### amends\n@qualifiers mod_type(substitution|insertion)\n@discriminator mod_type\n", memrank.OntologyConfirmedStatus)
		const wk = "law:ua/amend-3"
		reimport(t, d, ctx, amendEd1, wk, 1_000_000)
		amendID, _ := amendingDoc(t, d, ctx)
		rep := reimportAmending(t, d, ctx, amendEd2, wk, amendID, 2_000_000)["reimport"].(map[string]any)
		skipped, _ := rep["amends_skipped"].(map[string]any)
		if rep["moved"] != float64(2) || skipped["qualifier_violation"] != float64(2) {
			t.Errorf("report = %v, want moved 2 and amends_skipped {qualifier_violation: 2}", rep)
		}
		if _, has := rep["amends_written"]; has {
			t.Errorf("a refused amends statement was counted as written: %v", rep)
		}
		if n := len(amendsRows(t, d, ctx)); n != 0 {
			t.Errorf("%d amends statements written against the vocabulary", n)
		}
	})
}

// amending_document_id only means something on a reimport by work key.
func TestImportMD_AmendingDocumentWithoutAWorkKeyIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	amendID, _ := amendingDoc(t, d, ctx)
	docID := reimport(t, d, ctx, amendEd1, "law:ua/amend-4", 1_000_000)["document_id"].(string)
	for name, in := range map[string]map[string]any{
		"reimport by document_id": {"reimport": true, "document_id": docID, "edition_date": 2_000_000},
		"no reimport":             {},
	} {
		call := map[string]any{"op": "import_md", "scope": "user", "markdown": amendEd2, "amending_document_id": amendID}
		for k, v := range in {
			call[k] = v
		}
		b, _ := json.Marshal(call)
		_, r := docExec(t, d, ctx, string(b))
		if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryValidation || !strings.Contains(r.Text, "work_key") {
			t.Errorf("%s: want a validation refusal naming work_key, got %s", name, r.Text)
		}
	}
	docs, _ := docExec(t, d, ctx, `{"op":"query_documents","scope":"user"}`)
	if n := len(docs["documents"].([]any)); n != 2 {
		t.Errorf("a refused import wrote a document: %d documents, want 2", n)
	}
}
