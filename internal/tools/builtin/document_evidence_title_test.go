package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Statement evidence may quote a chunk's TITLE (part:"title") as well as its
// body. A document's title lives on its root chunk's title and in no body, so
// without this a relation resting on a title — find_duplicates' duplicate_of —
// had nothing quotable. Every fixture here is invented.

// getEdgeEvidence reads a statement's evidence rows through get_edge.
func getEdgeEvidence(t *testing.T, d *Document, ctx context.Context, scope, id string) []map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":%q,"id":%q}`, scope, id))
	if r.IsError {
		t.Fatalf("get_edge: %s", r.Text)
	}
	st, _ := out["statement"].(map[string]any)
	raw, _ := st["evidence"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		rows = append(rows, e.(map[string]any))
	}
	return rows
}

// A quote of the chunk's title is evidence only when it says so: without part
// (or with part body) it is looked for in the body and refused; with part title
// it is verified against the title, spanned in characters of the title, and read
// back with its part. A body row keeps the old shape (no part key).
func TestEvidence_TitleQuoteIsAcceptedOnlyAsPartTitle(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docID, a, b, c := stmtDoc(t, d, ctx, "title evidence")
		for _, part := range []string{``, `,"part":"body"`} {
			_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
				"evidence":[{"chunk_id":%q,"quote":"Clause 4"%s}]}`, a, b, a, part))
			if !r.IsError || !strings.Contains(r.Text, "evidence_quote_not_found") {
				t.Fatalf("a title quote as a body quote (%q): want evidence_quote_not_found, got %s", part, r.Text)
			}
			if r.Error == nil || !strings.Contains(r.Error.Description, `part "title"`) {
				t.Errorf("the refusal should point at part title, since the quote is the chunk's title: %s", r.Text)
			}
		}
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
			"evidence":[{"chunk_id":%q,"quote":"Clause 4","part":"title"},{"chunk_id":%q,"quote":"comply with Article 12"}]}`, a, b, a, a))
		if r.IsError || out["evidence_written"] != float64(2) {
			t.Fatalf("title + body evidence: %v %s", out, r.Text)
		}
		rows := getEdgeEvidence(t, d, ctx, "user", out["id"].(string))
		if len(rows) != 2 {
			t.Fatalf("evidence = %v", rows)
		}
		var title, body map[string]any
		for _, row := range rows {
			if row["quote"] == "Clause 4" {
				title = row
			} else {
				body = row
			}
		}
		if title == nil || title["part"] != "title" || title["char_start"] != float64(0) || title["char_end"] != float64(8) || title["chunk_id"] != a {
			t.Errorf("title row = %v, want part title spanning [0,8) of the title", title)
		}
		if _, has := body["part"]; has || body["char_start"] != float64(19) {
			t.Errorf("body row = %v, want the old shape (no part key) and its body span", body)
		}

		// The same quote and chunk as a title row and as a body row are two
		// citations, not a duplicate: "Article 12" is b's title and in b's body.
		out2, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
			"evidence":[{"chunk_id":%q,"quote":"Article 12"},{"chunk_id":%q,"quote":"Article 12","part":"title"}]}`, b, c, b, b))
		if r.IsError || out2["evidence_written"] != float64(2) {
			t.Fatalf("body + title of the same text: %v %s", out2, r.Text)
		}

		// A proposal's preview (get_edges, list_statements) carries the part too.
		p, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"implements",
			"evidence":[{"chunk_id":%q,"quote":"Notes","part":"title"}]}`, c, a, c))
		if r.IsError {
			t.Fatalf("propose: %s", r.Text)
		}
		q, _ := docExec(t, d, ctx, `{"op":"list_statements","scope":"user"}`)
		found := false
		for _, s := range q["statements"].([]any) {
			sm := s.(map[string]any)
			if sm["id"] != p["id"] {
				continue
			}
			ev, _ := sm["evidence"].([]any)
			found = len(ev) == 1 && ev[0].(map[string]any)["part"] == "title"
		}
		if !found {
			t.Errorf("list_statements should show the proposal's title evidence with its part: %v", q)
		}
		for _, e := range edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q}`, docID)) {
			if e["id"] != out["id"] {
				continue
			}
			ok := false
			for _, ev := range e["evidence"].([]any) {
				if ev.(map[string]any)["part"] == "title" {
					ok = true
				}
			}
			if !ok {
				t.Errorf("get_edges should show the title row's part: %v", e)
			}
		}
	})
}

// A title quote is checked as strictly as a body quote: a misquote, a quote of
// the body passed as the title, a span that is not where the quote is, an
// unknown part and a part on a source that has no title are all refused.
func TestEvidence_MisquotedTitleIsRefused(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, a, b, _ := stmtDoc(t, d, ctx, "misquoted title")
		link := func(ev string) string {
			_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","evidence":[%s]}`, a, b, ev))
			if !r.IsError {
				t.Errorf("evidence %s was accepted", ev)
			}
			return r.Text
		}
		for _, ev := range []string{
			fmt.Sprintf(`{"chunk_id":%q,"quote":"Clause 5","part":"title"}`, a),
			fmt.Sprintf(`{"chunk_id":%q,"quote":"The licensee","part":"title"}`, a),
			fmt.Sprintf(`{"chunk_id":%q,"quote":"Clause 4","part":"title","char_start":1}`, a),
		} {
			if txt := link(ev); !strings.Contains(txt, "evidence_quote_not_found") {
				t.Errorf("%s: want evidence_quote_not_found, got %s", ev, txt)
			}
		}
		if txt := link(fmt.Sprintf(`{"chunk_id":%q,"quote":"Clause 4","part":"heading"}`, a)); !strings.Contains(txt, "part must be") {
			t.Errorf("an unknown part: got %s", txt)
		}
		if txt := link(`{"scope":"tenant","natural_key":"k","quote":"Clause 4","part":"title"}`); !strings.Contains(txt, "part") {
			t.Errorf("part on a cross-scope citation: got %s", txt)
		}
		// The span inside the title is in characters of the title.
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
			"evidence":[{"chunk_id":%q,"quote":"4","part":"title","char_start":7,"char_end":8}]}`, a, b, a)); r.IsError {
			t.Errorf("a correct span inside the title was refused: %s", r.Text)
		}
	})
}

// relocate_evidence keeps a row's part: a title quote is looked for in the new
// chunk's title, not its body, and the row stays a title row.
func TestMaintainStatement_RelocateKeepsATitleRowsPart(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newMaintFixture(t, d, ctx)
	key, mscope, _ := d.resolveScope(ctx, "user")
	// b's title is "Article 12"; a's body holds "Article 12" too.
	res, refuse, err := d.writeStatement(ctx, key, mscope, edgeWrite{from: f.a, to: f.c, kind: "cites", method: "rule:maintain", status: edgeStatusProposed,
		evidence: []edgeEvidenceIn{{ChunkID: f.b, Quote: "Article 12", Part: "title"}}})
	if err != nil || refuse != nil {
		t.Fatalf("seed: %v %v", err, refuse)
	}
	ev := getEdgeEvidence(t, d, ctx, "user", res.ID)
	if len(ev) != 1 || ev[0]["part"] != "title" {
		t.Fatalf("seeded evidence = %v", ev)
	}
	evID := ev[0]["id"].(string)
	// a's TITLE is "Clause 4": the quote is in a's body, not its title, so a title
	// row may not be re-pointed there.
	_, r := f.maintain(t, f.run, res.ID, "relocate_evidence", `,"evidence_id":"`+evID+`","chunk_id":"`+f.a+`","method":"rule:maintain"`)
	wantRefusal(t, "relocating a title quote to a chunk whose body (not title) holds it", r, "evidence_quote_not_found")

	// A chunk titled "Article 12" elsewhere takes it, and the row stays a title row.
	o, r2 := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Article 12","body":"moved"}`, f.doc))
	if r2.IsError {
		t.Fatalf("create_chunk: %s", r2.Text)
	}
	moved := o["id"].(string)
	if _, r := f.maintain(t, f.run, res.ID, "relocate_evidence", `,"evidence_id":"`+evID+`","chunk_id":"`+moved+`","method":"rule:maintain"`); r.IsError {
		t.Fatalf("relocate to a chunk with that title: %s", r.Text)
	}
	ev = getEdgeEvidence(t, d, ctx, "user", res.ID)
	if len(ev) != 1 || ev[0]["chunk_id"] != moved || ev[0]["part"] != "title" || ev[0]["char_start"] != float64(0) || ev[0]["char_end"] != float64(10) {
		t.Errorf("relocated row = %v, want a title row on the new chunk spanning [0,10)", ev)
	}
}

// merge treats a title row and a body row of the same text as two citations.
func TestMaintainStatement_MergeKeepsATitleRowBesideTheSameBodyQuote(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newMaintFixture(t, d, ctx)
	key, mscope, _ := d.resolveScope(ctx, "user")
	seed := func(validAt int64, ev edgeEvidenceIn) string {
		w := edgeWrite{from: f.b, to: f.c, kind: "cites", method: "rule:maintain", status: edgeStatusProposed, evidence: []edgeEvidenceIn{ev}}
		if validAt != 0 {
			w.validAt = &validAt
		}
		res, refuse, err := d.writeStatement(ctx, key, mscope, w)
		if err != nil || refuse != nil {
			t.Fatalf("seed: %v %v", err, refuse)
		}
		return res.ID
	}
	survivor := seed(0, edgeEvidenceIn{ChunkID: f.b, Quote: "Article 12"})
	dup := seed(5, edgeEvidenceIn{ChunkID: f.b, Quote: "Article 12", Part: "title"})
	// Both rows span [0,10): b's body starts with its title's text.
	out, r := f.maintain(t, f.run, dup, "merge", `,"survivor_id":"`+survivor+`","method":"rule:maintain"`)
	if r.IsError {
		t.Fatalf("merge: %s", r.Text)
	}
	if out["evidence_moved"] != float64(1) || out["evidence_duplicate"] != float64(0) {
		t.Errorf("merge = %v, want the title row moved, not counted a duplicate of the body row", out)
	}
	parts := map[any]int{}
	for _, ev := range getEdgeEvidence(t, d, ctx, "user", survivor) {
		parts[ev["part"]]++
	}
	if parts["title"] != 1 || parts[nil] != 1 {
		t.Errorf("survivor evidence parts = %v, want one body and one title row", parts)
	}
}

// A copy and a move carry an evidence row's part.
func TestCopyDocuments_CarriesATitleEvidenceRowsPart(t *testing.T) {
	for _, move := range []bool{false, true} {
		t.Run(fmt.Sprintf("move=%v", move), func(t *testing.T) {
			d, ctx, _ := documentFixture(t)
			ctx = grantedTenantCtx(ctx)
			s := seedCopyDocs(t, d, ctx)
			if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","from_id":"`+s.planChunk+`","to_id":"`+s.guideChunk+`","kind":"cites",
				"evidence":[{"chunk_id":"`+s.planChunk+`","quote":"Plan part","part":"title"}]}`); r.IsError {
				t.Fatalf("link: %s", r.Text)
			}
			plan := s.planDoc
			if move {
				moveSet(t, d, ctx, `"under_path":"/proj"`)
			} else {
				plan = copiedID(t, copyExec(t, d, ctx, `{"op":"copy_documents","scope":"user","to_scope":"tenant","under_path":"/proj","dry_run":false}`), s.planDoc)
			}
			edges := copyExec(t, d, ctx, `{"op":"get_edges","scope":"tenant","document_id":"`+plan+`","direction":"out"}`)
			id := ""
			for _, e := range edges["edges"].([]any) {
				if em := e.(map[string]any); em["kind"] == "cites" {
					id = asStr(em["id"])
				}
			}
			if id == "" {
				t.Fatalf("the cites relation did not come along: %v", edges)
			}
			ev := getEdgeEvidence(t, d, ctx, "tenant", id)
			if len(ev) != 1 || ev[0]["part"] != "title" || ev[0]["quote"] != "Plan part" || ev[0]["char_end"] != float64(9) {
				t.Errorf("carried evidence = %v, want the title row with its part", ev)
			}
		})
	}
}

// A reimport that rewrites a part's body but keeps its title carries a statement
// whose evidence quotes the title: the quote still holds, against the new
// version's title, so the statement is copied, not sent back to review.
func TestReimport_TitleEvidenceHoldsAgainstTheNewVersionsTitle(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key, mscope, err := d.resolveScope(ctx, "user")
		if err != nil {
			t.Fatal(err)
		}
		docID, v1 := rulingFixture(t, d, ctx, "law:xx/title-1")
		res, refuse, err := d.writeStatement(ctx, key, mscope, edgeWrite{
			from: v1["article-1"].id, to: v1["article-2"].id, kind: "cites", method: "rule:citation", status: edgeStatusAsserted,
			evidence: []edgeEvidenceIn{{ChunkID: v1["article-1"].id, Quote: "Article 1", Part: "title"}},
		})
		if err != nil || refuse != nil {
			t.Fatalf("seed: %v %v", err, refuse)
		}
		rep := reimport(t, d, ctx, rulingEd2, "law:xx/title-1", 2_000_000)["reimport"].(map[string]any)
		if rep["statements_copied"] != float64(1) || rep["statements_proposed"] != float64(0) {
			t.Errorf("report = %v, want the statement copied and none sent to review", rep)
		}
		if st := evidenceStale(t, d, ctx, key, res.ID); len(st) != 1 || st[0] {
			t.Errorf("the original's title evidence was marked stale: %v", st)
		}
		v2 := currentPartsOf(t, d, ctx, docID, 2_000_001)
		copies := copiesOf(t, d, ctx, key, res.ID)
		if len(copies) != 1 {
			t.Fatalf("copies = %d, want 1", len(copies))
		}
		ev := getEdgeEvidence(t, d, ctx, "user", copies[0].ID)
		if len(ev) != 1 || ev[0]["chunk_id"] != v2["article-1"].id || ev[0]["part"] != "title" || ev[0]["stale"] == true {
			t.Errorf("the copy's evidence = %v, want a live title row on the new Article 1", ev)
		}
	})
}

// A sync carries a title row both ways with its part: on a push the peer checks
// it against its own chunk's title, on a pull this side does. A keyed chunk's
// title here is its natural key (upsertKeyed).
func TestDocumentSync_CarriesATitleEvidenceRowsPart(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		titleRow := func(s map[string]any) map[string]any {
			for _, e := range s["evidence"].([]any) {
				if em := e.(map[string]any); em["part"] == "title" {
					return em
				}
			}
			return nil
		}

		// push
		localDoc, pd, pctx, remoteDoc := carryPair(t, d, ctx)
		a := upsertKeyed(t, d, ctx, localDoc, "nkA", "The licensee shall report every incident.")
		b := upsertKeyed(t, d, ctx, localDoc, "nkB", "Article B sets the reporting duty.")
		linkJSON(t, d, operatorCtx(ctx), fmt.Sprintf(`{"op":"link_chunks","from_id":%q,"to_id":%q,"kind":"cites",
			"evidence":[{"chunk_id":%q,"quote":"nkA","part":"title"}]}`, a, b, a))
		if rep := syncReport(t, d, ctx, localDoc, "push"); rep["edges_added"] != float64(1) || rep["evidence_dropped"] != nil {
			t.Errorf("push report = %v, want the statement and its title row carried", rep)
		}
		pkey, _, _ := pd.resolveScope(pctx, "user")
		peerA, _ := pd.chunkIDByNaturalKey(pctx, pkey, "nkA")
		if row := titleRow(statementsOfKind(t, pd, pctx, remoteDoc, "cites", 1)[0]); row == nil || row["chunk_id"] != peerA || row["quote"] != "nkA" {
			t.Errorf("pushed title row = %v, want it on the peer's nkA (%s)", row, peerA)
		}

		// pull: a fresh pair, the title row written on the peer.
		localDoc2, pd2, pctx2, remoteDoc2 := carryPair(t, d, ctx)
		pa := upsertKeyed(t, pd2, pctx2, remoteDoc2, "nkC", "Clause C text.")
		pb := upsertKeyed(t, pd2, pctx2, remoteDoc2, "nkD", "Clause D text.")
		linkJSON(t, pd2, operatorCtx(pctx2), fmt.Sprintf(`{"op":"link_chunks","from_id":%q,"to_id":%q,"kind":"cites",
			"evidence":[{"chunk_id":%q,"quote":"nkC","part":"title"}]}`, pa, pb, pa))
		if rep := syncReport(t, d, ctx, localDoc2, "pull"); rep["edges_added"] != float64(1) || rep["evidence_dropped"] != nil {
			t.Errorf("pull report = %v, want the statement and its title row carried", rep)
		}
		key, _, _ := d.resolveScope(ctx, "user")
		localC, _ := d.chunkIDByNaturalKey(ctx, key, "nkC")
		if row := titleRow(statementsOfKind(t, d, ctx, localDoc2, "cites", 1)[0]); row == nil || row["chunk_id"] != localC {
			t.Errorf("pulled title row = %v, want it on this side's nkC (%s)", row, localC)
		}
	})
}

// A scope whose evidence table predates parts gains the column on its next
// provisioning, and the rows it already held read back as body rows — quotes
// of the body, which is what every one of them is.
func TestMigrateEvidencePart_ReachesAnExistingScope(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, a, b, _ := stmtDoc(t, d, ctx, "pre-part scope")
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
			"evidence":[{"chunk_id":%q,"quote":"comply with Article 12"}]}`, a, b, a))
		if r.IsError {
			t.Fatalf("link: %s", r.Text)
		}
		key, _, err := d.resolveScope(ctx, "user")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.docSQL().Exec(ctx, key, `ALTER TABLE chunk_edge_evidence DROP COLUMN part`, nil, 0); err != nil {
			t.Fatalf("rewind: %v", err)
		}
		if d.tableHasColumn(ctx, key, "chunk_edge_evidence", "part") {
			t.Fatal("rewind did not take — the test would pass against the unmigrated code")
		}
		d.docSQL().ForgetEnsured(key)
		if err := d.ensureSchema(ctx, key); err != nil {
			t.Fatalf("ensureSchema: %v", err)
		}
		if !d.tableHasColumn(ctx, key, "chunk_edge_evidence", "part") {
			t.Fatal("part absent after ensureSchema — an existing scope never gains the column")
		}
		ev := getEdgeEvidence(t, d, ctx, "user", out["id"].(string))
		if len(ev) != 1 || ev[0]["quote"] != "comply with Article 12" {
			t.Fatalf("evidence after the migration = %v", ev)
		}
		if _, has := ev[0]["part"]; has {
			t.Errorf("an old row reads back with a part: %v", ev[0])
		}
	})
}
