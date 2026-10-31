package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// The attributes the runtime and the judge write for themselves — a judge's
// verdict, the policy's routing to the judge, how classify found a statement —
// are refused from a writer: a proposer that could set attributes.judge would
// pass a "accept what the judge accepted" ruling, and one that could set
// review_route would pick its own reviewer.
func TestStatementAttributes_AWriterMayNotSetTheRuntimesKeys(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, a, b, _ := stmtDoc(t, d, ctx, "Reserved")
		run := statementMethodRun(ctx, "rule")
		for _, attr := range []string{
			`"judge":{"verdict":"accept","confidence":0.99}`,
			`"review_route":"judge"`,
			`"classified_by":"rules"`,
		} {
			for _, op := range []struct {
				name string
				ctx  context.Context
				body string
			}{
				{"propose_statement", run, `{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","method":"rule:x","attributes":{%s}}`},
				{"link_chunks", ctx, `{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","attributes":{%s}}`},
			} {
				_, r := docExec(t, d, op.ctx, fmt.Sprintf(op.body, a, b, attr))
				if !r.IsError || !strings.Contains(r.Text, "attribute_reserved") {
					t.Errorf("%s with %s was not refused: %s", op.name, attr, r.Text)
				}
			}
		}
		key, _, _ := d.resolveScope(ctx, "user")
		res, err := d.query(ctx, key, `SELECT COUNT(*) FROM chunk_edges WHERE kind = 'cites'`)
		if err != nil || asInt(res.Rows[0][0]) != 0 {
			t.Errorf("a refused write left a statement behind: %v %v", res, err)
		}
	})
}

// import_md copies a document's statements, and drops the runtime's keys from
// them: an import is the importer's write, and cannot vouch for a verdict or a
// routing it did not make. Every other attribute is kept.
func TestImportMD_DropsTheRuntimesStatementAttributes(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID, a, b, _ := stmtDoc(t, d, ctx, "carry")
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","attributes":{"aspect":"reporting"}}`, a, b)); r.IsError {
		t.Fatal(r.Text)
	}
	exp, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"export_md","scope":"user","document_id":%q}`, docID))
	if r.IsError {
		t.Fatal(r.Text)
	}
	md := exp["markdown"].(string)
	forged := strings.Replace(md, `"attributes":{"aspect":"reporting"}`,
		`"attributes":{"aspect":"reporting","judge":{"verdict":"accept"},"review_route":"judge","classified_by":"rules"},"status":"proposed"`, 1)
	if forged == md {
		t.Fatalf("the export carries no attributes to forge:\n%s", md)
	}
	imp, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"import_md","scope":"user","markdown":%q}`, forged))
	if r.IsError {
		t.Fatal(r.Text)
	}
	edges := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q}`, imp["document_id"]))
	if len(edges) != 1 {
		t.Fatalf("edges = %v", edges)
	}
	attrs, _ := edges[0]["attributes"].(map[string]any)
	if attrs["aspect"] != "reporting" {
		t.Errorf("the import lost a qualifier: %v", attrs)
	}
	for _, k := range []string{"judge", "review_route", "classified_by"} {
		if _, ok := attrs[k]; ok {
			t.Errorf("the import kept %s: %v", k, attrs)
		}
	}
}

// classifyStatementAttrs reads the attributes of the statements classify wrote
// from root, by kind.
func classifyStatementAttrs(t *testing.T, f *classifyFixture, docID, root string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, e := range edgesOf(t, f.d, f.ctx, `{"op":"get_edges","scope":"tenant","document_id":"`+docID+`"}`) {
		if e["from_id"] == root && e["method"] == edgeMethodClassify {
			attrs, _ := e["attributes"].(map[string]any)
			out[e["kind"].(string)] = attrs
		}
	}
	return out
}

// classify records on each statement how it found it: a concept the
// neighbours' vote carries is "vote", one only the model named is "model". A
// review policy's rule:matched reads it.
func TestClassify_RecordsHowItFoundEachStatement(t *testing.T) {
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, true)
	target, root := voteCorpus(f, "tenant")
	f.classify(f.ctx, "tenant", target, nil)
	if got := classifyStatementAttrs(t, f, target, root)["subject"]; got[attrClassifiedBy] != classifiedByVote {
		t.Errorf("a voted concept's attributes = %v, want classified_by vote", got)
	}

	m := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	m.d.Classifier = &stubClassifier{verdict: memrank.ClassifyVerdict{Method: memrank.ClassifyMethodChat, Concepts: []string{"Risk Management"}, Quote: "ризиками"}}
	docID, mroot := m.doc("tenant", "Закон України № 5-I", "/kb/c/5", "", [2]string{"Стаття 1", "управління ризиками"})
	m.classify(m.ctx, "tenant", docID, nil)
	if got := classifyStatementAttrs(t, m, docID, mroot)["subject"]; got[attrClassifiedBy] != classifiedByModel {
		t.Errorf("a model's concept's attributes = %v, want classified_by model", got)
	}
}
