package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Second review of RFC FY P2: the probes, kept.

// moveChunkToDocument re-homes a chunk the way fact homing or a subject merge does:
// the natural key now lives in another document.
func moveChunkToDocument(t *testing.T, e reservedEnv, chunkID, docID string) {
	t.Helper()
	key := mustTenantKey(t, e.d, e.opCtx)
	out, r := docExec(t, e.d, e.opCtx, `{"op":"get_document","scope":"tenant","id":"`+docID+`"}`)
	if r.IsError {
		t.Fatalf("get_document %s: %s", docID, r.Text)
	}
	root, _ := out["root_chunk_id"].(string)
	if err := e.d.exec(context.Background(), key, `UPDATE chunks SET document_id = ?, parent_id = ? WHERE id = ?`, docID, root, chunkID); err != nil {
		t.Fatalf("re-home: %v", err)
	}
}

// TestOntologyGuard_AReKeyedUpsertOutsideTheOntologyIsNotRefused is the reviewer's
// probe for the "addresses must agree" rule: the consolidator names its home document
// beside a natural key that homing has moved elsewhere, and that must keep working —
// the rule is for calls that reach the ontology.
func TestOntologyGuard_AReKeyedUpsertOutsideTheOntologyIsNotRefused(t *testing.T) {
	setup := func(t *testing.T) (reservedEnv, string, string) {
		e := newReservedEnv(t)
		out, r := docExec(t, e.d, e.opCtx, `{"op":"create_document","scope":"tenant","title":"B","path":"/notes/b"}`)
		if r.IsError {
			t.Fatalf("B: %s", r.Text)
		}
		b, _ := out["document_id"].(string)
		out, r = docExec(t, e.d, e.agentCtx, `{"op":"upsert_chunk","scope":"tenant","document_id":"`+e.decoy+`","natural_key":"note:k1","title":"K","body":"one"}`)
		if r.IsError {
			t.Fatalf("first upsert: %s", r.Text)
		}
		chunk, _ := out["id"].(string)
		return e, b, chunk
	}
	again := func(doc string) string {
		return `{"op":"upsert_chunk","scope":"tenant","document_id":"` + doc + `","natural_key":"note:k1","title":"K","body":"two","status":"proposed"}`
	}

	t.Run("neither side is the ontology", func(t *testing.T) {
		e, b, chunk := setup(t)
		moveChunkToDocument(t, e, chunk, b)
		if r := e.doc(t, e.agentCtx, again(e.decoy)); r.IsError {
			t.Errorf("a re-keyed upsert outside the ontology was refused: %s", r.Text)
		}
	})
	t.Run("the key moved into the ontology", func(t *testing.T) {
		e, _, chunk := setup(t)
		moveChunkToDocument(t, e, chunk, e.ontology)
		if r := e.doc(t, e.agentCtx, again(e.decoy)); !r.IsError {
			t.Errorf("an upsert naming a decoy reached an ontology chunk by its key: %s", r.Text)
		}
	})
	t.Run("the call names the ontology", func(t *testing.T) {
		e, b, chunk := setup(t)
		moveChunkToDocument(t, e, chunk, b)
		if r := e.doc(t, e.agentCtx, again(e.ontology)); !r.IsError {
			t.Errorf("an upsert naming the ontology with a key held elsewhere was allowed: %s", r.Text)
		}
	})
}

// TestOntologyGuard_AProposalCannotRewriteAnAcceptedEntity is the reviewer's four-step
// probe: an upsert into the ontology marked `proposed` whose key names an ACCEPTED
// entity rewrote it in place and dropped it back to proposed. Only a new chunk is a
// proposal.
func TestOntologyGuard_AProposalCannotRewriteAnAcceptedEntity(t *testing.T) {
	e := newReservedEnv(t)
	read, _ := e.d.OntologyTermsFromTree(e.opCtx, "tenant", memrank.OntologyPath)
	// 1. The operator writes an accepted entity with a natural key.
	if r := e.doc(t, e.opCtx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"tenant","document_id":"%s","parent_id":"%s","natural_key":"ent:accepted","title":"accepted","body":"orig"}`,
		read.DocumentID, read.RootChunkID)); r.IsError {
		t.Fatalf("operator upsert: %s", r.Text)
	}
	// 2. An agent "proposes" over it by the same key.
	r := e.doc(t, e.agentCtx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"tenant","document_id":"%s","natural_key":"ent:accepted","title":"hijacked","body":"rewritten","status":"proposed"}`,
		read.DocumentID))
	if !r.IsError || !strings.Contains(r.Text, "ontology") {
		t.Errorf("an agent's proposal rewrote an accepted entity: %s", r.Text)
	}
	// 3. The entity is unchanged and still in force.
	after, _ := e.d.OntologyTermsFromTree(e.opCtx, "tenant", memrank.OntologyPath)
	names := map[string]bool{}
	for _, term := range after.Terms {
		names[term.Name] = true
	}
	if !names["accepted"] || names["hijacked"] {
		t.Errorf("the accepted entity changed: terms %v", names)
	}
	// 4. A new key is still a proposal an agent may file, and re-filing its own
	// proposal is still allowed.
	newProp := fmt.Sprintf(`{"op":"upsert_chunk","scope":"tenant","document_id":"%s","parent_id":"%s","natural_key":"ent:new","title":"suggested","body":"x","status":"proposed"}`,
		read.DocumentID, read.RootChunkID)
	if r := e.doc(t, e.agentCtx, newProp); r.IsError {
		t.Errorf("a new proposal was refused: %s", r.Text)
	}
	if r := e.doc(t, e.agentCtx, strings.Replace(newProp, `"body":"x"`, `"body":"y"`, 1)); r.IsError {
		t.Errorf("re-filing an agent's own proposal was refused: %s", r.Text)
	}
}

// TestReservedName_APlantBelowTheEntitiesNameIsNotAdopted: the entity document's
// get-or-create fell back to any document BELOW /memory/entities, so an agent's
// /memory/entities/mine became the document every placed fact lands in, the reserved
// name itself was never registered, and the ensure reported created:true.
func TestReservedName_APlantBelowTheEntitiesNameIsNotAdopted(t *testing.T) {
	e := newReservedEnv(t)
	out, r := docExec(t, e.d, e.agentCtx, `{"op":"create_document","scope":"tenant","title":"Mine","path":"/memory/entities/mine"}`)
	if r.IsError {
		t.Fatalf("plant: %s", r.Text)
	}
	mine, _ := out["document_id"].(string)
	out, r = docExec(t, e.d, e.agentCtx, `{"op":"create_document","scope":"tenant","title":"Entities","path":"/memory/entities"}`)
	if r.IsError {
		t.Fatalf("ensure: %s", r.Text)
	}
	got, _ := out["document_id"].(string)
	if got == mine || got == "" {
		t.Errorf("the ensure adopted the planted document %s", mine)
	}
	if created, _ := out["created"].(bool); !created {
		t.Errorf("the ensure created the entities document but reported created:%v", out["created"])
	}
	if at := e.tenantName(t, "/memory/entities"); at != got {
		t.Errorf("/memory/entities names %q, the ensure answered %q", at, got)
	}
	if r := e.doc(t, withStatementGrant(e.agentCtx, "remember"), `{"op":"remember","scope":"tenant","text":"Ada wrote the first program."}`); r.IsError {
		t.Fatalf("remember: %s", r.Text)
	}
	key := mustTenantKey(t, e.d, e.opCtx)
	res, err := e.d.query(context.Background(), key, `SELECT count(*) FROM chunks WHERE document_id = ?`, mine)
	if err != nil {
		t.Fatal(err)
	}
	if n := asInt(res.Rows[0][0]); n != 1 {
		t.Errorf("remember filed into the planted document (it holds %d chunks, want only its root)", n)
	}
}

// TestReservedName_CreatesAtAnAncestorAreNotRefused: /memory itself is not a reserved
// name. Creating a name there takes nothing the runtime finds documents by, so only
// deletes and moves of an ancestor are judged on the subtree.
func TestReservedName_CreatesAtAnAncestorAreNotRefused(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := tools.WithRunID(tools.UpdateRunPolicy(base, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}
	}), "r_agent")
	p := &Path{Store: d.Store}
	if _, r := pathExec(t, p, ctx, `{"op":"mkdir","scope":"tenant","path":"/memory"}`); r.IsError {
		t.Errorf("mkdir /memory was refused: %s", r.Text)
	}
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Notes","path":"/memory/notes"}`)
	if r.IsError || out["path_warning"] != nil {
		t.Errorf("create_document under /memory: %s %v", r.Text, out["path_warning"])
	}
	// A second tenant tree, so the set does not land on the directory mkdir made.
	d2, base2, _ := documentFixture(t)
	m := &Memory{Store: d2.Store, SqlMem: d2.SqlMem, MaxValueBytes: 65536, DefaultQuotaBytes: 1 << 20}
	mctx := tools.WithRunID(tools.UpdateRunPolicy(base2, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
	}), "r_agent")
	res, err := m.Execute(mctx, []byte(`{"op":"set","scope":"tenant","key":"k","value":"x","path":"/memory"}`))
	if err != nil || res.IsError || strings.Contains(res.Text, "path_warning") {
		t.Errorf("Memory set at /memory: %v %s", err, res.Text)
	}
}

// TestReservedName_TheRootOfAReservedDocumentIsTheOperators: the consolidator writes
// chunks INTO the entities document, so those stay writable; the root's own facets —
// title, status, body, tags, type — name and describe the runtime's document and are
// the operator's.
func TestReservedName_TheRootOfAReservedDocumentIsTheOperators(t *testing.T) {
	e, entities := entitiesEnv(t)
	out, r := docExec(t, e.d, e.opCtx, `{"op":"get_document","scope":"tenant","id":"`+entities+`"}`)
	if r.IsError {
		t.Fatalf("get: %s", r.Text)
	}
	root, _ := out["root_chunk_id"].(string)
	for _, req := range []string{
		`{"op":"update_chunk","scope":"tenant","id":"` + root + `","title":"Mine now","revision":1}`,
		`{"op":"update_chunk","scope":"tenant","id":"` + root + `","status":"archived","revision":1}`,
		`{"op":"delete_chunk","scope":"tenant","id":"` + root + `"}`,
		`{"op":"add_tags","scope":"tenant","id":"` + root + `","tags":["x"]}`,
		`{"op":"add_tags","scope":"tenant","document_id":"` + entities + `","tags":["x"]}`,
		`{"op":"remove_tags","scope":"tenant","document_id":"` + entities + `","tags":["x"]}`,
	} {
		if r := e.doc(t, e.agentCtx, req); !r.IsError || !strings.Contains(r.Text, "reserved name") {
			t.Errorf("an agent wrote the root of a reserved document: %s\n  -> %s", req, r.Text)
		}
	}
	// Chunks under the root stay the consolidator's to write.
	out, r = docExec(t, e.d, e.agentCtx, `{"op":"create_chunk","scope":"tenant","document_id":"`+entities+`","parent_id":"`+root+`","title":"fact","body":"x"}`)
	if r.IsError {
		t.Fatalf("an agent could not write a chunk under the root: %s", r.Text)
	}
	child, _ := out["id"].(string)
	if r := e.doc(t, e.agentCtx, `{"op":"update_chunk","scope":"tenant","id":"`+child+`","body":"y","revision":1}`); r.IsError {
		t.Errorf("an agent could not update a chunk under the root: %s", r.Text)
	}
	// The operator may.
	if r := e.doc(t, e.opCtx, `{"op":"update_chunk","scope":"tenant","id":"`+root+`","title":"Entities","revision":1}`); r.IsError {
		t.Errorf("the operator could not update the root: %s", r.Text)
	}
}

// TestOntologyGuard_ADerivedNaturalKeyIsCheckedToo: upsert_chunk may omit natural_key
// and let the class derive it from its identity fields. The guard must judge the key
// the handler will use, not the empty one the caller sent, or a "proposal" rewrites an
// accepted entity whose identity it repeats.
func TestOntologyGuard_ADerivedNaturalKeyIsCheckedToo(t *testing.T) {
	d, base, _ := documentFixture(t)
	opCtx := installTenantOntology(t, d, base, typedOntologyMD, memrank.OntologyConfirmedStatus)
	agentCtx := tools.WithRunID(tools.UpdateRunPolicy(base, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}
	}), "r_agent")
	read, _ := d.OntologyTermsFromTree(opCtx, "tenant", memrank.OntologyPath)

	// The operator files an accepted Provision in the ontology; the class derives its key.
	out, r := docExec(t, d, opCtx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"tenant","document_id":"%s","parent_id":"%s","type":"Provision","title":"Art. 5","body":"orig","fields":{"anchor":"art-5"}}`,
		read.DocumentID, read.RootChunkID))
	if r.IsError {
		t.Fatalf("operator upsert: %s", r.Text)
	}
	accepted, _ := out["id"].(string)

	// An agent repeats the identity with no natural_key, marked proposed.
	r = docExec2(t, d, agentCtx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"tenant","document_id":"%s","type":"Provision","title":"hijacked","body":"rewritten","status":"proposed","fields":{"anchor":"art-5"}}`,
		read.DocumentID))
	if !r.IsError {
		t.Errorf("a proposal with a derived key rewrote an accepted entity: %s", r.Text)
	}
	got, gr := docExec(t, d, opCtx, `{"op":"get_chunk","scope":"tenant","id":"`+accepted+`"}`)
	if gr.IsError || got["title"] != "Art. 5" || got["status"] != nil && got["status"] != "" {
		t.Errorf("the accepted entity changed: %v", got)
	}
}

// TestReservedName_EveryRootWriteIsTheOperators: the ops that rewrite a document as a
// whole (a reimport, a sync from a peer, binding it to one, declaring its types,
// classifying it) and an upsert whose key names the root all reach the root of a
// reserved document; each is refused for an agent and allowed to the operator.
func TestReservedName_EveryRootWriteIsTheOperators(t *testing.T) {
	md, _ := json.Marshal("# Entities\n\n## planted\nx\n")
	cases := []struct {
		name string
		req  func(doc string) string
	}{
		{"import_md reimport by document_id", func(doc string) string {
			return `{"op":"import_md","scope":"tenant","document_id":"` + doc + `","reimport":true,"markdown":` + string(md) + `}`
		}},
		{"import_md reimport by path", func(string) string {
			return `{"op":"import_md","scope":"tenant","path":"/memory/entities","reimport":true,"markdown":` + string(md) + `}`
		}},
		{"upsert_chunk by the root's natural key", func(doc string) string {
			return `{"op":"upsert_chunk","scope":"tenant","document_id":"` + doc + `","natural_key":"ent:entities-root","title":"Mine","body":"rewritten"}`
		}},
		{"sync", func(doc string) string { return `{"op":"sync","scope":"tenant","document_id":"` + doc + `"}` }},
		{"set_remote", func(doc string) string {
			return `{"op":"set_remote","scope":"tenant","document_id":"` + doc + `","source":"peer","remote_ref":"/x"}`
		}},
		{"define_type", func(doc string) string {
			return `{"op":"define_type","scope":"tenant","document_id":"` + doc + `","name":"thing","fields":["a"]}`
		}},
		{"classify (proposes by default)", func(doc string) string {
			return `{"op":"classify","scope":"tenant","document_id":"` + doc + `"}`
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newReservedEnv(t)
			out, r := docExec(t, e.d, e.opCtx, `{"op":"create_document","scope":"tenant","title":"Entities","path":"/memory/entities","natural_key":"ent:entities-root","type":"object","subject":"Entities"}`)
			if r.IsError {
				t.Fatalf("entities: %s", r.Text)
			}
			doc, _ := out["document_id"].(string)
			if r := e.doc(t, e.agentCtx, c.req(doc)); !r.IsError || !strings.Contains(r.Text, "reserved name") {
				t.Errorf("an agent's %s reached the root of a reserved document: %s", c.name, r.Text)
			}
			if r := e.doc(t, e.opCtx, c.req(doc)); r.IsError && strings.Contains(r.Text, "reserved name") {
				t.Errorf("the operator's %s was refused as a reserved-name write: %s", c.name, r.Text)
			}
		})
	}
}

// faultyDefaultTenant fails every dirent read under the raw tenant "default", which is
// one of the two raw tenants SQL Memory's "default" can stand for.
type faultyDefaultTenant struct{ store.Store }

func (f faultyDefaultTenant) DirentGet(ctx context.Context, tenant, scope, scopeID, parent, name string) (store.DirentRow, error) {
	if tenant == "default" {
		return store.DirentRow{}, fmt.Errorf("injected store fault")
	}
	return f.Store.DirentGet(ctx, tenant, scope, scopeID, parent, name)
}

// TestOntology_BootPassTriesEveryRawTenantAfterAnError: SQL Memory stores the raw
// tenant "" as "default", so the boot pass tries both raw keys. An error on the first
// stopped it before the second, which is where a default-tenant deployment's ontology
// actually is.
func TestOntology_BootPassTriesEveryRawTenantAfterAnError(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a", UserID: "u1", TenantID: ""})
	ctx = tools.WithCallerTier(tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}}
	}), tools.TierOperatorPlane)
	md, _ := json.Marshal("# Tenant Ontology\n\n## event\n")
	out, r := docExec(t, d, ctx, `{"op":"import_md","scope":"tenant","path":"/memory/ontology","markdown":`+string(md)+`}`)
	if r.IsError {
		t.Fatalf("import_md: %s", r.Text)
	}
	ontology, _ := out["document_id"].(string)
	key := mustTenantKey(t, d, ctx)
	if _, err := d.docSQL().Exec(context.Background(), key, `DELETE FROM document_roles`, nil, 0); err != nil {
		t.Fatal(err)
	}
	faulty := &Document{Store: faultyDefaultTenant{d.Store}, SqlMem: d.SqlMem}
	if n, _ := faulty.RecordTenantOntologies(context.Background()); n != 1 {
		t.Errorf("boot pass found %d ontologies, want 1", n)
	}
	if got, err := d.recordedRole(context.Background(), key, roleOntology); err != nil || got != ontology {
		t.Errorf("recorded ontology = %q (err %v), want %q", got, err, ontology)
	}
}

// TestOntology_BootPassDoesNotProvisionADocumentPlane: a tenant scope that holds only
// an agent's own SQL has no document tables, and the boot pass must not create a
// document-plane schema and role for it — not even when a name sits at
// /memory/ontology (a dangling one, or a memory entry from before the names were
// reserved).
func TestOntology_BootPassDoesNotProvisionADocumentPlane(t *testing.T) {
	d, ctx := pgDocumentOrSkip(t)
	key := mustTenantKey(t, d, ctx)
	if _, err := d.SqlMem.Exec(context.Background(), key, `CREATE TABLE notes (x TEXT)`, nil, 0); err != nil {
		t.Fatalf("agent table: %v", err)
	}
	if ok, err := d.docSQL().PlaneExists(context.Background(), key); err != nil || ok {
		t.Fatalf("the fixture already has a document plane (%v, %v)", ok, err)
	}
	if _, err := d.Store.DirentCreate(context.Background(), store.DirentRow{TenantID: "tnt", Scope: "tenant",
		ParentPath: "/memory/", Name: "ontology", Kind: "document", ResourceRef: []byte(`{"document_id":"gone"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RecordTenantOntologies(context.Background()); err != nil {
		t.Fatalf("boot pass: %v", err)
	}
	if ok, err := d.docSQL().PlaneExists(context.Background(), key); err != nil || ok {
		t.Errorf("the boot pass provisioned a document plane for a scope that had none (exists=%v err=%v)", ok, err)
	}
}
