package builtin

import (
	"context"
	"fmt"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A remembered fact on the peer is the peer's claim: a run's pull carries it
// like any keyed chunk, with the peer's provenance, rather than refusing it as
// a forged remember and leaving the pull half applied.
func TestSyncPull_ARunPullsAPeersRememberedFact(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		localDoc, pd, pctx, remoteDoc := carryPair(t, d, ctx)
		upsertKeyed(t, pd, operatorCtx(pctx), remoteDoc, "memory/operator/ada-takes-tea", "Ada takes tea.")
		upsertKeyed(t, pd, pctx, remoteDoc, "nkB", "b")
		out, r := docExec(t, d, tenantRunCtx(ctx), fmt.Sprintf(`{"op":"sync","id":%q}`, localDoc))
		if r.IsError {
			t.Fatalf("a run's pull of a peer's remembered fact failed: %s", r.Text)
		}
		if out["created"] != float64(2) {
			t.Errorf("pull = %v, want both keyed chunks created", out)
		}
	})
}

// A scope from before statements had methods stamped every edge manual with no
// run id, the runtime's structural ones (supersedes, about) included. A run's copy
// carries those as they are — demoting them would hide a supersedes chain from
// every walk — and still files the run's own relations for review.
func TestCopyDocuments_AnOldScopesStructuralEdgesTravelAsTheyAre(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		run := tenantRunCtx(base)
		a, b := twoChunks(t, d, run, "user", "/old/kb")
		scopeExec(t, d, run, `INSERT INTO chunk_edges (id, from_id, to_id, kind, created_at, auto, status, method) VALUES (?, ?, ?, 'supersedes', 1, 0, 'asserted', 'manual')`, "old-supersedes", b, a)
		scopeExec(t, d, run, `INSERT INTO chunk_edges (id, from_id, to_id, kind, created_at, auto, status, method, run_id) VALUES (?, ?, ?, 'supported_by', 1, 0, 'asserted', 'manual', 'run-old')`, "old-run-row", a, b)
		if _, r := docExec(t, d, run, `{"op":"copy_documents","scope":"user","to_scope":"tenant","under_path":"/old","dry_run":false}`); r.IsError {
			t.Fatalf("copy_documents: %s", r.Text)
		}
		key, _, err := d.resolveScope(reviewOperatorCtx(base, nil), "tenant")
		if err != nil {
			t.Fatal(err)
		}
		res, err := d.query(base, key, `SELECT kind, status FROM chunk_edges`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, row := range res.Rows {
			got[asStr(row[0])] = asStr(row[1])
		}
		if got["supersedes"] != edgeStatusAsserted {
			t.Errorf("an old scope's supersedes edge arrived %q, want asserted", got["supersedes"])
		}
		if got["supported_by"] != edgeStatusProposed {
			t.Errorf("a run's own manual row arrived %q, want proposed", got["supported_by"])
		}
	})
}

// A link that rules on a proposal is accept_statement's ruling: a judge's
// refuting verdict goes (the statement is visible again), the ruling is the
// operator's, and the proposer stays the actor.
func TestLinkChunks_AReviewersLinkRulesAsAcceptStatementDoes(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		op := reviewOperatorCtx(base, nil)
		a, b := twoChunks(t, d, op, "tenant", "/kb/ruled")
		p, r := docExec(t, d, tenantRunCtx(base), fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites"}`, a, b))
		if r.IsError {
			t.Fatal(r.Text)
		}
		id := asStr(p["id"])
		proposer := asStr(statementOf(t, d, op, "tenant", id)["actor"])
		key, _, _ := d.resolveScope(op, "tenant")
		if err := d.exec(op, key, `UPDATE chunk_edges SET verdict_confidence = 0.05, judged_at = 1, judged_by = 'judge' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}

		linkFrom(t, d, op, "tenant", a, b, "cites")
		s := statementOf(t, d, op, "tenant", id)
		if s["status"] != edgeStatusAsserted || s["verdict_confidence"] != nil || s["judged_by"] != "operator" || s["operator_ruled_at"] == nil {
			t.Errorf("the operator's link = %v, want asserted, the refuting verdict cleared, judged by the operator", s)
		}
		if proposer == "" || s["actor"] != proposer {
			t.Errorf("actor = %v, want the proposer %q", s["actor"], proposer)
		}
	})
}

// A member's link asserts a waiting proposal only where a rule grants it review
// on the statement's document; without it the proposal keeps waiting.
func TestLinkChunks_AMemberAssertsAProposalOnlyWithTheReviewGrant(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		op := reviewOperatorCtx(base, nil)
		a, b := twoChunks(t, d, op, "tenant", "/kb/member")
		member := func(review bool) context.Context {
			actions := []string{"read", "write", "link", "propose"}
			if review {
				actions = append(actions, "review")
			}
			eff := aclSet(t, authz.Rule{Effect: authz.Allow, Actions: actions, Resources: []string{"document:tenant", "document:tenant:/**"}})
			ctx := auth.WithPrincipal(base, auth.Principal{TenantID: "tnt", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate}, Effective: eff})
			return tools.WithCallerTier(tools.WithRunPolicy(ctx, tools.RunPolicy{Tools: []string{"*"},
				Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
				SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}}), tools.TierMember)
		}
		for _, tc := range []struct {
			review bool
			kind   string
			want   string
		}{{false, "cites", edgeStatusProposed}, {true, "refers", edgeStatusAsserted}} {
			p, r := docExec(t, d, tenantRunCtx(base), fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":%q}`, a, b, tc.kind))
			if r.IsError {
				t.Fatal(r.Text)
			}
			if _, r := docExec(t, d, member(tc.review), fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":%q}`, a, b, tc.kind)); r.IsError {
				t.Fatalf("member link (review %v): %s", tc.review, r.Text)
			}
			if s := statementOf(t, d, op, "tenant", asStr(p["id"])); s["status"] != tc.want {
				t.Errorf("a member's link with review=%v left it %v, want %s", tc.review, s["status"], tc.want)
			}
		}
	})
}
