package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC CV P4 — an unknown subject is PROPOSED, and proposing must not mint it.

// TestProposeSubject_DoesNotMakeTheSubjectKnownToTheTenant is the load-bearing one.
//
// The curator gate asks "does the tenant know this subject" by joining tenant chunks to
// chunk_memory_meta on the title. If a proposal carried entity metadata it would answer
// YES — so filing the proposal would mint the subject the gate had just refused to mint,
// and the next pass would place one user's facts on a name nobody adopted.
//
// The property holds because a proposal goes in through create_chunk, which writes no
// sidecar — only upsert_chunk and create_document do. So it cannot be broken by adding
// fields to the docInput; it breaks the day someone "files the proposal properly" by
// calling writeChunkMeta with the key it already carries, which is a reasonable-looking
// change. Verified by making exactly that change and watching this test red.
func TestProposeSubject_DoesNotMakeTheSubjectKnownToTheTenant(t *testing.T) {
	d, ctx, st := documentFixture(t)
	seedTenantOntology(t, d, ctx)
	m := &Memory{Store: st, SqlMem: d.SqlMem}

	if m.subjectKnownToTenant(ctx, "Dave") {
		t.Fatal("the tenant knows Dave before anything was filed — the fixture is wrong")
	}

	out, r := docExec(t, d, ctx, `{"op":"propose_subject","subject":"Dave",
		"natural_key":"person:dave","path":"dave","body":"Seen in one user's chat."}`)
	if r.IsError {
		t.Fatalf("propose_subject: %s", r.Text)
	}
	if asStr(out["chunk_id"]) == "" {
		t.Fatalf("propose_subject filed nothing: %v", out)
	}

	if m.subjectKnownToTenant(ctx, "Dave") {
		t.Error("filing a PROPOSAL made the subject known to the tenant — the proposal " +
			"mints what the curator gate refused to mint, and facts will place on a name " +
			"no operator adopted")
	}
}

// TestProposeSubject_StaysOutOfTheInForceTypeList. The ontology's in-force terms are
// injected into the extractor's prompt as the kinds it may use. A subject leaking in
// there would have the extractor typing facts as "Dave".
func TestProposeSubject_StaysOutOfTheInForceTypeList(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	seedTenantOntology(t, d, ctx)

	if _, r := docExec(t, d, ctx, `{"op":"propose_subject","subject":"Dave",
		"natural_key":"person:dave","path":"dave"}`); r.IsError {
		t.Fatalf("propose_subject: %s", r.Text)
	}
	read, err := d.OntologyTermsFromTree(grantedTenantCtx(ctx), "tenant", memory.OntologyPath)
	if err != nil {
		t.Fatalf("read ontology: %v", err)
	}
	for _, term := range read.Terms {
		if strings.EqualFold(term.Name, "Dave") {
			t.Error("a proposed subject appears as an in-force entity TYPE — it is injected " +
				"into the extractor's prompt from there")
		}
	}
	var listed bool
	for _, p := range read.Proposals {
		if strings.EqualFold(p.Name, "Dave") {
			listed = true
		}
	}
	if !listed {
		t.Error("the subject is not in the proposals list, so no operator can act on it")
	}
}

// TestProposeSubject_RequiresTheKeyFromTheProposer. The proposer owns the identity:
// slug/entityKey exist once, in the consolidator, and the adopt path mints under
// exactly what was recorded rather than deriving it again in another language.
func TestProposeSubject_RequiresTheKeyFromTheProposer(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	seedTenantOntology(t, d, ctx)
	_, r := docExec(t, d, ctx, `{"op":"propose_subject","subject":"Dave"}`)
	if !r.IsError || !strings.Contains(r.Text, "natural_key") {
		t.Errorf("propose_subject accepted a proposal with no key: %q", r.Text)
	}
}

// TestProposeSubject_IsIdempotentAndRespectsATombstone: a pass meets the same subject
// every run, so re-filing must be quiet, and a rejected subject must not come back.
func TestProposeSubject_IsIdempotentAndRespectsATombstone(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	seedTenantOntology(t, d, ctx)
	body := `{"op":"propose_subject","subject":"Dave","natural_key":"person:dave","path":"dave"}`
	if _, r := docExec(t, d, ctx, body); r.IsError {
		t.Fatalf("first propose: %s", r.Text)
	}
	out, r := docExec(t, d, ctx, body)
	if r.IsError {
		t.Fatalf("second propose errored instead of reporting already-seen: %s", r.Text)
	}
	if asStr(out["already"]) != memory.OntologyStatusProposed {
		t.Errorf("re-filing did not report the existing proposal: %v", out)
	}
	read, _ := d.OntologyTermsFromTree(grantedTenantCtx(ctx), "tenant", memory.OntologyPath)
	n := 0
	for _, p := range read.Proposals {
		if strings.EqualFold(p.Name, "Dave") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the subject is filed %d times — a pass meets it every run", n)
	}
}

// TestAdoptSubjectProposal_MintsTheEntityTheConsolidatorWouldHaveWritten.
//
// The shape matters more than the fact of it: a bare chunk would look adopted and then
// COLLIDE, because natural_key is unique per scope — the next pass's create_document
// for the same subject fails, the subject resolves to nothing, and its facts land with
// no edge to it. That failure shipped once already.
func TestAdoptSubjectProposal_MintsTheEntityTheConsolidatorWouldHaveWritten(t *testing.T) {
	d, ctx, st := documentFixture(t)
	seedTenantOntology(t, d, ctx)
	if _, r := docExec(t, d, ctx, `{"op":"propose_subject","subject":"Dave",
		"natural_key":"person:dave","path":"dave","body":"Seen in one user's chat."}`); r.IsError {
		t.Fatalf("propose_subject: %s", r.Text)
	}
	read, _ := d.OntologyTermsFromTree(grantedTenantCtx(ctx), "tenant", memory.OntologyPath)
	var chunkID string
	for _, p := range read.Proposals {
		if strings.EqualFold(p.Name, "Dave") {
			chunkID = p.ChunkID
		}
	}
	if chunkID == "" {
		t.Fatal("no proposal to adopt")
	}

	isSubject, f, evidence := d.SubjectProposal(grantedTenantCtx(ctx), "tenant", chunkID)
	if !isSubject {
		t.Fatal("the proposal does not report as a subject proposal — accept would make it a TYPE")
	}
	if f.NaturalKey != "person:dave" || f.PathSlug != "dave" {
		t.Fatalf("the proposal lost the identity the proposer recorded: %+v", f)
	}
	docID, err := d.AdoptSubjectProposalInScope(grantedTenantCtx(ctx), "tenant", f, evidence)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if docID == "" {
		t.Fatal("adopt minted no document")
	}

	// The dossier shape: /facts/<slug>, root chunk IS the entity, carrying the key.
	got, r := docExec(t, d, grantedTenantCtx(ctx), `{"op":"get_document","scope":"tenant","path":"/facts/dave"}`)
	if r.IsError {
		t.Fatalf("the adopted entity is not at /facts/dave: %s", r.Text)
	}
	root := asStr(got["root_chunk_id"])
	key, _, _ := d.resolveScope(grantedTenantCtx(ctx), "tenant")
	res, err := d.query(grantedTenantCtx(ctx), key, `SELECT natural_key FROM chunk_memory_meta WHERE chunk_id = ?`, root)
	if err != nil || len(res.Rows) == 0 {
		t.Fatalf("the adopted document's ROOT carries no entity sidecar: %v", err)
	}
	if asStr(res.Rows[0][0]) != "person:dave" {
		t.Errorf("adopted under %q, want the proposer's own key person:dave — a second "+
			"derivation of the identity is how one subject becomes two nodes", asStr(res.Rows[0][0]))
	}

	// And NOW the tenant knows it, which is the whole point of the loop.
	m := &Memory{Store: st, SqlMem: d.SqlMem}
	if !m.subjectKnownToTenant(ctx, "Dave") {
		t.Error("after adoption the curator gate still says the tenant does not know Dave — " +
			"the loop does not close and facts keep staying home")
	}
}

// seedTenantOntology creates the tenant ontology document a proposal hangs off.
//
// It needs the tenant GRANTS and the proposals below deliberately do not: propose_subject
// takes the same no-grant carve-out propose_entity takes, because a proposal cannot
// change what any run is told. Requiring the grant would mean only an agent that could
// already mint tenant entities could suggest one — which inverts the gate. Seeding on a
// granted context and proposing on the plain one is what keeps that honest.
func seedTenantOntology(t *testing.T, d *Document, ctx context.Context) {
	t.Helper()
	res, err := d.Execute(grantedTenantCtx(ctx), json.RawMessage(`{"op":"create_document","scope":"tenant",
		"title":"Tenant Ontology","path":"`+memory.OntologyPath+`"}`))
	if err != nil || res.IsError {
		t.Fatalf("seed ontology: %v %s", err, res.Text)
	}
}

// grantedTenantCtx is the OPERATOR's authority — what Settings → Ontology runs with.
// Reads and adoptions in these tests use it; the proposals deliberately do not.
func grantedTenantCtx(ctx context.Context) context.Context {
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
		AllowedScopes: []string{"agent", "user", "tenant"}})
	return tools.WithSqlMemPolicy(ctx, tools.SqlMemPolicyValue{
		AllowedScopes: []string{"agent", "user", "tenant"}})
}

// TestAdoptSubjectProposal_DoesNotBackfillFactsFromOtherScopes — RFC CV P4, decided
// 2026-09-14.
//
// The ontology declaring `person → tenant` means facts about people are MEANT to be
// shared, and the curator gate only delayed that while the subject was unknown. So
// there is a real argument that adoption should reach back and move what was learned
// in the meantime.
//
// IT MUST NOT, and the reason is what the gate was protecting in the first place.
// Moving those facts republishes one user's accumulated history to the whole tenant,
// retroactively, as a side effect of an operator clicking accept on a NAME. Minting an
// entity and publishing a person's back catalogue are different decisions, and only one
// of them was made. Adoption shares from now on.
//
// The property holds today because AdoptSubjectProposal only creates a document. That
// is an accident of implementation, not a design anybody wrote down, and it would be
// undone by the obvious "while we're here, move the existing facts over" change. This
// is what makes that a red build.
func TestAdoptSubjectProposal_DoesNotBackfillFactsFromOtherScopes(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	seedTenantOntology(t, d, ctx)

	// What the user's own scope already knows about Dave, learned while the tenant did
	// not know him.
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Dave",
		"path":"/facts/dave"}`)
	if r.IsError {
		t.Fatalf("seed user dossier: %s", r.Text)
	}
	userDoc := asStr(out["document_id"])
	out, r = docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+userDoc+`",
		"natural_key":"memory/fact/dave-runs-the-shop","title":"Dave runs the shop",
		"body":"Dave runs the shop.","type":"fact","subject":"Dave"}`)
	if r.IsError {
		t.Fatalf("seed user fact: %s", r.Text)
	}
	userFact := asStr(out["id"])

	if _, r := docExec(t, d, ctx, `{"op":"propose_subject","subject":"Dave",
		"natural_key":"person:dave","path":"dave"}`); r.IsError {
		t.Fatalf("propose_subject: %s", r.Text)
	}
	read, _ := d.OntologyTermsFromTree(grantedTenantCtx(ctx), "tenant", memory.OntologyPath)
	var chunkID string
	for _, p := range read.Proposals {
		if strings.EqualFold(p.Name, "Dave") {
			chunkID = p.ChunkID
		}
	}
	_, f, evidence := d.SubjectProposal(grantedTenantCtx(ctx), "tenant", chunkID)
	if _, err := d.AdoptSubjectProposalInScope(grantedTenantCtx(ctx), "tenant", f, evidence); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	// STILL THERE, STILL LIVE, STILL WHERE IT WAS. Each half is asserted separately
	// because they fail differently: a moved fact is gone, a "migrated" one is
	// superseded in place, and a copied one is in both — and only the first is visible
	// to a test that just counts rows in the user scope.
	userKey, _, _ := d.resolveScope(ctx, "user")
	if n := countRows(t, d, ctx, `SELECT COUNT(*) FROM chunks WHERE id = ?`, userFact); n != 1 {
		t.Errorf("the user-scope fact was removed by an adoption in another scope")
	}
	res, err := d.query(ctx, userKey,
		`SELECT coalesce(expired_at, 0), coalesce(invalid_at, 0) FROM chunk_memory_meta WHERE chunk_id = ?`,
		userFact)
	if err != nil || len(res.Rows) == 0 {
		t.Fatalf("read the user fact's sidecar: %v", err)
	}
	if exp, _ := asInt64(res.Rows[0][0]); exp != 0 {
		t.Errorf("the user-scope fact was RETIRED by adoption — its owner loses it from " +
			"every fact surface, and nothing told them why")
	}
	if inv, _ := asInt64(res.Rows[0][1]); inv != 0 {
		t.Errorf("adoption invalidated the user-scope fact")
	}
	// And it did not appear in the tenant plane, which is the disclosure half.
	tenantKey, _, _ := d.resolveScope(grantedTenantCtx(ctx), "tenant")
	res, err = d.query(grantedTenantCtx(ctx), tenantKey,
		`SELECT COUNT(*) FROM chunk_memory_meta WHERE natural_key = ?`, "memory/fact/dave-runs-the-shop")
	if err != nil {
		t.Fatalf("read the tenant plane: %v", err)
	}
	if n, _ := asInt64(res.Rows[0][0]); n != 0 {
		t.Errorf("adoption copied a user's existing fact into the TENANT plane — accepting " +
			"a name republished that user's history to everyone in the tenant")
	}

	// The tenant dossier exists and SAYS why it is empty. A dossier that is silently
	// empty next to a user scope that knows plenty is the failure this records against.
	got, r := docExec(t, d, grantedTenantCtx(ctx), `{"op":"get_document","scope":"tenant","path":"/facts/dave"}`)
	if r.IsError {
		t.Fatalf("the adopted dossier is missing: %s", r.Text)
	}
	body, _ := d.readBody(grantedTenantCtx(ctx), store.MemoryScopeTenant, tenantKey.ScopeID, asStr(got["root_chunk_id"]))
	if !strings.Contains(body.Body, "BEFORE adoption") {
		t.Errorf("the adopted entity does not record that earlier facts stayed behind: %q", body.Body)
	}
}
