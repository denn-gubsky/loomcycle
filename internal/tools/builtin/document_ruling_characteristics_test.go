package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A ruling makes a statement live exactly as link_chunks does, so it answers to
// the same relation characteristics; and accepting a revision retires the
// statement it revises, so the corrected relation is never live twice.

// relBothTiers runs fn on a relation fixture over each SQL Memory tier: the
// accept and its retirement of an original are one transaction, which each tier
// runs its own way.
func relBothTiers(t *testing.T, fn func(*testing.T, relFixture)) {
	t.Helper()
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		fn(t, newRelFixtureOn(t, d, base, relationsOntologyMD))
	})
}

func (f relFixture) rule(t *testing.T, op, id string) (map[string]any, tools.Result) {
	t.Helper()
	extra := ""
	if op == "reject_statement" {
		extra = `,"reason":"wrong"`
	}
	return docExec(t, f.d, f.ctx, fmt.Sprintf(`{"op":%q,"scope":"user","id":%q%s}`, op, id, extra))
}

// edgeState reads a statement's status and whether it has expired, from the store.
func (f relFixture) edgeState(t *testing.T, id string) (status string, expired bool) {
	t.Helper()
	res, err := f.d.query(f.ctx, sidecarScope(t, f.d, f.ctx), `SELECT status, expired_at FROM chunk_edges WHERE id = ?`, id)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("read %s: %v %v", id, res, err)
	}
	return asStr(res.Rows[0][0]), res.Rows[0][1] != nil
}

// liveFrom counts the live asserted statements of kind from a chunk.
func (f relFixture) liveFrom(t *testing.T, kind, from string) int {
	t.Helper()
	res, err := f.d.query(f.ctx, sidecarScope(t, f.d, f.ctx), `SELECT count(*) FROM chunk_edges
		WHERE kind = ? AND from_id = ? AND status = 'asserted' AND expired_at IS NULL`, kind, from)
	if err != nil {
		t.Fatalf("count %s: %v", kind, err)
	}
	return asInt(res.Rows[0][0])
}

func (f relFixture) linkOK(t *testing.T, kind, from, to string) string {
	t.Helper()
	out, refused, text := f.link(t, kind, from, to, "")
	if refused {
		t.Fatalf("link %s: %s", kind, text)
	}
	return out["id"].(string)
}

func (f relFixture) revise(t *testing.T, id string) string {
	t.Helper()
	out, r := docExec(t, f.d, f.ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","id":%q,"confidence":0.9}`, id))
	if r.IsError {
		t.Fatalf("revise %s: %s", id, r.Text)
	}
	return out["id"].(string)
}

func TestAcceptStatement_RefusedWhenAFunctionalKindWouldGainASecondTarget(t *testing.T) {
	relBothTiers(t, testAcceptStatement_RefusedWhenAFunctionalKindWouldGainASecondTarget)
}

func testAcceptStatement_RefusedWhenAFunctionalKindWouldGainASecondTarget(t *testing.T, f relFixture) {
	f.linkOK(t, "supervises", f.provA, f.provB)
	p := f.propose(t, "supervises", f.provA, f.untyped)["id"].(string)
	_, r := f.rule(t, "accept_statement", p)
	text := refusalText(r)
	if !r.IsError || !strings.Contains(text, "characteristic_violation") || !strings.Contains(text, "revision") {
		t.Fatalf("accepting a second target of a functional kind: %s", text)
	}
	if status, _ := f.edgeState(t, p); status != edgeStatusProposed {
		t.Errorf("the refused proposal is %s, want proposed", status)
	}
	if n := f.liveFrom(t, "supervises", f.provA); n != 1 {
		t.Errorf("%d live supervises from A, want 1", n)
	}
	// A rejection is never checked: it makes nothing live.
	if _, r := f.rule(t, "reject_statement", p); r.IsError {
		t.Errorf("rejecting the conflicting proposal: %s", r.Text)
	}
}

func TestAcceptStatement_RefusedWhenAnAsymmetricKindHasItsOppositeLive(t *testing.T) {
	relBothTiers(t, testAcceptStatement_RefusedWhenAnAsymmetricKindHasItsOppositeLive)
}

func testAcceptStatement_RefusedWhenAnAsymmetricKindHasItsOppositeLive(t *testing.T, f relFixture) {
	f.linkOK(t, "stricter_than", f.provA, f.provB)
	p := f.propose(t, "stricter_than", f.provB, f.provA)["id"].(string)
	_, r := f.rule(t, "accept_statement", p)
	if text := refusalText(r); !r.IsError || !strings.Contains(text, "characteristic_violation") || !strings.Contains(text, "asymmetric") {
		t.Fatalf("accepting the opposite of an asymmetric statement: %s", text)
	}
	if status, _ := f.edgeState(t, p); status != edgeStatusProposed {
		t.Errorf("the refused proposal is %s, want proposed", status)
	}
}

func TestRuleStatements_ReportsACharacteristicRefusalPerItem(t *testing.T) {
	relBothTiers(t, testRuleStatements_ReportsACharacteristicRefusalPerItem)
}

func testRuleStatements_ReportsACharacteristicRefusalPerItem(t *testing.T, f relFixture) {
	f.linkOK(t, "supervises", f.provA, f.provB)
	bad := f.propose(t, "supervises", f.provA, f.untyped)["id"].(string)
	good := f.propose(t, "supervises", f.provB, f.untyped)["id"].(string)
	req := connector.RuleStatementsRequest{Action: "accept", IDs: []string{bad, good}}
	dry, err := f.d.RuleStatements(f.ctx, "user", req, RuleAsOperator)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	req.DryRun, req.ExpectCount = boolPtr(false), intPtr(dry.Matched)
	res, err := f.d.RuleStatements(f.ctx, "user", req, RuleAsOperator)
	if err != nil {
		t.Fatalf("rule: %v", err)
	}
	if res.Ruled != 1 || res.Failed != 1 {
		t.Fatalf("ruled %d, failed %d, want 1 and 1: %+v", res.Ruled, res.Failed, res.Items)
	}
	for _, it := range res.Items {
		switch it.ID {
		case bad:
			if it.Error != "characteristic_violation" || it.Status != "" {
				t.Errorf("conflicting item = %+v, want error characteristic_violation", it)
			}
		case good:
			if it.Status != edgeStatusAsserted || it.Error != "" {
				t.Errorf("clean item = %+v, want asserted", it)
			}
		}
	}
	if status, _ := f.edgeState(t, bad); status != edgeStatusProposed {
		t.Errorf("the refused proposal is %s, want proposed", status)
	}
}

func TestAcceptStatement_RevisionRetiresTheOriginal(t *testing.T) {
	relBothTiers(t, testAcceptStatement_RevisionRetiresTheOriginal)
}

func testAcceptStatement_RevisionRetiresTheOriginal(t *testing.T, f relFixture) {
	orig := f.linkOK(t, "stricter_than", f.provA, f.provB)
	rev := f.revise(t, orig)
	if _, r := f.rule(t, "accept_statement", rev); r.IsError {
		t.Fatalf("accepting the revision: %s", r.Text)
	}
	if status, expired := f.edgeState(t, orig); !expired || status != edgeStatusAsserted {
		t.Errorf("original = %s expired %v, want asserted and expired", status, expired)
	}
	if status, expired := f.edgeState(t, rev); expired || status != edgeStatusAsserted {
		t.Errorf("revision = %s expired %v, want live asserted", status, expired)
	}
	if n := f.liveFrom(t, "stricter_than", f.provA); n != 1 {
		t.Errorf("%d live stricter_than from A, want 1", n)
	}
}

func TestAcceptStatement_RevisionOfAnExpiredOriginalStillAccepts(t *testing.T) {
	relBothTiers(t, testAcceptStatement_RevisionOfAnExpiredOriginalStillAccepts)
}

func testAcceptStatement_RevisionOfAnExpiredOriginalStillAccepts(t *testing.T, f relFixture) {
	orig := f.linkOK(t, "stricter_than", f.provA, f.provB)
	rev := f.revise(t, orig)
	// Retired by other means meanwhile (maintain_statement leaves a person's
	// link alone, so the store is written directly, as a sweep would).
	key := sidecarScope(t, f.d, f.ctx)
	if err := f.d.exec(f.ctx, key, `UPDATE chunk_edges SET expired_at = 42 WHERE id = ?`, orig); err != nil {
		t.Fatalf("expire the original: %v", err)
	}
	if _, r := f.rule(t, "accept_statement", rev); r.IsError {
		t.Fatalf("accepting a revision of an expired original: %s", r.Text)
	}
	if res, err := f.d.query(f.ctx, key, `SELECT expired_at FROM chunk_edges WHERE id = ?`, orig); err != nil || asInt(res.Rows[0][0]) != 42 {
		t.Errorf("the original's earlier expiry was rewritten: %v %v", res, err)
	}
	if status, _ := f.edgeState(t, rev); status != edgeStatusAsserted {
		t.Errorf("revision = %s, want asserted", status)
	}
}

// The original holds the only other supervises from A; it is the statement being
// replaced, so it is no conflict.
func TestAcceptStatement_RevisionOfAFunctionalStatementIsAccepted(t *testing.T) {
	relBothTiers(t, testAcceptStatement_RevisionOfAFunctionalStatementIsAccepted)
}

func testAcceptStatement_RevisionOfAFunctionalStatementIsAccepted(t *testing.T, f relFixture) {
	orig := f.linkOK(t, "supervises", f.provA, f.provB)
	rev := f.revise(t, orig)
	if _, r := f.rule(t, "accept_statement", rev); r.IsError {
		t.Fatalf("accepting a revision of a functional statement: %s", refusalText(r))
	}
	if n := f.liveFrom(t, "supervises", f.provA); n != 1 {
		t.Errorf("%d live supervises from A, want 1", n)
	}
	if _, expired := f.edgeState(t, orig); !expired {
		t.Error("the original stayed live beside its accepted revision")
	}
}

func TestRevertRuling_RestoresTheOriginalARevisionRetired(t *testing.T) {
	relBothTiers(t, testRevertRuling_RestoresTheOriginalARevisionRetired)
}

func testRevertRuling_RestoresTheOriginalARevisionRetired(t *testing.T, f relFixture) {
	accept := func(ids ...string) string {
		req := connector.RuleStatementsRequest{Action: "accept", IDs: ids}
		dry, err := f.d.RuleStatements(f.ctx, "user", req, RuleAsOperator)
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		req.DryRun, req.ExpectCount = boolPtr(false), intPtr(dry.Matched)
		res, err := f.d.RuleStatements(f.ctx, "user", req, RuleAsOperator)
		if err != nil || res.Ruled != len(ids) {
			t.Fatalf("rule: %+v %v", res, err)
		}
		return res.RulingID
	}
	revert := func(id string) connector.RevertRulingResult {
		rev, err := f.d.RevertRuling(f.ctx, "user", connector.RevertRulingRequest{RulingID: id}, RuleAsOperator)
		if err != nil {
			t.Fatalf("revert: %v", err)
		}
		return rev
	}

	orig := f.linkOK(t, "stricter_than", f.provA, f.provB)
	rev := f.revise(t, orig)
	if rv := revert(accept(rev)); rv.Reverted != 1 || rv.Changed != 0 {
		t.Fatalf("revert = %+v", rv)
	}
	if status, expired := f.edgeState(t, orig); expired || status != edgeStatusAsserted {
		t.Errorf("original after revert = %s expired %v, want live asserted", status, expired)
	}
	if status, _ := f.edgeState(t, rev); status != edgeStatusProposed {
		t.Errorf("revision after revert = %s, want proposed", status)
	}

	// An original removed since is not brought back, and the revision's
	// ruling stands: the revert is all or nothing for the pair.
	ruling := accept(rev)
	if _, r := docExec(t, f.d, f.ctx, fmt.Sprintf(`{"op":"unlink_chunks","scope":"user","id":%q}`, orig)); r.IsError {
		t.Fatalf("unlink the original: %s", r.Text)
	}
	if rv := revert(ruling); rv.Reverted != 0 || rv.Changed != 1 {
		t.Errorf("revert after the original was removed = %+v", rv)
	}
	if status, _ := f.edgeState(t, rev); status != edgeStatusAsserted {
		t.Errorf("revision = %s, want asserted (the revert was refused)", status)
	}
}
