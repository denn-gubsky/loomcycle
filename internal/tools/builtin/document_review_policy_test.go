package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
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

// policyFixture is a tenant whose confirmed ontology carries md, with three
// documents to propose between: /board/reg/one (two parts), /board/reg/two and
// /kb/notes/n.
type policyFixture struct {
	t         *testing.T
	d         *Document
	op, run   context.Context // the operator plane; a linker's run (rule and extractor methods)
	one, oneB string          // parts of /board/reg/one
	two       string          // a part of /board/reg/two
	notes     string          // a part of /kb/notes/n
}

func newPolicyFixture(t *testing.T, d *Document, base context.Context, md string) *policyFixture {
	t.Helper()
	installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus)
	f := &policyFixture{t: t, d: d, op: reviewOperatorCtx(base, nil), run: reviewWriterCtx(base)}
	mk := func(path string, bodies ...string) []string {
		out, res := docExec(t, d, f.op, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":%q}`, path, path))
		if res.IsError {
			t.Fatalf("create_document %s: %s", path, res.Text)
		}
		var ids []string
		for i, body := range bodies {
			out, res := docExec(t, d, f.op, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"part %d","body":%q}`, out["document_id"], i, body))
			if res.IsError {
				t.Fatalf("create_chunk %s: %s", path, res.Text)
			}
			ids = append(ids, out["id"].(string))
		}
		return ids
	}
	one := mk("/board/reg/one", "Article 4 cites the Act of 2015.", "Article 5 refers to Article 4.")
	f.one, f.oneB = one[0], one[1]
	f.two = mk("/board/reg/two", "The Act of 2015 sets the duty.")[0]
	f.notes = mk("/kb/notes/n", "These notes elaborate the duty.")[0]
	return f
}

// propose files a statement from the linker's run; extra is appended to the
// call (method, evidence, confidence).
func (f *policyFixture) propose(from, to, kind, extra string) map[string]any {
	f.t.Helper()
	out, res := docExec(f.t, f.d, f.run, fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":%q%s}`, from, to, kind, extra))
	if res.IsError {
		f.t.Fatalf("propose %s: %s", kind, res.Text)
	}
	return out
}

func (f *policyFixture) edge(id string) map[string]any {
	f.t.Helper()
	out, res := docExec(f.t, f.d, f.op, fmt.Sprintf(`{"op":"get_edge","scope":"tenant","id":%q}`, id))
	if res.IsError {
		f.t.Fatalf("get_edge: %s", res.Text)
	}
	return out["statement"].(map[string]any)
}

// citation is a rule:citation proposal from one to two with its quote found.
func (f *policyFixture) citation(kind string) map[string]any {
	return f.propose(f.one, f.two, kind, fmt.Sprintf(`,"method":"rule:citation","confidence":0.9,"evidence":[{"chunk_id":%q,"quote":"cites the Act of 2015"}]`, f.one))
}

// At filing, accept rules the proposal as an operator's accept would, recorded
// as policy:<rule>@<version> under a ruling of the run, which revert_ruling
// undoes; get_edge names the ruling.
func TestReviewPolicy_AcceptAtFilingIsARevertableRuling(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, reviewOntologyMD)
		pf, _ := d.reviewPolicyInForce(f.op)
		want := fmt.Sprintf("policy:1@%d", pf.version)
		out := f.citation("cites")
		if out["status"] != edgeStatusAsserted || out["ruled_by"] != want || out["ruling_id"] == nil {
			t.Fatalf("filed = %v, want asserted by %s", out, want)
		}
		e := f.edge(out["id"].(string))
		if e["status"] != edgeStatusAsserted || e["ruled_by"] != want || e["ruling_id"] != out["ruling_id"] || e["operator_ruled_at"] == nil {
			t.Fatalf("statement = %v", e)
		}
		// A second acceptance in the same run joins the run's ruling.
		rep := f.citation("repeals")
		if rep["status"] != edgeStatusAsserted || rep["ruling_id"] != out["ruling_id"] {
			t.Fatalf("second filing = %v, want the ruling %v", rep, out["ruling_id"])
		}
		rev, err := d.RevertRuling(f.op, "tenant", connector.RevertRulingRequest{RulingID: out["ruling_id"].(string)}, RuleAsOperator)
		if err != nil || rev.Reverted != 2 || rev.Action != "accept" {
			t.Fatalf("revert = %+v %v", rev, err)
		}
		for _, id := range []any{out["id"], rep["id"]} {
			if e := f.edge(id.(string)); e["status"] != edgeStatusProposed || e["ruled_by"] != nil {
				t.Errorf("after the revert %v", e)
			}
		}
	})
}

// At filing, reject rules the proposal rejected with the rule as its reason: a
// tombstone, as an operator's rejection is, so the proposer does not file it
// again every run; the revert lifts it.
func TestReviewPolicy_RejectAtFilingIsATombstone(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, reviewOntologyMD)
		out := f.propose(f.notes, f.two, "elaborates", `,"method":"extractor:relate","confidence":0.4`)
		if out["status"] != edgeStatusRejected || !strings.HasPrefix(out["ruled_by"].(string), "policy:3@") {
			t.Fatalf("filed = %v, want rejected by rule 3", out)
		}
		e := f.edge(out["id"].(string))
		if !strings.Contains(e["judge_reason"].(string), "reject kind:elaborates method:extractor:relate when confidence<0.6") {
			t.Errorf("reason = %v", e["judge_reason"])
		}
		_, res := docExec(t, d, f.run, fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"elaborates","method":"extractor:relate","confidence":0.4}`, f.notes, f.two))
		if !res.IsError || !strings.Contains(res.Text, "statement_rejected") {
			t.Errorf("a policy rejection is no tombstone: %s", res.Text)
		}
		if _, err := d.RevertRuling(f.op, "tenant", connector.RevertRulingRequest{RulingID: out["ruling_id"].(string)}, RuleAsOperator); err != nil {
			t.Fatal(err)
		}
		if e := f.edge(out["id"].(string)); e["status"] != edgeStatusProposed {
			t.Errorf("after the revert %v", e)
		}
	})
}

// judge leaves the proposal proposed and routes it to the judge
// (attributes.review_route); human leaves it as it is.
func TestReviewPolicy_JudgeRoutesAndHumanLeavesIt(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, reviewOntologyMD)
		out := f.propose(f.notes, f.two, "elaborates", `,"method":"extractor:relate","confidence":0.7`)
		e := f.edge(out["id"].(string))
		attrs, _ := e["attributes"].(map[string]any)
		if out["status"] != edgeStatusProposed || out["review_route"] != "judge" || attrs[attrReviewRoute] != "judge" || e["ruled_by"] != nil {
			t.Errorf("judge: filed %v, statement %v", out, e)
		}
		out = f.propose(f.notes, f.one, "defines", `,"method":"rule:terms"`)
		e = f.edge(out["id"].(string))
		if out["status"] != edgeStatusProposed || out["review_route"] != nil || e["attributes"] != nil || e["ruled_by"] != nil {
			t.Errorf("human: filed %v, statement %v", out, e)
		}
	})
}

// MTB's first policy: a rule:citation proposal with its quote found, between two
// documents, is accepted; a self-citation, one without a found quote and a
// model-made one stay proposed for a person.
func TestReviewPolicy_MTBFirstPolicy(t *testing.T) {
	d, base, _ := documentFixture(t)
	f := newPolicyFixture(t, d, base, reviewOntologyMD)
	if out := f.citation("cites"); out["status"] != edgeStatusAsserted {
		t.Errorf("a resolved rule:citation = %v", out)
	}
	for name, out := range map[string]map[string]any{
		"self-citation":  f.propose(f.one, f.oneB, "cites", fmt.Sprintf(`,"method":"rule:citation","evidence":[{"chunk_id":%q,"quote":"cites the Act"}]`, f.one)),
		"no quote":       f.propose(f.two, f.one, "cites", `,"method":"rule:citation"`),
		"model-made":     f.propose(f.notes, f.one, "cites", fmt.Sprintf(`,"method":"extractor:relate","evidence":[{"chunk_id":%q,"quote":"elaborate the duty"}]`, f.notes)),
		"another method": f.propose(f.notes, f.two, "repeals", fmt.Sprintf(`,"method":"rule:terms","evidence":[{"chunk_id":%q,"quote":"elaborate the duty"}]`, f.notes)),
	} {
		if out["status"] != edgeStatusProposed || out["ruled_by"] != nil {
			t.Errorf("%s = %v, want proposed", name, out)
		}
	}
}

// MTB's first policy over classify: a subject tag the neighbours' vote carries
// is accepted, one only the model named stays proposed.
func TestReviewPolicy_ClassifiesRuleMatchedSubjectTags(t *testing.T) {
	md := classifyOntologyMD + "\n## Review\n@section review\n- accept kind:subject method:classify when rule:matched\n- human *\n"
	f := newClassifyFixture(t, memrank.OntologyConfirmedStatus, true)
	f.ctx = operatorCtx(installTenantOntology(t, f.d, f.ctx, md, memrank.OntologyConfirmedStatus))
	target, root := voteCorpus(f, "tenant")
	a := f.classify(f.ctx, "tenant", target, nil)
	if len(a.Statements) != 1 || a.Statements[0].Status != edgeStatusAsserted || !strings.HasPrefix(a.Statements[0].RuledBy, "policy:1@") {
		t.Fatalf("a voted subject tag = %+v", a.Statements)
	}
	if rec := pendingOf(t, f.root("tenant", root)); len(rec.Statements) != 0 {
		t.Errorf("an accepted tag is still pending: %+v", rec)
	}

	m := newClassifyFixture(t, memrank.OntologyConfirmedStatus, false)
	m.ctx = operatorCtx(installTenantOntology(t, m.d, m.ctx, md, memrank.OntologyConfirmedStatus))
	m.d.Classifier = &stubClassifier{verdict: memrank.ClassifyVerdict{Method: memrank.ClassifyMethodChat, Concepts: []string{"Risk Management"}, Quote: "ризиками"}}
	docID, _ := m.doc("tenant", "Закон України № 5-I", "/kb/c/5", "", [2]string{"Стаття 1", "управління ризиками"})
	b := m.classify(m.ctx, "tenant", docID, nil)
	if len(b.Statements) != 1 || b.Statements[0].Status != edgeStatusProposed {
		t.Errorf("a model's subject tag = %+v", b.Statements)
	}
}

// A Review section that does not parse rules nothing: a proposal its rules
// would have accepted waits, and @auto_assert does not apply beside it.
func TestReviewPolicy_AMalformedSectionRulesNothing(t *testing.T) {
	d, base, _ := documentFixture(t)
	md := strings.Replace(reviewOntologyMD, "- human  *", "- human  * when", 1)
	f := newPolicyFixture(t, d, base, md)
	if pf, ok := d.reviewPolicyInForce(f.op); !ok || !pf.section || pf.policy.Applied() {
		t.Fatalf("policy = %+v %v", pf, ok)
	}
	if out := f.citation("cites"); out["status"] != edgeStatusProposed {
		t.Errorf("a broken policy accepted %v", out)
	}
	out := f.propose(f.one, f.two, "supervises", fmt.Sprintf(`,"method":"rule:l2","evidence":[{"chunk_id":%q,"quote":"cites the Act"}]`, f.one))
	if out["status"] != edgeStatusProposed {
		t.Errorf("@auto_assert applied beside a broken Review section: %v", out)
	}
}

// Without a Review section, filing rules exactly as before: an @auto_assert
// kind made by a rule with every quote in a chunk is written asserted — with no
// ruling and no ruled_by — and every other proposal stays proposed; and the
// policy describe_ontology shows decides each proposal the same way.
func TestReviewPolicy_AutoAssertRulesExactlyAsBefore(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		f := newPolicyFixture(t, d, base, relationsOntologyMD)
		pf, ok := d.reviewPolicyInForce(f.op)
		if !ok || pf.section || len(pf.policy.Rules) != 1 {
			t.Fatalf("policy = %+v %v", pf, ok)
		}
		i := 0
		for _, kind := range []string{"supervises", "cites"} {
			for _, method := range []string{`,"method":"rule:l2"`, `,"method":"extractor:m"`, ``} {
				for _, withQuote := range []bool{true, false} {
					// Fresh ends per case, so each files a new statement.
					i++
					doc, res := docExec(t, d, f.op, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":"case %d"}`, i))
					if res.IsError {
						t.Fatal(res.Text)
					}
					var ends [2]string
					for j := range ends {
						c, res := docExec(t, d, f.op, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"p%d","body":"Article 4 cites the Act."}`, doc["document_id"], j))
						if res.IsError {
							t.Fatal(res.Text)
						}
						ends[j] = c["id"].(string)
					}
					from, to, ev := ends[0], ends[1], ""
					if withQuote {
						ev = fmt.Sprintf(`,"evidence":[{"chunk_id":%q,"quote":"cites the Act"}]`, from)
					}
					out := f.propose(from, to, kind, method+ev)
					legacy := kind == "supervises" && method == `,"method":"rule:l2"` && withQuote
					wantStatus := map[bool]string{true: edgeStatusAsserted, false: edgeStatusProposed}[legacy]
					c, err := d.policyCandidateByID(f.op, mustTenantKey(t, d, f.op), pf, out["id"].(string))
					if err != nil || c == nil {
						t.Fatal(err)
					}
					_, action := pf.decide(*c)
					if out["status"] != wantStatus || (action == memrank.ReviewAccept) != legacy {
						t.Errorf("%s%s%s: status %v, policy %s; @auto_assert wants %s", kind, method, ev, out["status"], action, wantStatus)
					}
					if out["ruled_by"] != nil || out["ruling_id"] != nil {
						t.Errorf("%s%s%s: an @auto_assert filing recorded a ruling: %v", kind, method, ev, out)
					}
				}
			}
		}
		key := mustTenantKey(t, d, f.op)
		res, err := d.query(f.op, key, `SELECT COUNT(*) FROM statement_rulings`)
		if err != nil || asInt(res.Rows[0][0]) != 0 {
			t.Errorf("rulings recorded without a Review section: %v %v", res, err)
		}
	})
}
