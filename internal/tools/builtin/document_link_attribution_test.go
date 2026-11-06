package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// tenantRunCtx is a run holding the tenant Document grants and, when given,
// statement_methods.
func tenantRunCtx(base context.Context, methods ...string) context.Context {
	return tools.WithRunPolicy(tools.WithRunID(base, "run-link"), tools.RunPolicy{
		Memory:           tools.MemoryPolicyValue{AllowedScopes: []string{"tenant", "user"}},
		SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant", "user"}},
		StatementMethods: methods,
	})
}

// withStatementGrant adds statement_methods grants to the run on ctx, for tests
// whose subject is not the grant itself.
func withStatementGrant(ctx context.Context, grants ...string) context.Context {
	return tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) {
		p.StatementMethods = append(append([]string(nil), p.StatementMethods...), grants...)
	})
}

// memberCtx is a tenant member's off-run call: a person, not a run.
func memberCtx(base context.Context) context.Context {
	return tools.WithCallerTier(tools.WithRunPolicy(base, tools.RunPolicy{
		Tools:  []string{"*"},
		Memory: tools.MemoryPolicyValue{AllowedScopes: []string{"tenant", "user"}},
		SqlMem: tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant", "user"}},
	}), tools.TierMember)
}

// twoChunks makes a document in scope with two chunks and returns their ids.
func twoChunks(t *testing.T, d *Document, ctx context.Context, scope, path string) (string, string) {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":%q,"title":"Links","path":%q}`, scope, path))
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	var ids []string
	for _, body := range []string{"The first clause cites the second.", "The second clause."} {
		o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":%q,"document_id":%q,"title":"part","body":%q}`, scope, out["document_id"], body))
		if r.IsError {
			t.Fatalf("create_chunk: %s", r.Text)
		}
		ids = append(ids, o["id"].(string))
	}
	return ids[0], ids[1]
}

func linkFrom(t *testing.T, d *Document, ctx context.Context, scope, from, to, kind string) map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":%q,"from_id":%q,"to_id":%q,"kind":%q}`, scope, from, to, kind))
	if r.IsError {
		t.Fatalf("link_chunks %s: %s", kind, r.Text)
	}
	return out
}

func statementOf(t *testing.T, d *Document, ctx context.Context, scope, id string) map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":%q,"id":%q}`, scope, id))
	if r.IsError {
		t.Fatalf("get_edge: %s", r.Text)
	}
	return out["statement"].(map[string]any)
}

// A relation a run writes says so: method agent, the run's agent as actor and
// its run id. Outside its own tenant scope it stays asserted. A person's off-run
// call — a member's, the operator's — keeps manual.
func TestLinkChunks_ARunIsRecordedAsTheActor(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		run := tenantRunCtx(base)
		a, b := twoChunks(t, d, run, "user", "/docs/links")

		s := statementOf(t, d, run, "user", linkFrom(t, d, run, "user", a, b, "cites")["id"].(string))
		if agent := tools.AgentName(base); s["status"] != edgeStatusAsserted || s["method"] != edgeMethodAgent || s["actor"] != agent || s["run_id"] != "run-link" {
			t.Errorf("a run's link = status %v method %v actor %v run %v; want asserted, agent, %s, run-link",
				s["status"], s["method"], s["actor"], s["run_id"], agent)
		}
		for name, ctx := range map[string]context.Context{"member": memberCtx(base), "operator": operatorCtx(memberCtx(base))} {
			s := statementOf(t, d, ctx, "user", linkFrom(t, d, ctx, "user", a, b, "refers_"+name)["id"].(string))
			if s["method"] != edgeMethodManual || s["run_id"] != nil {
				t.Errorf("%s's off-run link = method %v run %v; want manual and no run", name, s["method"], s["run_id"])
			}
		}
	})
}

// In the tenant scope — read by every user and agent of the tenant — a run's
// link is filed for review unless its agent holds statement_methods: [link].
// A person's link there is asserted as before.
func TestLinkChunks_ATenantLinkFromARunIsAProposal(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		op := reviewOperatorCtx(base, nil)
		a, b := twoChunks(t, d, op, "tenant", "/kb/links")

		out := linkFrom(t, d, tenantRunCtx(base), "tenant", a, b, "cites")
		s := statementOf(t, d, op, "tenant", out["id"].(string))
		if out["status"] != edgeStatusProposed || s["status"] != edgeStatusProposed || s["method"] != edgeMethodAgent {
			t.Errorf("an ungranted run's tenant link = %v (stored %v/%v), want proposed by agent", out["status"], s["status"], s["method"])
		}

		granted := linkFrom(t, d, tenantRunCtx(base, "link"), "tenant", a, b, "refers")
		if s := statementOf(t, d, op, "tenant", granted["id"].(string)); s["status"] != edgeStatusAsserted || s["method"] != edgeMethodAgent {
			t.Errorf("a granted run's tenant link = %v/%v, want asserted by agent", s["status"], s["method"])
		}

		person := linkFrom(t, d, op, "tenant", a, b, "defines")
		if s := statementOf(t, d, op, "tenant", person["id"].(string)); s["status"] != edgeStatusAsserted || s["method"] != edgeMethodManual {
			t.Errorf("the operator's tenant link = %v/%v, want asserted manual", s["status"], s["method"])
		}
	})
}

// remember files a statement as a person's, self-citing and evidential. From a
// run that takes the agent's statement_methods: [remember]; without it the run is
// pointed at the proposal path. A person's off-run call needs no grant.
func TestRemember_ARunNeedsTheGrant(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		const req = `{"op":"remember","scope":"user","text":"The user deploys on Fridays."}`
		_, r := docExec(t, d, tenantRunCtx(base), req)
		if !r.IsError || !strings.Contains(r.Text, "propose_statement") {
			t.Fatalf("an ungranted run's remember = %v %s, want a refusal pointing at propose_statement", r.IsError, r.Text)
		}
		for name, ctx := range map[string]context.Context{"granted run": tenantRunCtx(base, "remember"), "member": memberCtx(base)} {
			out, r := docExec(t, d, ctx, req)
			if r.IsError || out["id"] == nil {
				t.Errorf("%s's remember was refused: %s", name, r.Text)
			}
		}
	})
}

// The relation a fact states (upsert_chunk relation with two about_ids) is a
// statement a run chose like a link_chunks: in the tenant scope it is filed for
// review unless the agent holds the link grant. A code agent stamping its granted
// rule: method is its own authority and is not held to it.
func TestFactRelation_ATenantRelationFromARunIsAProposal(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		op := reviewOperatorCtx(base, nil)
		a, b := twoChunks(t, d, op, "tenant", "/kb/facts")
		out, r := docExec(t, d, op, `{"op":"get_chunk","scope":"tenant","id":"`+a+`"}`)
		if r.IsError {
			t.Fatalf("get_chunk: %s", r.Text)
		}
		doc := asStr(out["document_id"])
		relationOf := func(ctx context.Context, nk, kind string) map[string]any {
			t.Helper()
			out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"tenant","document_id":%q,"natural_key":%q,
				"title":"A relates to B.","body":"A relates to B.","about_ids":[%q,%q],"relation":%q}`, doc, nk, a, b, kind))
			if r.IsError {
				t.Fatalf("upsert_chunk %s: %s", nk, r.Text)
			}
			rel, _ := out["relation"].(map[string]any)
			if rel == nil {
				t.Fatalf("upsert_chunk %s wrote no relation: %v", nk, out)
			}
			return rel
		}
		if rel := relationOf(tenantRunCtx(base), "fact:one", "cites"); rel["status"] != edgeStatusProposed {
			t.Errorf("an ungranted run's tenant relation = %v, want proposed", rel)
		}
		if rel := relationOf(tenantRunCtx(base, statementLinkGrant), "fact:two", "refers"); rel["status"] != edgeStatusAsserted {
			t.Errorf("a granted run's tenant relation = %v, want asserted", rel)
		}
	})
}

// A run copying documents into the tenant scope carries their statements as
// proposals: what it asserted, or ruled on, in a scope of its own is not the
// tenant's until it is reviewed there.
func TestCopyDocuments_ARunsStatementsArriveInTheTenantAsProposals(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		run := tenantRunCtx(base)
		a, b := twoChunks(t, d, run, "user", "/mine/links")
		if s := linkFrom(t, d, run, "user", a, b, "cites"); s["status"] != edgeStatusAsserted {
			t.Fatalf("the user-scope link = %v, want asserted", s)
		}
		out, r := docExec(t, d, run, `{"op":"copy_documents","scope":"user","to_scope":"tenant","under_path":"/mine","dry_run":false}`)
		if r.IsError {
			t.Fatalf("copy_documents: %s", r.Text)
		}
		_ = out
		op := reviewOperatorCtx(base, nil)
		ls, r := docExec(t, d, op, `{"op":"list_statements","scope":"tenant","kind":"cites","status":"asserted"}`)
		if r.IsError {
			t.Fatalf("list_statements: %s", r.Text)
		}
		if got, _ := ls["statements"].([]any); len(got) != 0 {
			t.Errorf("a run's copy asserted %d statements in the tenant: %v", len(got), got)
		}
		ls, _ = docExec(t, d, op, `{"op":"list_statements","scope":"tenant","kind":"cites","status":"proposed"}`)
		if got, _ := ls["statements"].([]any); len(got) != 1 {
			t.Errorf("the copied statement is not a proposal in the tenant: %v", ls)
		}
	})
}
