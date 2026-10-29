package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC FN P0 — a relation is a statement. These tests run on both SQL Memory tiers
// (the postgres half is skipped without LOOMCYCLE_TEST_SQLMEM_PG_DSN).

func statementsBothTiers(t *testing.T, fn func(*testing.T, *Document, context.Context)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		d, ctx, _ := documentFixture(t)
		fn(t, d, ctx)
	})
	t.Run("postgres", func(t *testing.T) {
		d, ctx := pgDocumentOrSkip(t)
		fn(t, d, ctx)
	})
}

// stmtDoc makes a document with three chunks and returns their ids and the
// document id. Chunk "a" carries a body to quote from.
func stmtDoc(t *testing.T, d *Document, ctx context.Context, title string) (docID, a, b, c string) {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"user","title":%q}`, title))
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID = out["document_id"].(string)
	mk := func(name, body string) string {
		o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":%q,"body":%q}`, docID, name, body))
		if r.IsError {
			t.Fatalf("create_chunk %s: %s", name, r.Text)
		}
		return o["id"].(string)
	}
	return docID, mk("Clause 4", "The licensee shall comply with Article 12 of the Act."),
		mk("Article 12", "Article 12 sets the reporting duty."), mk("Notes", "unrelated")
}

func operatorCtx(ctx context.Context) context.Context {
	return tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) { p.Tools = []string{"*"} })
}

func inRunCtx(ctx context.Context) context.Context {
	return tools.WithRunID(ctx, "run-1")
}

func edgesOf(t *testing.T, d *Document, ctx context.Context, body string) []map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, body)
	if r.IsError {
		t.Fatalf("%s: %s", body, r.Text)
	}
	field := "edges"
	if _, ok := out["backlinks"]; ok {
		field = "backlinks"
	}
	raw, _ := out[field].([]any)
	res := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		res = append(res, e.(map[string]any))
	}
	return res
}

// A plain link is the old link plus an envelope: the same fields a reader of the
// old shape used, asserted and manual, no evidence block when none was given.
func TestLinkChunks_PlainLinkKeepsTheOldShape(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docID, a, b, _ := stmtDoc(t, d, ctx, "plain")
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))
		if r.IsError {
			t.Fatalf("link: %s", r.Text)
		}
		if out["ok"] != true || out["status"] != "asserted" || out["id"] == "" {
			t.Fatalf("link result = %v", out)
		}
		// Idempotent: the same triple again is the same statement.
		again, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))
		if again["id"] != out["id"] {
			t.Fatalf("relink made a second statement: %v vs %v", again["id"], out["id"])
		}
		edges := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q}`, docID))
		if len(edges) != 1 {
			t.Fatalf("edges = %v", edges)
		}
		e := edges[0]
		if e["from_id"] != a || e["to_id"] != b || e["kind"] != "cites" || e["auto"] != false ||
			e["method"] != "manual" || e["from_title"] != "Clause 4" || e["to_title"] != "Article 12" {
			t.Fatalf("edge = %v", e)
		}
		if _, ok := e["evidence"]; ok {
			t.Fatalf("an edge without evidence lists an evidence block: %v", e)
		}
	})
}

// The runtime's own kinds cannot be forged: a manual `supersedes` would make a
// real correction look already made (priorSuperseder), a manual `references` is
// mistaken for an inline [[name]] link.
func TestLinkChunks_RefusesARuntimeKind(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, a, b, _ := stmtDoc(t, d, ctx, "reserved")
		for _, kind := range []string{"supersedes", "references", "mentions", "derived_from", "supported_by"} {
			_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":%q}`, a, b, kind))
			if !r.IsError || !strings.Contains(r.Text, "kind_reserved") {
				t.Errorf("link %s: want kind_reserved, got %s", kind, r.Text)
			}
			_, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":%q}`, a, b, kind))
			if !r.IsError || !strings.Contains(r.Text, "kind_reserved") {
				t.Errorf("propose %s: want kind_reserved, got %s", kind, r.Text)
			}
		}
		// about through link_chunks only from a fact (a consolidator still on the old
		// path); from ordinary prose it is refused. Proposing one is refused.
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"about"}`, a, b)); !r.IsError || !strings.Contains(r.Text, "not a fact") {
			t.Errorf("link about from prose: want a refusal, got %s", r.Text)
		}
		docID := firstDocOf(t, d, ctx, a)
		fact := upsertIn(t, d, ctx, docID, "f|about|x", "a fact")
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"about"}`, fact, b)); r.IsError {
			t.Errorf("link about from a fact: %s", r.Text)
		}
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"about"}`, a, b)); !r.IsError {
			t.Errorf("propose about was accepted")
		}
	})
}

// Evidence is checked, never trusted: a quote that does not occur is refused and
// nothing is written; one that does is stored with its span, and a second link of
// the same triple adds to the same statement without duplicating a row.
func TestLinkChunks_EvidenceIsVerifiedAndAccumulates(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docID, a, b, _ := stmtDoc(t, d, ctx, "evidence")
		_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
			"evidence":[{"chunk_id":%q,"quote":"comply with Article 99"}]}`, a, b, a))
		if !r.IsError || !strings.Contains(r.Text, "evidence_quote_not_found") {
			t.Fatalf("forged quote: want evidence_quote_not_found, got %s", r.Text)
		}
		if edges := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q}`, docID)); len(edges) != 0 {
			t.Fatalf("a refused link still wrote an edge: %v", edges)
		}
		link := func(quote string) map[string]any {
			out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
				"evidence":[{"chunk_id":%q,"quote":%q}]}`, a, b, a, quote))
			if r.IsError {
				t.Fatalf("link: %s", r.Text)
			}
			return out
		}
		first := link("comply with Article 12")
		second := link("comply with Article 12") // same row: not written twice
		third := link("The licensee shall")
		if first["evidence_written"] != float64(1) || second["evidence_written"] != float64(0) || third["evidence_written"] != float64(1) {
			t.Fatalf("evidence_written = %v %v %v", first["evidence_written"], second["evidence_written"], third["evidence_written"])
		}
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, first["id"]))
		if r.IsError {
			t.Fatalf("get_edge: %s", r.Text)
		}
		st := out["statement"].(map[string]any)
		ev := st["evidence"].([]any)
		if len(ev) != 2 || st["evidence_count"] != float64(2) {
			t.Fatalf("evidence = %v", st)
		}
		row := ev[0].(map[string]any)
		if row["quote"] != "comply with Article 12" || row["char_start"] != float64(19) || row["char_end"] != float64(41) {
			t.Fatalf("span = %v", row)
		}
	})
}

// The cap keeps the oldest rows and says so.
func TestLinkChunks_EvidenceCapKeepsTheOldest(t *testing.T) {
	t.Setenv("LOOMCYCLE_DOCUMENT_EVIDENCE_MAX_PER_EDGE", "1")
	d, ctx, _ := documentFixture(t)
	_, a, b, _ := stmtDoc(t, d, ctx, "cap")
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
		"evidence":[{"chunk_id":%q,"quote":"The licensee"},{"chunk_id":%q,"quote":"Article 12"}]}`, a, b, a, a))
	if r.IsError {
		t.Fatalf("link: %s", r.Text)
	}
	if out["evidence_written"] != float64(1) || out["evidence_capped"] != true {
		t.Fatalf("cap = %v", out)
	}
}

// The review lifecycle: a proposal is visible to get_edges and the queue, not to
// backlinks or the walk; only an operator off-run can rule on it; a rejection is a
// tombstone nobody but the operator can overwrite.
func TestProposeStatement_ReviewLifecycle(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docID, a, b, _ := stmtDoc(t, d, ctx, "review")
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"implements",
			"confidence":0.8,"evidence":[{"chunk_id":%q,"quote":"Article 12 of the Act"}]}`, a, b, a))
		if r.IsError || out["status"] != "proposed" {
			t.Fatalf("propose: %v %s", out, r.Text)
		}
		id := out["id"].(string)
		if e := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q}`, docID)); len(e) != 1 || e[0]["status"] != "proposed" {
			t.Fatalf("get_edges should show the proposal: %v", e)
		}
		if bl := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"backlinks","scope":"user","id":%q}`, b)); len(bl) != 0 {
			t.Fatalf("backlinks should leave a proposal out: %v", bl)
		}
		q, _ := docExec(t, d, ctx, `{"op":"list_statements","scope":"user"}`)
		if l, _ := q["statements"].([]any); len(l) != 1 {
			t.Fatalf("queue = %v", q)
		}

		// Neither an agent in a run nor an off-run caller without the operator
		// ceiling may rule.
		for name, c := range map[string]context.Context{"in run": inRunCtx(operatorCtx(ctx)), "no ceiling": ctx} {
			if _, r := docExec(t, d, c, fmt.Sprintf(`{"op":"accept_statement","scope":"user","id":%q}`, id)); !r.IsError {
				t.Errorf("%s: accept_statement was allowed", name)
			}
		}
		op := operatorCtx(ctx)
		if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"reject_statement","scope":"user","id":%q}`, id)); !r.IsError {
			t.Errorf("reject without a reason was allowed")
		}
		if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"reject_statement","scope":"user","id":%q,"reason":"wrong act"}`, id)); r.IsError {
			t.Fatalf("reject: %s", r.Text)
		}
		if e := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q}`, docID)); len(e) != 0 {
			t.Fatalf("a rejected statement is still listed: %v", e)
		}
		if e := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q,"include_refuted":true}`, docID)); len(e) != 1 || e[0]["status"] != "rejected" {
			t.Fatalf("include_refuted should show the tombstone: %v", e)
		}
		for _, o := range []string{"propose_statement", "link_chunks"} {
			_, r := docExec(t, d, inRunCtx(ctx), fmt.Sprintf(`{"op":%q,"scope":"user","from_id":%q,"to_id":%q,"kind":"implements"}`, o, a, b))
			if !r.IsError || !strings.Contains(r.Text, "statement_rejected") {
				t.Errorf("%s over a rejection: want statement_rejected, got %s", o, r.Text)
			}
		}
		// The operator may re-assert it.
		if out, r := docExec(t, d, op, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"implements"}`, a, b)); r.IsError || out["status"] != "asserted" || out["id"] != id {
			t.Fatalf("operator link over a rejection: %v %s", out, r.Text)
		}
	})
}

// A proposal with id revises an existing statement: filed beside it, never on top.
func TestProposeStatement_RevisionSitsBesideTheOriginal(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID, a, b, _ := stmtDoc(t, d, ctx, "revise")
	orig, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))
	rev, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","id":%q,"kind":"implements"}`, orig["id"]))
	if r.IsError {
		t.Fatalf("revise: %s", r.Text)
	}
	if rev["id"] == orig["id"] {
		t.Fatalf("the revision overwrote the original")
	}
	edges := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q}`, docID))
	if len(edges) != 2 {
		t.Fatalf("edges = %v", edges)
	}
	for _, e := range edges {
		if e["id"] == rev["id"] {
			attrs, _ := e["attributes"].(map[string]any)
			if attrs["revision_of"] != orig["id"] || e["status"] != "proposed" {
				t.Fatalf("revision = %v", e)
			}
		}
	}
}

// judge_fact takes a statement id: unsupported withholds it from every reader
// (never deletes it), and a run cannot vouch for a statement with no evidence.
func TestJudgeFact_OnAStatement(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docID, a, b, c := stmtDoc(t, d, ctx, "judge")
		bare, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, c))
		if _, r := docExec(t, d, inRunCtx(ctx), fmt.Sprintf(`{"op":"judge_fact","scope":"user","id":%q,"verdict":"supported","reason":"looks right"}`, bare["id"])); !r.IsError {
			t.Errorf("a run vouched for a statement with no evidence")
		}
		withEv, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
			"evidence":[{"chunk_id":%q,"quote":"Article 12"}]}`, a, b, a))
		out, r := docExec(t, d, inRunCtx(ctx), fmt.Sprintf(`{"op":"judge_fact","scope":"user","id":%q,"verdict":"unsupported","reason":"the clause cites another article"}`, withEv["id"]))
		if r.IsError || out["withheld"] != true {
			t.Fatalf("judge: %v %s", out, r.Text)
		}
		edges := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q}`, docID))
		if len(edges) != 1 || edges[0]["id"] != bare["id"] {
			t.Fatalf("the refuted statement is still listed: %v", edges)
		}
		edges = edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q,"include_refuted":true}`, docID))
		if len(edges) != 2 {
			t.Fatalf("include_refuted lost the statement: %v", edges)
		}
	})
}

// graph_recall walks only what a reader is shown: not a proposal, not a refuted
// statement.
func TestGraphRecall_DoesNotWalkAProposal(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, a, b, c := stmtDoc(t, d, ctx, "walk")
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b)); r.IsError {
		t.Fatal(r.Text)
	}
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, c)); r.IsError {
		t.Fatal(r.Text)
	}
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],"hops":1}`, a))
	if r.IsError {
		t.Fatalf("graph_recall: %s", r.Text)
	}
	b2, _ := json.Marshal(out)
	if !strings.Contains(string(b2), b) {
		t.Fatalf("the asserted neighbour is missing: %s", b2)
	}
	if strings.Contains(string(b2), c) {
		t.Fatalf("the walk followed a proposal: %s", b2)
	}
}

// get_edges pages: limit + after reach every statement exactly once.
func TestGetEdges_PagesWithACursor(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID, a, _, _ := stmtDoc(t, d, ctx, "pages")
	targets := map[string]bool{}
	for i := 0; i < 5; i++ {
		o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"t%d","body":"x"}`, docID, i))
		if r.IsError {
			t.Fatal(r.Text)
		}
		id := o["id"].(string)
		targets[id] = true
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, a, id)); r.IsError {
			t.Fatal(r.Text)
		}
	}
	seen := map[string]int{}
	after := ""
	for page := 0; page < 10; page++ {
		req := fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q,"limit":2,"after":%q}`, docID, after)
		out, r := docExec(t, d, ctx, req)
		if r.IsError {
			t.Fatal(r.Text)
		}
		for _, e := range out["edges"].([]any) {
			seen[e.(map[string]any)["to_id"].(string)]++
		}
		next, _ := out["next_cursor"].(string)
		if next == "" {
			break
		}
		after = next
	}
	for id := range targets {
		if seen[id] != 1 {
			t.Fatalf("target %s seen %d times (%v)", id, seen[id], seen)
		}
	}
}

// unlink by id takes the evidence with it; deleting a chunk does the same.
func TestUnlinkChunks_RemovesEvidence(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, a, b, _ := stmtDoc(t, d, ctx, "unlink")
	out, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
		"evidence":[{"chunk_id":%q,"quote":"Article 12"}]}`, a, b, a))
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"unlink_chunks","scope":"user","id":%q}`, out["id"])); r.IsError {
		t.Fatal(r.Text)
	}
	key, _, _ := d.resolveScope(ctx, "user")
	res, err := d.query(ctx, key, `SELECT COUNT(*) FROM chunk_edge_evidence`)
	if err != nil || asInt(res.Rows[0][0]) != 0 {
		t.Fatalf("evidence survived its statement: %v %v", res, err)
	}
	out, _ = docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
		"evidence":[{"chunk_id":%q,"quote":"Article 12"}]}`, a, b, a))
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"delete_chunk","scope":"user","id":%q}`, b)); r.IsError {
		t.Fatal(r.Text)
	}
	res, err = d.query(ctx, key, `SELECT COUNT(*) FROM chunk_edge_evidence`)
	if err != nil || asInt(res.Rows[0][0]) != 0 {
		t.Fatalf("evidence survived a deleted endpoint: %v %v", res, err)
	}
}

// export_md → import_md keeps a statement's envelope and evidence, and an inbound
// statement from a fact outside the document comes back by its natural key.
func TestMarkdownRoundTrip_KeepsStatements(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID, a, b, _ := stmtDoc(t, d, ctx, "roundtrip")
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","valid_at":1000,
		"attributes":{"aspect":"reporting"},"evidence":[{"chunk_id":%q,"quote":"Article 12 of the Act"}]}`, a, b, a)); r.IsError {
		t.Fatal(r.Text)
	}
	factsDoc, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"facts"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	fact := upsert(t, d, ctx, factsDoc["document_id"].(string), "reg|duty|report", "Reporting duty", "the duty to report", "")
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"based_on"}`, fact, b)); r.IsError {
		t.Fatal(r.Text)
	}
	exp, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"export_md","scope":"user","document_id":%q}`, docID))
	if r.IsError {
		t.Fatal(r.Text)
	}
	md := exp["markdown"].(string)
	if !strings.Contains(md, "@reg|duty|report -> "+b+" [based_on]") {
		t.Fatalf("inbound statement not exported:\n%s", md)
	}
	imp, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"import_md","scope":"user","markdown":%q}`, md))
	if r.IsError {
		t.Fatal(r.Text)
	}
	edges := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q}`, imp["document_id"]))
	var cites, based map[string]any
	for _, e := range edges {
		switch e["kind"] {
		case "cites":
			cites = e
		case "based_on":
			based = e
		}
	}
	if cites == nil || based == nil {
		t.Fatalf("round trip lost statements: %v", edges)
	}
	if cites["method"] != "import" || cites["valid_at"] != float64(1000) || cites["evidence_count"] != float64(1) {
		t.Fatalf("envelope lost: %v", cites)
	}
	if attrs, _ := cites["attributes"].(map[string]any); attrs["aspect"] != "reporting" {
		t.Fatalf("attributes lost: %v", cites)
	}
	if based["from_id"] != fact {
		t.Fatalf("inbound statement reconnected to %v, want %s", based["from_id"], fact)
	}
}

// The dead-link sweep collects evidence whose statement is gone.
func TestDeadLink_CollectsOrphanEvidence(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, a, b, _ := stmtDoc(t, d, ctx, "orphan")
	out, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
		"evidence":[{"chunk_id":%q,"quote":"Article 12"}]}`, a, b, a))
	key, mscope, _ := d.resolveScope(ctx, "user")
	// An older path that deleted the statement and not its evidence.
	if err := d.exec(ctx, key, `DELETE FROM chunk_edges WHERE id = ?`, out["id"]); err != nil {
		t.Fatal(err)
	}
	rep, err := d.ReconcileDeadLinks(ctx, key, mscope, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Evidence != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

// A scope created before statements: its edges read back identically — same
// endpoints, kinds and auto flags, each with an id, asserted, manual or wikilink —
// and a second ensure is a no-op.
func TestMigrateEdgeStatements_PreExistingEdgesReadIdentically(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key, _, err := d.resolveScope(ctx, "user")
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UnixNano()
		for _, s := range []string{
			`CREATE TABLE chunk_edges (from_id TEXT NOT NULL, to_id TEXT NOT NULL, kind TEXT NOT NULL, created_at BIGINT NOT NULL, auto INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (from_id, to_id, kind))`,
			`CREATE INDEX chunk_edges_to_kind ON chunk_edges(to_id, kind)`,
		} {
			if err := d.exec(ctx, key, s); err != nil {
				t.Fatalf("stage old shape: %v", err)
			}
		}
		for _, e := range []struct {
			from, to, kind string
			auto           int
		}{{"a", "b", "cites", 0}, {"a", "c", "references", 1}, {"b", "c", "blocks", 0}} {
			if err := d.exec(ctx, key, `INSERT INTO chunk_edges (from_id, to_id, kind, created_at, auto) VALUES (?, ?, ?, ?, ?)`, e.from, e.to, e.kind, now, e.auto); err != nil {
				t.Fatal(err)
			}
		}
		if !d.tableHasColumn(ctx, key, "chunk_edges", "auto") || d.tableHasColumn(ctx, key, "chunk_edges", "discriminator") {
			t.Fatalf("staging invalid: the table is not the pre-statement shape")
		}
		for i := 0; i < 2; i++ {
			d.SqlMem.ForgetEnsured(key)
			if err := d.ensureSchema(ctx, key); err != nil {
				t.Fatalf("ensureSchema %d: %v", i, err)
			}
		}
		res, err := d.query(ctx, key, `SELECT id, from_id, to_id, kind, auto, status, method FROM chunk_edges ORDER BY from_id, to_id`)
		if err != nil {
			t.Fatal(err)
		}
		want := [][]string{{"a", "b", "cites", "0", "asserted", "manual"}, {"a", "c", "references", "1", "asserted", "wikilink"}, {"b", "c", "blocks", "0", "asserted", "manual"}}
		if len(res.Rows) != len(want) {
			t.Fatalf("rows = %v", res.Rows)
		}
		ids := map[string]bool{}
		for i, r := range res.Rows {
			got := []string{asStr(r[1]), asStr(r[2]), asStr(r[3]), fmt.Sprint(asInt(r[4])), asStr(r[5]), asStr(r[6])}
			if strings.Join(got, ",") != strings.Join(want[i], ",") {
				t.Errorf("row %d = %v, want %v", i, got, want[i])
			}
			if id := asStr(r[0]); len(id) != 32 || ids[id] {
				t.Errorf("row %d id = %q (want a fresh 32-hex id)", i, id)
			} else {
				ids[id] = true
			}
		}
		// The unique statement key holds on the rebuilt table.
		if err := d.exec(ctx, key, `INSERT INTO chunk_edges (id, from_id, to_id, kind, created_at) VALUES (?, 'a', 'b', 'cites', ?)`, newDocID(), now); err == nil {
			t.Errorf("a duplicate undated statement was accepted on the rebuilt table")
		}
	})
}

func firstDocOf(t *testing.T, d *Document, ctx context.Context, chunkID string) string {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, chunkID))
	if r.IsError {
		t.Fatal(r.Text)
	}
	return out["document_id"].(string)
}

// upsertIn writes a fact into a document of the default user scope (any tier).
func upsertIn(t *testing.T, d *Document, ctx context.Context, docID, key, body string) string {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":%q,"title":%q,"body":%q}`, docID, key, body, body))
	if r.IsError {
		t.Fatalf("upsert %s: %s", key, r.Text)
	}
	return out["id"].(string)
}
