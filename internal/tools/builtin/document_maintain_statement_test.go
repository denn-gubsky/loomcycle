package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// maintFixture is a scope with three chunks (stmtDoc) and the ctx of a code-js
// run holding the rule grant.
type maintFixture struct {
	d          *Document
	ctx, run   context.Context
	doc        string
	a, b, c    string
	quoteInA   string // occurs in a and in b
	quoteOnlyA string // occurs only in a
}

func newMaintFixture(t *testing.T, d *Document, ctx context.Context) maintFixture {
	t.Helper()
	f := maintFixture{d: d, ctx: ctx, run: statementMethodRun(ctx, "rule"), quoteInA: "Article 12", quoteOnlyA: "The licensee shall comply"}
	f.doc, f.a, f.b, f.c = stmtDoc(t, d, ctx, "Licence")
	return f
}

// seed writes a statement directly with the given method and status — how
// sync, import or a code-js run's propose would have left it.
func (f maintFixture) seed(t *testing.T, from, to, kind, method, status string, validAt int64, quotes ...string) string {
	t.Helper()
	key, mscope, err := f.d.resolveScope(f.ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	w := edgeWrite{from: from, to: to, kind: kind, method: method, status: status}
	if validAt != 0 {
		w.validAt = &validAt
	}
	for _, q := range quotes {
		w.evidence = append(w.evidence, edgeEvidenceIn{ChunkID: f.a, Quote: q})
	}
	res, refuse, err := f.d.writeStatement(f.ctx, key, mscope, w)
	if err != nil || refuse != nil {
		t.Fatalf("seed %s: %v %v", kind, err, refuse)
	}
	return res.ID
}

func (f maintFixture) maintain(t *testing.T, ctx context.Context, id, action, extra string) (map[string]any, tools.Result) {
	t.Helper()
	return docExec(t, f.d, ctx, fmt.Sprintf(`{"op":"maintain_statement","scope":"user","id":%q,"action":%q%s}`, id, action, extra))
}

// row reads the stored statement columns maintain_statement changes.
func (f maintFixture) row(t *testing.T, id string) statementRow {
	t.Helper()
	key, _, _ := f.d.resolveScope(f.ctx, "user")
	s, err := f.d.statementByID(f.ctx, key, id)
	if err != nil || s == nil {
		t.Fatalf("statement %s: %v", id, err)
	}
	return *s
}

type evRow struct {
	id, edge, chunk string
	start, end      int
	stale           bool
}

func (f maintFixture) evidence(t *testing.T, edgeID string) []evRow {
	t.Helper()
	key, _, _ := f.d.resolveScope(f.ctx, "user")
	res, err := f.d.query(f.ctx, key, `SELECT id, edge_id, COALESCE(chunk_id, ''), COALESCE(char_start, -1), COALESCE(char_end, -1), stale
		FROM chunk_edge_evidence WHERE edge_id = ? ORDER BY created_at, id`, edgeID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]evRow, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, evRow{id: asStr(r[0]), edge: asStr(r[1]), chunk: asStr(r[2]), start: asInt(r[3]), end: asInt(r[4]), stale: asInt(r[5]) == 1})
	}
	return out
}

func (f maintFixture) count(t *testing.T, table string) int {
	t.Helper()
	key, _, _ := f.d.resolveScope(f.ctx, "user")
	res, err := f.d.query(f.ctx, key, `SELECT COUNT(*) FROM `+table)
	if err != nil {
		t.Fatal(err)
	}
	return asInt(res.Rows[0][0])
}

func wantRefusal(t *testing.T, what string, r tools.Result, code string) {
	t.Helper()
	if !r.IsError || !strings.Contains(r.Text, code) {
		t.Errorf("%s: want a refusal naming %s, got IsError=%v %s", what, code, r.IsError, r.Text)
	}
}

// verify stamps verified_at and nothing else: not the confidence, not the
// status, not the verdict.
func TestMaintainStatement_VerifyStampsOnlyVerifiedAt(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		f := newMaintFixture(t, d, ctx)
		id := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 0, f.quoteInA)
		key, _, _ := d.resolveScope(ctx, "user")
		if err := d.exec(ctx, key, `UPDATE chunk_edges SET confidence = 0.7 WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		out, r := f.maintain(t, f.run, id, "verify", `,"method":"rule:maintain"`)
		if r.IsError || out["ok"] != true || out["id"] != id || out["action"] != "verify" {
			t.Fatalf("verify = %v %s", out, r.Text)
		}
		s := f.row(t, id)
		if s.VerifiedAt == nil || s.Status != edgeStatusAsserted || s.Confidence == nil || *s.Confidence != 0.7 ||
			s.ExpiredAt != nil || s.JudgedAt != nil || s.VerdictConfidence != nil {
			t.Errorf("after verify: %+v", s)
		}
	})
}

// expire retires the statement with the reason and the stamped method as its
// judge; a reader no longer sees it unless it asks for retired statements.
func TestMaintainStatement_ExpireRetiresWithReasonAndMethod(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		f := newMaintFixture(t, d, ctx)
		id := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 0, f.quoteInA)
		if _, r := f.maintain(t, f.run, id, "expire", `,"reason":"the cited part is gone","method":"rule:maintain"`); r.IsError {
			t.Fatalf("expire: %s", r.Text)
		}
		s := f.row(t, id)
		if s.ExpiredAt == nil || s.JudgeReason != "the cited part is gone" || s.JudgedBy != "rule:maintain" || s.Status != edgeStatusAsserted {
			t.Errorf("after expire: %+v", s)
		}
		if edges := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q}`, f.doc)); len(edges) != 0 {
			t.Errorf("an expired statement is still read: %v", edges)
		}
		if edges := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q,"include_retired":true}`, f.doc)); len(edges) != 1 {
			t.Errorf("include_retired lost the expired statement: %v", edges)
		}
		if f.count(t, "chunk_edges") != 1 || f.count(t, "chunk_edge_evidence") != 1 {
			t.Error("expire deleted something")
		}
	})
}

func TestMaintainStatement_ExpireWithoutAReasonIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newMaintFixture(t, d, ctx)
	id := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 0, f.quoteInA)
	_, r := f.maintain(t, f.run, id, "expire", `,"reason":"  ","method":"rule:maintain"`)
	wantRefusal(t, "expire without a reason", r, "reason")
	if f.row(t, id).ExpiredAt != nil {
		t.Error("a refused expire retired the statement")
	}
}

// demote moves an asserted statement back to review, recording why; one that is
// not asserted is not demoted.
func TestMaintainStatement_DemoteMovesAssertedToProposed(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		f := newMaintFixture(t, d, ctx)
		id := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 0, f.quoteInA)
		if _, r := f.maintain(t, f.run, id, "demote", `,"reason":"kind no longer declared","method":"rule:maintain"`); r.IsError {
			t.Fatalf("demote: %s", r.Text)
		}
		if s := f.row(t, id); s.Status != edgeStatusProposed || s.JudgeReason != "kind no longer declared" || s.ExpiredAt != nil {
			t.Errorf("after demote: %+v", s)
		}
		_, r := f.maintain(t, f.run, id, "demote", `,"reason":"again","method":"rule:maintain"`)
		wantRefusal(t, "demote of a proposal", r, "statement_not_asserted")
		_, r = f.maintain(t, f.run, f.seed(t, f.b, f.c, "cites", "rule:citation", edgeStatusAsserted, 0), "demote", `,"method":"rule:maintain"`)
		wantRefusal(t, "demote without a reason", r, "reason")
	})
}

// A statement a person wrote (method manual) takes only verify and flag, from
// any caller: what a person asserted is not a pass's to retire, demote, merge
// or re-cite.
func TestMaintainStatement_ManualStatementTakesOnlyVerifyAndFlag(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		f := newMaintFixture(t, d, ctx)
		id := f.seed(t, f.a, f.b, "cites", edgeMethodManual, edgeStatusAsserted, 0, f.quoteInA)
		other := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 5, f.quoteInA)
		ev := f.evidence(t, id)[0].id
		for _, c := range []struct{ action, extra string }{
			{"expire", `,"reason":"r"`},
			{"demote", `,"reason":"r"`},
			{"merge", `,"survivor_id":"` + other + `"`},
			{"stale_evidence", `,"evidence_id":"` + ev + `"`},
			{"relocate_evidence", `,"evidence_id":"` + ev + `","chunk_id":"` + f.b + `"`},
		} {
			for name, ctx := range map[string]context.Context{"run": f.run, "operator": operatorCtx(ctx)} {
				_, r := f.maintain(t, ctx, id, c.action, c.extra)
				wantRefusal(t, name+" "+c.action+" on a manual statement", r, "statement_manual")
			}
		}
		if s := f.row(t, id); s.Status != edgeStatusAsserted || s.ExpiredAt != nil || s.JudgedAt != nil {
			t.Errorf("a refused action changed the manual statement: %+v", s)
		}
		if e := f.evidence(t, id); len(e) != 1 || e[0].stale || e[0].chunk != f.a {
			t.Errorf("a refused action changed the manual statement's evidence: %+v", e)
		}
		if _, r := f.maintain(t, f.run, id, "verify", ``); r.IsError {
			t.Errorf("verify on a manual statement: %s", r.Text)
		}
		if _, r := f.maintain(t, f.run, id, "flag", `,"reason":"check this"`); r.IsError {
			t.Errorf("flag on a manual statement: %s", r.Text)
		}
	})
}

// Only manual is limited: a statement that import or sync wrote is expired and
// demoted like a rule's.
func TestMaintainStatement_ImportAndSyncStatementsAreMaintainable(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	f := newMaintFixture(t, d, ctx)
	for i, method := range []string{edgeMethodImport, edgeMethodSync} {
		id := f.seed(t, f.a, f.b, "cites", method, edgeStatusAsserted, int64(i+1), f.quoteInA)
		if _, r := f.maintain(t, f.run, id, "demote", `,"reason":"recheck","method":"rule:maintain"`); r.IsError {
			t.Errorf("demote of a %s statement: %s", method, r.Text)
		}
		if _, r := f.maintain(t, f.run, id, "expire", `,"reason":"gone","method":"rule:maintain"`); r.IsError {
			t.Errorf("expire of a %s statement: %s", method, r.Text)
		}
		if s := f.row(t, id); s.Status != edgeStatusProposed || s.ExpiredAt == nil {
			t.Errorf("%s statement after demote and expire: %+v", method, s)
		}
	}
	manual := f.seed(t, f.a, f.c, "cites", edgeMethodManual, edgeStatusAsserted, 0, f.quoteInA)
	_, r := f.maintain(t, f.run, manual, "demote", `,"reason":"recheck","method":"rule:maintain"`)
	wantRefusal(t, "demote of a manual statement", r, "statement_manual")
	_, r = f.maintain(t, f.run, manual, "expire", `,"reason":"gone","method":"rule:maintain"`)
	wantRefusal(t, "expire of a manual statement", r, "statement_manual")
}

// merge moves the merged statement's evidence onto a survivor of the same
// triple — a row the survivor already holds (same source, quote and span)
// stays where it was, nothing is deleted — and expires the merged statement
// with judge_reason "merged".
func TestMaintainStatement_MergeMovesEvidenceToTheSurvivor(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		f := newMaintFixture(t, d, ctx)
		gone := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 1, f.quoteInA, f.quoteOnlyA)
		keep := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 2, f.quoteInA)
		before := f.count(t, "chunk_edge_evidence")
		out, r := f.maintain(t, f.run, gone, "merge", `,"survivor_id":"`+keep+`","method":"rule:maintain"`)
		if r.IsError {
			t.Fatalf("merge: %s", r.Text)
		}
		if out["evidence_moved"] != float64(1) || out["evidence_duplicate"] != float64(1) {
			t.Errorf("merge counts = %v", out)
		}
		if s := f.row(t, gone); s.ExpiredAt == nil || s.JudgeReason != "merged" || s.JudgedBy != "rule:maintain" {
			t.Errorf("the merged statement: %+v", s)
		}
		if s := f.row(t, keep); s.ExpiredAt != nil {
			t.Errorf("the survivor was retired: %+v", s)
		}
		kept := f.evidence(t, keep)
		if len(kept) != 2 {
			t.Fatalf("survivor evidence = %+v", kept)
		}
		quotes := map[int]bool{}
		for _, e := range kept {
			quotes[e.start] = true
		}
		if len(quotes) != 2 {
			t.Errorf("the survivor holds a duplicate row: %+v", kept)
		}
		if f.count(t, "chunk_edge_evidence") != before || f.count(t, "chunk_edges") != 2 {
			t.Error("merge deleted something")
		}
	})
}

// A survivor must be another live statement of the same from, to and kind.
func TestMaintainStatement_MergeRefusesAMismatchedSurvivor(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		f := newMaintFixture(t, d, ctx)
		id := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 1, f.quoteInA)
		otherKind := f.seed(t, f.a, f.b, "based_on", "rule:citation", edgeStatusAsserted, 0)
		otherEnd := f.seed(t, f.a, f.c, "cites", "rule:citation", edgeStatusAsserted, 0)
		retired := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 3)
		if _, r := f.maintain(t, f.run, retired, "expire", `,"reason":"old","method":"rule:maintain"`); r.IsError {
			t.Fatal(r.Text)
		}
		for name, sv := range map[string]string{"another kind": otherKind, "another end": otherEnd, "itself": id, "retired": retired} {
			_, r := f.maintain(t, f.run, id, "merge", `,"survivor_id":"`+sv+`","method":"rule:maintain"`)
			wantRefusal(t, "merge into "+name, r, "survivor_mismatch")
		}
		_, r := f.maintain(t, f.run, id, "merge", `,"method":"rule:maintain"`)
		wantRefusal(t, "merge without a survivor", r, "survivor_id")
		if s := f.row(t, id); s.ExpiredAt != nil || len(f.evidence(t, id)) != 1 {
			t.Errorf("a refused merge changed the statement: %+v", s)
		}
	})
}

// stale_evidence marks one row of the statement; a row of another statement is
// not reachable through it.
func TestMaintainStatement_StaleEvidenceMarksTheRow(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		f := newMaintFixture(t, d, ctx)
		id := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 0, f.quoteInA, f.quoteOnlyA)
		other := f.seed(t, f.b, f.c, "cites", "rule:citation", edgeStatusAsserted, 0, f.quoteInA)
		rows := f.evidence(t, id)
		if _, r := f.maintain(t, f.run, id, "stale_evidence", `,"evidence_id":"`+rows[0].id+`","method":"rule:maintain"`); r.IsError {
			t.Fatalf("stale_evidence: %s", r.Text)
		}
		after := f.evidence(t, id)
		if !after[0].stale || after[1].stale {
			t.Errorf("after stale_evidence: %+v", after)
		}
		_, r := f.maintain(t, f.run, id, "stale_evidence", `,"evidence_id":"`+f.evidence(t, other)[0].id+`","method":"rule:maintain"`)
		wantRefusal(t, "another statement's evidence", r, "no such evidence")
		if f.evidence(t, other)[0].stale {
			t.Error("another statement's evidence was marked stale")
		}
	})
}

// relocate_evidence re-points a row to the chunk where its quote now is: the
// span is recomputed there (in characters) and the stale mark cleared.
func TestMaintainStatement_RelocateEvidenceRepointsTheRow(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		f := newMaintFixture(t, d, ctx)
		id := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 0, f.quoteInA)
		ev := f.evidence(t, id)[0]
		if _, r := f.maintain(t, f.run, id, "stale_evidence", `,"evidence_id":"`+ev.id+`","method":"rule:maintain"`); r.IsError {
			t.Fatal(r.Text)
		}
		if _, r := f.maintain(t, f.run, id, "relocate_evidence", `,"evidence_id":"`+ev.id+`","chunk_id":"`+f.b+`","method":"rule:maintain"`); r.IsError {
			t.Fatalf("relocate_evidence: %s", r.Text)
		}
		got := f.evidence(t, id)[0]
		// "Article 12 sets the reporting duty." — the quote is its first 10 characters.
		if got.chunk != f.b || got.start != 0 || got.end != 10 || got.stale {
			t.Errorf("after relocate: %+v (was %+v)", got, ev)
		}
	})
}

// A quote absent from chunk_id is refused, and the row stays as it was.
func TestMaintainStatement_RelocateRefusesAQuoteAbsentFromTheChunk(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		f := newMaintFixture(t, d, ctx)
		id := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 0, f.quoteOnlyA)
		ev := f.evidence(t, id)[0]
		_, r := f.maintain(t, f.run, id, "relocate_evidence", `,"evidence_id":"`+ev.id+`","chunk_id":"`+f.c+`","method":"rule:maintain"`)
		wantRefusal(t, "relocate to a chunk without the quote", r, "evidence_quote_not_found")
		if got := f.evidence(t, id)[0]; got != ev {
			t.Errorf("a refused relocate changed the row: %+v, was %+v", got, ev)
		}
	})
}

// flag marks the statement for a person's review with the reason, and changes
// nothing a reader acts on.
func TestMaintainStatement_FlagMarksForReview(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		f := newMaintFixture(t, d, ctx)
		id := f.seed(t, f.a, f.b, "cites", edgeMethodManual, edgeStatusAsserted, 0, f.quoteInA)
		if _, r := f.maintain(t, f.run, id, "flag", `,"reason":"kind renamed to refers_to","method":"rule:maintain"`); r.IsError {
			t.Fatalf("flag: %s", r.Text)
		}
		s := f.row(t, id)
		if s.Attributes["needs_review"] != true || s.Attributes["needs_review_reason"] != "kind renamed to refers_to" ||
			s.Status != edgeStatusAsserted || s.ExpiredAt != nil {
			t.Errorf("after flag: %+v", s)
		}
	})
}

// maintain_statement is for the code that maintains statements, and the
// operator: a run without a statement_methods grant is refused (permission),
// a method outside the grant is refused (method_not_granted), and nothing is
// asserted, accepted or deleted through it.
func TestMaintainStatement_RefusedWithoutAGrant(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		f := newMaintFixture(t, d, ctx)
		id := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusProposed, 0, f.quoteInA)
		_, r := f.maintain(t, tools.WithRunID(ctx, "run-model"), id, "verify", ``)
		wantRefusal(t, "a run holding no grant", r, "permission")
		_, r = f.maintain(t, f.run, id, "verify", `,"method":"extractor:x"`)
		wantRefusal(t, "a method outside the grant", r, "method_not_granted")
		for _, action := range []string{"assert", "accept", "delete", ""} {
			_, r = f.maintain(t, f.run, id, action, `,"method":"rule:maintain"`)
			wantRefusal(t, "action "+action, r, "action")
		}
		if s := f.row(t, id); s.Status != edgeStatusProposed || s.VerifiedAt != nil {
			t.Errorf("a refused call changed the statement: %+v", s)
		}
		// The operator plane maintains without a grant; its judge is the operator.
		if _, r = f.maintain(t, operatorCtx(ctx), id, "expire", `,"reason":"withdrawn"`); r.IsError {
			t.Fatalf("operator expire: %s", r.Text)
		}
		if s := f.row(t, id); s.JudgedBy != "operator" || s.ExpiredAt == nil {
			t.Errorf("operator expire: %+v", s)
		}
	})
}

// The propose verb gates it like propose_statement: a principal whose rules deny
// propose on either end's document is refused by access control.
func TestMaintainStatement_NeedsProposeOnTheStatementsDocument(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, open)
	out, res := docExec(t, d, open, `{"op":"link_chunks","scope":"tenant","from_id":"`+s.publicChunk+`","to_id":"`+s.secretChunk+`","kind":"cites"}`)
	if res.IsError {
		t.Fatal(res.Text)
	}
	key, _, _ := d.resolveScope(open, "tenant")
	if err := d.exec(open, key, `UPDATE chunk_edges SET method = 'rule:citation' WHERE id = ?`, out["id"]); err != nil {
		t.Fatal(err)
	}
	noPropose := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"propose"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
	noPropose = statementMethodRun(noPropose, "rule")
	_, res = docExec(t, d, noPropose, `{"op":"maintain_statement","scope":"tenant","id":"`+out["id"].(string)+`","action":"verify","method":"rule:maintain"}`)
	refusedByACL(t, "maintain without propose", res)
}

// Every action that changes a statement tells its documents' change channels.
func TestMaintainStatement_PublishesTheEdgeChange(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	rec := &edgeChangeRecorder{}
	d.Writer = rec
	f := newMaintFixture(t, d, ctx)
	id := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 0, f.quoteInA)
	rec.reset()
	if _, r := f.maintain(t, f.run, id, "verify", `,"method":"rule:maintain"`); r.IsError {
		t.Fatal(r.Text)
	}
	if !rec.saw("documents/"+f.doc+"/chunks", `"op":"maintain_statement"`, id) {
		t.Errorf("verify published no edge change: %v", rec.all())
	}
}

type edgeChangeRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (w *edgeChangeRecorder) Write(_ context.Context, req channels.WriteRequest) (channels.WriteResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, req.Channel+" "+string(req.Payload))
	return channels.WriteResult{}, nil
}

func (w *edgeChangeRecorder) reset() { w.mu.Lock(); w.msgs = nil; w.mu.Unlock() }

func (w *edgeChangeRecorder) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.msgs...)
}

func (w *edgeChangeRecorder) saw(parts ...string) bool {
	for _, m := range w.all() {
		ok := true
		for _, p := range parts {
			if !strings.Contains(m, p) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// A maintenance write is compare-and-set on the statement it read: when the
// statement changed in between, the action is refused naming statement_changed
// rather than applied over the other writer's change.
func TestMaintainStatement_LostRaceNamesStatementChanged(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		f := newMaintFixture(t, d, ctx)
		key, mscope, _ := d.resolveScope(ctx, "user")
		for i, c := range []struct{ action, extra string }{
			{"verify", ``},
			{"expire", `,"reason":"r"`},
			{"demote", `,"reason":"r"`},
			{"flag", `,"reason":"r"`},
		} {
			id := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, int64(i+100), f.quoteInA)
			read := f.row(t, id) // what this maintainer read
			// Another writer retires it first.
			if _, r := f.maintain(t, f.run, id, "expire", `,"reason":"other writer","method":"rule:maintain"`); r.IsError {
				t.Fatal(r.Text)
			}
			var in docInput
			if err := json.Unmarshal([]byte(`{"op":"maintain_statement","id":"`+id+`","action":"`+c.action+`"`+c.extra+`}`), &in); err != nil {
				t.Fatal(err)
			}
			in.edgeMethod = "rule:maintain"
			r, err := d.applyMaintenance(f.run, key, mscope, in, &read)
			if err != nil {
				t.Fatal(err)
			}
			wantRefusal(t, c.action+" after another writer expired it", r, "statement_changed")
			if s := f.row(t, id); s.JudgeReason != "other writer" {
				t.Errorf("%s overwrote the other writer's change: %+v", c.action, s)
			}
		}

		// A merge or an evidence mark over a statement retired since it was read.
		survivor := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, 200)
		for i, c := range []struct{ action, extra string }{
			{"merge", `,"survivor_id":"` + survivor + `"`},
			{"stale_evidence", ``},
		} {
			id := f.seed(t, f.a, f.b, "cites", "rule:citation", edgeStatusAsserted, int64(i+300), f.quoteOnlyA)
			ev := f.evidence(t, id)[0]
			read := f.row(t, id)
			if _, r := f.maintain(t, f.run, id, "expire", `,"reason":"other writer","method":"rule:maintain"`); r.IsError {
				t.Fatal(r.Text)
			}
			var in docInput
			if err := json.Unmarshal([]byte(`{"op":"maintain_statement","id":"`+id+`","action":"`+c.action+`","evidence_id":"`+ev.id+`"`+c.extra+`}`), &in); err != nil {
				t.Fatal(err)
			}
			in.edgeMethod = "rule:maintain"
			r, err := d.applyMaintenance(f.run, key, mscope, in, &read)
			if err != nil {
				t.Fatal(err)
			}
			wantRefusal(t, c.action+" after another writer expired it", r, "statement_changed")
			if got := f.evidence(t, id); len(got) != 1 || got[0] != ev {
				t.Errorf("%s changed the retired statement's evidence: %+v", c.action, got)
			}
		}
		if got := f.evidence(t, survivor); len(got) != 0 {
			t.Errorf("a refused merge moved evidence to the survivor: %+v", got)
		}
	})
}
