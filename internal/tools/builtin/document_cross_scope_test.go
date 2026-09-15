package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC CV P4, decision 11 — one subject, read across the scopes the caller can reach.

// seedSubjectWithFact builds a /facts/<slug> dossier in `scope` whose ROOT is the
// entity, with one fact under it. Returns the root chunk id.
func seedSubjectWithFact(t *testing.T, d *Document, ctx context.Context, scope, slug, key, fact string) string {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"`+scope+`","title":"Dave",
		"path":"/facts/`+slug+`","type":"person","subject":"Dave","natural_key":"`+key+`"}`)
	if r.IsError {
		t.Fatalf("create %s dossier: %s", scope, r.Text)
	}
	docID, root := asStr(out["document_id"]), asStr(out["root_chunk_id"])
	if fact != "" {
		if _, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"`+scope+`","document_id":"`+docID+`",
			"parent_id":"`+root+`","natural_key":"memory/fact/`+slug+`-`+strings.Fields(fact)[0]+`",
			"title":"`+fact+`","body":"`+fact+`","type":"fact","subject":"Dave"}`); r.IsError {
			t.Fatalf("seed %s fact: %s", scope, r.Text)
		}
	}
	return root
}

// TestListFacts_AcrossScopesFindsTheSameSubjectElsewhere is the payoff.
//
// A subject exists once per scope. After an adoption the tenant registry holds
// `person:dave` with nothing under it while the user scope knows plenty — so "what do
// we know about Dave" answered with whichever half the caller happened to read.
func TestListFacts_AcrossScopesFindsTheSameSubjectElsewhere(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	gctx := grantedTenantCtx(ctx)

	// The tenant registry entry, adopted and still empty; and the user's own knowledge.
	tenantRoot := seedSubjectWithFact(t, d, gctx, "tenant", "dave", "person:dave", "")
	seedSubjectWithFact(t, d, gctx, "user", "dave", "person:dave", "Dave runs the shop")

	// Read from the TENANT side, which is the empty one.
	out, r := docExec(t, d, gctx, `{"op":"list_facts","scope":"tenant","about":"`+tenantRoot+`","across_scopes":true}`)
	if r.IsError {
		t.Fatalf("list_facts across: %s", r.Text)
	}
	// The tenant side knows only its own entity node — `about:` includes the subject
	// chunk itself, which is existing P3 behaviour and not what this phase changes.
	// What matters is that it knows no FACTS, and the block below says another scope
	// does.
	if n, _ := out["count"].(float64); n != 1 {
		t.Fatalf("the tenant dossier should hold just its entity node, got count=%v", n)
	}
	across, _ := out["across_scopes"].([]any)
	if len(across) == 0 {
		t.Fatal("the tenant dossier reported nothing elsewhere, while the user scope knows " +
			"about the same subject by the same key — the split this exists to close")
	}
	first, _ := across[0].(map[string]any)
	if got, _ := first["scope"].(string); got != "user" {
		t.Errorf("elsewhere scope = %q, want user", got)
	}
	if n, _ := first["facts"].(float64); n != 1 {
		t.Errorf("elsewhere reported %v facts, want 1", n)
	}
	rows, _ := first["fact_rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("fact_rows = %d, want the fact itself", len(rows))
	}
	row, _ := rows[0].(map[string]any)
	if title, _ := row["title"].(string); !strings.Contains(title, "runs the shop") {
		t.Errorf("the fact from the other scope is missing: %v", row)
	}
	// The scope rides on every ROW, not just the group — a caller that flattens the
	// response must still be able to tell a shared fact from their own.
	if got, _ := row["scope"].(string); got != "user" {
		t.Errorf("row scope = %q, want user", got)
	}
}

// TestListFacts_AcrossScopesIsOffByDefault. Every existing caller's response must be
// unchanged: the fan-out costs up to three queries across three schemas.
func TestListFacts_AcrossScopesIsOffByDefault(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	gctx := grantedTenantCtx(ctx)
	tenantRoot := seedSubjectWithFact(t, d, gctx, "tenant", "dave", "person:dave", "")
	seedSubjectWithFact(t, d, gctx, "user", "dave", "person:dave", "Dave runs the shop")

	out, r := docExec(t, d, gctx, `{"op":"list_facts","scope":"tenant","about":"`+tenantRoot+`"}`)
	if r.IsError {
		t.Fatalf("list_facts: %s", r.Text)
	}
	if _, present := out["across_scopes"]; present {
		t.Error("a read that did not ask for the fan-out got one")
	}
}

// TestListFacts_AcrossWithoutAboutIsRefused. The flag unifies ONE SUBJECT; with no
// subject it would silently do nothing and the caller would read the local answer as
// the wide one.
func TestListFacts_AcrossWithoutAboutIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, r := docExec(t, d, grantedTenantCtx(ctx), `{"op":"list_facts","scope":"user","across_scopes":true}`)
	if !r.IsError || !strings.Contains(r.Text, "about") {
		t.Errorf("across without about = %q, want a refusal that names `about`", r.Text)
	}
}

// TestSubjectAcrossScopes_NeverReachesAnotherUsersScope.
//
// The fan-out resolves `user` through the SAME gate every Document op uses, which
// resolves it to the CALLER'S OWN user id server-side. So a tenant dossier read by one
// person gains their facts and the tenant's, never somebody else's — the boundary
// decision 10 drew when it refused to backfill one user's history into the shared
// plane.
func TestSubjectAcrossScopes_NeverReachesAnotherUsersScope(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	gctx := grantedTenantCtx(ctx)
	seedSubjectWithFact(t, d, gctx, "user", "dave", "person:dave", "Dave runs the shop")

	// A DIFFERENT user asks. Same tenant, same subject key, their own empty scope.
	other := tools.WithRunIdentity(gctx, tools.RunIdentityValue{
		AgentID: "a", UserID: "u2", TenantID: direntTenant(gctx)})
	key, _, err := d.resolveScope(other, "tenant")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	found := d.subjectAcrossScopes(other, key.Scope, "person:dave", 10, true)
	for _, sc := range found {
		if sc.Scope == "user" && sc.FactCount > 0 {
			t.Errorf("the fan-out reached another user's facts: %+v", sc)
		}
	}
}

// TestSubjectAcrossScopes_SkipsAScopeWithNoEntityTier: a scope that never used the
// fact tier has no sidecar table at all, and "this scope knows nothing about Dave" is
// the same answer to the caller as "this scope has no table".
func TestSubjectAcrossScopes_SkipsAScopeWithNoEntityTier(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	gctx := grantedTenantCtx(ctx)
	seedSubjectWithFact(t, d, gctx, "user", "dave", "person:dave", "Dave runs the shop")
	key, _, _ := d.resolveScope(gctx, "user")
	// agent + tenant scopes were never written to.
	found := d.subjectAcrossScopes(gctx, key.Scope, "person:dave", 10, false)
	for _, sc := range found {
		if sc.Scope == "agent" {
			t.Errorf("an untouched scope was reported as knowing the subject: %+v", sc)
		}
	}
}

// TestSubjectAcrossScopes_NoKeyIsAnEmptyAnswerNotAnError. An ordinary document's root
// has no sidecar, so asking to read it across scopes is a question with an empty
// answer rather than a wrong one.
func TestSubjectAcrossScopes_NoKeyIsAnEmptyAnswerNotAnError(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	key, _, _ := d.resolveScope(ctx, "user")
	if found := d.subjectAcrossScopes(ctx, key.Scope, "", 10, true); len(found) != 0 {
		t.Errorf("a subject with no key matched something: %+v", found)
	}
}

// TestGetDocument_AcrossScopesSignpostsWithoutTheRows. A dossier read is a signpost;
// a caller who wants the facts asks list_facts. What it answers is the question an
// empty tenant dossier otherwise leaves hanging.
func TestGetDocument_AcrossScopesSignpostsWithoutTheRows(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	gctx := grantedTenantCtx(ctx)
	seedSubjectWithFact(t, d, gctx, "tenant", "dave", "person:dave", "")
	seedSubjectWithFact(t, d, gctx, "user", "dave", "person:dave", "Dave runs the shop")

	out, r := docExec(t, d, gctx, `{"op":"get_document","scope":"tenant","path":"/facts/dave","across_scopes":true}`)
	if r.IsError {
		t.Fatalf("get_document across: %s", r.Text)
	}
	across, _ := out["across_scopes"].([]any)
	if len(across) == 0 {
		t.Fatal("an empty tenant dossier gave no sign that another scope knows the subject")
	}
	first, _ := across[0].(map[string]any)
	if n, _ := first["facts"].(float64); n != 1 {
		t.Errorf("signpost count = %v, want 1", n)
	}
	if _, hasRows := first["fact_rows"]; hasRows {
		t.Error("the dossier signpost carried the fact rows — that is list_facts' job, " +
			"and a dossier read should not pay for them")
	}
	// Unasked-for reads stay byte-identical.
	out2, _ := docExec(t, d, gctx, `{"op":"get_document","scope":"tenant","path":"/facts/dave"}`)
	if _, present := out2["across_scopes"]; present {
		t.Error("a get_document that did not ask for the fan-out got one")
	}
}
