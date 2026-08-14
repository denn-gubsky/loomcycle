package builtin

// document_ontology_guard.go — an agent may PROPOSE an entity type, never decide one
// (RFC CA phase 2).
//
// THE HOLE THIS CLOSES. Phase 1 made a proposal inert and gave the operator accept /
// reject on a dedicated route. That narrowing was real for the UI and unenforced
// underneath: `scope=tenant` on the Document tool is write authority over the whole
// ontology document, so an agent holding it could create a live type outright, or clear
// its own proposal's status and call the result reviewed. A review gate that the thing
// being reviewed can open is ceremony.
//
// So on the TENANT ONTOLOGY DOCUMENT, and only there, an agent's tool call may:
//
//	create_chunk  — only with status `proposed` (an inert suggestion)
//	update_chunk  — refused
//	delete_chunk  — refused
//
// The operator's own surfaces are unaffected: the confirm toggle, accept/reject, adopt,
// and the Web UI's document editor all run with the operator marker stamped, which no
// run path sets and no wire field can claim.
//
// WHY NOT KEY ON THE IDENTITY the off-run path already stamps: it uses a synthetic agent
// id, and `POST /v1/runs` accepts a caller-supplied `agent_id`, so a run could claim that
// value and be read as the operator. The marker is a context value with no wire form.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// refusal wraps an error result so the guard reads as a gate rather than a failure.
func refusal(msg string) *tools.Result {
	r := errResult(msg)
	return &r
}

// proposalBodyMax bounds the evidence a single proposal can carry.
//
// The body reaches an operator's config document and the panel renders it, so an
// unbounded one is a way to make the ontology page unreadable. Generous enough for
// counts and a few example facts, which is what evidence looks like.
const proposalBodyMax = 4000

// guardOntologyWrite refuses an agent's attempt to author or resolve a live entity.
//
// Returns a non-nil result when the call must be refused. Cheap on the common path: the
// ontology document only exists at tenant scope, so a call on any other scope returns
// immediately without touching the store.
func (d *Document) guardOntologyWrite(ctx context.Context, key sqlmem.ScopeKey, op string, in docInput) *tools.Result {
	if tools.IsSubstrateOperator(ctx) || key.Scope != "tenant" {
		return nil
	}
	docID := d.ontologyDocumentID(ctx, key)
	if docID == "" {
		return nil // no ontology document in this tenant — nothing to protect
	}
	// Which document does this call touch? Three addressings, and missing one is how a
	// guard gets bypassed: create/import name the document, the chunk ops name a chunk
	// whose document must be looked up, and delete_document/set_path put the DOCUMENT id
	// in `id` — where a chunk lookup returns nothing and the check would sail past.
	target := strings.TrimSpace(in.DocumentID)
	if target == "" && in.ID != "" {
		if target = d.documentIDForChunk(ctx, key, in.ID); target == "" {
			target = strings.TrimSpace(in.ID)
		}
	}
	if target != docID {
		return nil
	}

	switch op {
	case "create_chunk", "upsert_chunk":
		if !strings.EqualFold(strings.TrimSpace(in.Status), memrank.OntologyStatusProposed) {
			return refusal("an agent may only add a PROPOSED entity to the ontology: pass " +
				"status=\"" + memrank.OntologyStatusProposed + "\" (or use op=propose_entity, which " +
				"does it for you). An entity that is in force is the operator's decision, and " +
				"accepting a proposal is a separate operator action.")
		}
		return nil
	// EVERY op that can create, alter, retire or re-home an entity — audited from the op
	// list rather than from the obvious ones. The first version of this guard covered the
	// five chunk mutations and missed four routes to the same effect: import_md and
	// import_canvas author chunks wholesale, supersede_chunk both creates and retires, and
	// delete_document / set_path reach the WHOLE ontology (set_path by re-homing
	// /memory/ontology so the reader resolves somewhere else entirely).
	case "update_chunk", "delete_chunk", "move_chunk", "reorder_chunk",
		"supersede_chunk", "import_md", "import_canvas", "delete_document", "set_path":
		return refusal("an agent may not modify the ontology document — it may only add a " +
			"proposal (op=propose_entity). Resolving one, editing a live entity, importing " +
			"over the document, re-homing it, or removing anything is the operator's decision.")
	default:
		return nil
	}
}

// ontologyDocumentID resolves the tenant ontology document, or "" when there is none.
func (d *Document) ontologyDocumentID(ctx context.Context, key sqlmem.ScopeKey) string {
	id, err := d.docIDFromInput(ctx, key, docInput{Path: memrank.OntologyPath})
	if err != nil {
		return ""
	}
	return id
}

// documentIDForChunk returns the document a chunk belongs to, or "".
func (d *Document) documentIDForChunk(ctx context.Context, key sqlmem.ScopeKey, chunkID string) string {
	res, err := d.query(ctx, key, `SELECT document_id FROM chunks WHERE id = ? LIMIT 1`, chunkID)
	if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return ""
	}
	return asStr(res.Rows[0][0])
}

// proposeEntity files an inert entity proposal against the tenant ontology.
//
// The ergonomic front door to what create_chunk can already do under the guard above,
// and it exists because the fiddly parts are exactly where a curator would get it wrong:
// finding the ontology document, addressing the parent, and remembering the status. It
// takes the parent by NAME — which is what the agent read in its prompt — rather than by
// chunk id, which it has no way to know.
//
// NO TENANT GRANT REQUIRED, deliberately. A proposal cannot change what any run is told,
// so it does not need the authority that live authoring needs; requiring the grant would
// mean only an agent that could already author live types could suggest one, which
// inverts the point. The tenant comes from the run's server-stamped identity, so nothing
// here crosses a tenant.
func (d *Document) proposeEntity(ctx context.Context, in docInput) (tools.Result, error) {
	name := memrank.TrimOntologyName(in.Name)
	if name == "" {
		name = memrank.TrimOntologyName(in.Title)
	}
	if name == "" {
		return errResult("propose_entity: name is required — it becomes the entity type's name"), nil
	}
	if len(in.Body) > proposalBodyMax {
		return errResult(fmt.Sprintf("propose_entity: body is %d bytes, over the %d-byte limit — "+
			"keep the evidence to counts and a few examples", len(in.Body), proposalBodyMax)), nil
	}

	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return errResult("propose_entity: " + err.Error()), nil
	}
	read, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath)
	if err != nil {
		return errResult("propose_entity: " + err.Error()), nil
	}
	if read.DocumentID == "" || read.RootChunkID == "" {
		return errResult("propose_entity: this tenant has no ontology document yet — an " +
			"operator opens Settings → Ontology once to create it"), nil
	}
	for _, t := range read.Terms {
		if strings.EqualFold(t.Name, name) {
			return errResult("propose_entity: " + name + " is already in force — propose a " +
				"SUBTYPE of it (parent=\"" + t.Name + "\") or leave it alone"), nil
		}
	}
	for _, p := range read.Proposals {
		if strings.EqualFold(p.Name, name) {
			// Naming the status matters: a `rejected` twin means the operator already
			// said no, and re-filing it is the nagging the tombstone exists to prevent.
			return errResult("propose_entity: " + name + " is already " + p.Status +
				" — an operator has seen it"), nil
		}
	}

	// The parent is resolved by NAME against what is IN FORCE. A proposal under a
	// proposal would be a suggestion contingent on another suggestion, which the
	// operator cannot evaluate on its own.
	parentChunk := read.RootChunkID
	if want := memrank.TrimOntologyName(in.Parent); want != "" {
		id := d.chunkIDForEntity(ctx, key, read.DocumentID, want)
		if id == "" {
			names := make([]string, 0, len(read.Terms))
			for _, t := range read.Terms {
				names = append(names, t.Name)
			}
			return errResult("propose_entity: no entity named " + want + " is in force in this " +
				"tenant's ontology (have: " + strings.Join(names, ", ") + "). A subtype must " +
				"hang off a type the operator has already accepted"), nil
		}
		parentChunk = id
	}
	return d.createOntologyProposal(ctx, key, mscope, read.DocumentID, parentChunk, name, in.Body)
}

// chunkIDForEntity finds the chunk backing an in-force entity by name.
func (d *Document) chunkIDForEntity(ctx context.Context, key sqlmem.ScopeKey, docID, name string) string {
	res, err := d.query(ctx, key,
		`SELECT id, coalesce(status, '') FROM chunks
		  WHERE document_id = ? AND lower(coalesce(title, '')) = lower(?) LIMIT 1`, docID, name)
	if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) < 2 {
		return ""
	}
	if memrank.IsInertEntityStatus(asStr(res.Rows[0][1])) {
		return "" // an inert chunk is not a parent an operator can reason about
	}
	return asStr(res.Rows[0][0])
}

// ontologyTenantKey builds the tenant scope key WITHOUT the scope=tenant grant check.
//
// Same carve-out the retrieval-side ontology read uses, for the same reason: the tenant
// comes from the run's server-stamped identity, and what is reached is operator-authored
// config that already appears in the agent's own prompt. What differs is the WRITE, which
// is bounded to inert proposals by the guard above — an agent cannot use this to author a
// live type.
func (d *Document) ontologyTenantKey(ctx context.Context) (sqlmem.ScopeKey, store.MemoryScope, error) {
	if d.Store == nil || d.SqlMem == nil {
		return sqlmem.ScopeKey{}, "", fmt.Errorf("the ontology needs SQL Memory (set LOOMCYCLE_SQLMEM_ENABLED=1)")
	}
	t := sqlScopeTenant(ctx)
	return sqlmem.ScopeKey{Tenant: t, Scope: "tenant", ScopeID: t}, store.MemoryScopeTenant, nil
}

// createOntologyProposal writes the inert chunk.
//
// Status is stamped HERE rather than taken from the caller: propose_entity's whole
// contract is that what it produces is not in force, and a status argument would be a way
// to spell that contract wrong.
func (d *Document) createOntologyProposal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, docID, parentID, name, body string) (tools.Result, error) {
	// CALLED DIRECTLY, not through Execute, and this is the whole no-grant design.
	//
	// Execute resolves the scope from the request and grant-checks it, so routing a
	// `scope: tenant` create back through it asked for the very authority propose_entity
	// exists to avoid — a live run failed with "scope=tenant is not granted to this
	// agent" while every unit test passed, because the test fixture granted tenant scopes
	// to exercise the guard. The key here came from ontologyTenantKey, which resolves the
	// caller's own tenant without the check; the write is bounded to an inert entity by
	// stamping the status below rather than by a policy gate.
	res, err := d.createChunk(ctx, key, mscope, docInput{
		Scope: "tenant", DocumentID: docID, ParentID: parentID,
		Title: name, Status: memrank.OntologyStatusProposed, Body: body,
	})
	if err != nil {
		return errResult("propose_entity: " + err.Error()), nil
	}
	if res.IsError {
		return res, nil
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	return jsonResult(map[string]any{
		"proposed":  name,
		"chunk_id":  out["id"],
		"parent_id": parentID,
		"note": "Filed as a suggestion, not in force. An operator accepts or rejects it in " +
			"Settings → Ontology; nothing changes for any run until they do.",
	})
}

// gateEntityType refuses an entity assertion typed as something the ontology does not
// declare.
//
// GATED ON THE PAIR — type AND subject — which is the discriminator this check needs and
// the reason `subject` is now a field. The first version keyed on "a natural_key is
// present, therefore this is a fact", and the existing suite refused it inside one run:
// an idempotent DOCUMENT write carries a natural key and a type too, because `type` holds
// a document's own structural vocabulary (rfc, section, image) in the same column. That
// gate would have refused a documentation store for using its own words.
//
// A document write carries no subject and never will, so the pair separates the two
// populations by construction rather than by heuristic.
//
// WHY IN THE TOOL. The consolidator already refuses two statement classes as entity
// types, and that reaches exactly one writer — an agent calling upsert_chunk, an MCP
// session or a curator writes an entity node with no check at all. A rule one caller
// enforces is a convention; the tool is where it becomes a rule.
//
// NOT A DENY-LIST, and this is load-bearing: `preference` and `fact` are seed types the
// memory tier needs as chunk types, and the consolidator writes its STATEMENT node as
// type "fact". Denying them here would break the pipeline this RFC exists to protect.
// Whether a seed type is being misused as a SUBJECT's type is a distinction only the
// writer knows, so that check stays where it already works.
//
// FAILS OPEN. No SQL Memory, no ontology document, an unreadable one: the write proceeds.
// A verification feature that refuses writes when it cannot verify is an outage.
func (d *Document) gateEntityType(ctx context.Context, in docInput) *tools.Result {
	typ := strings.TrimSpace(in.Type)
	if typ == "" || strings.TrimSpace(in.Subject) == "" {
		return nil // not an entity assertion — nothing the ontology governs
	}
	tenantTerms, confirmed, hasOntology := d.tenantOntologyState(ctx)
	// FAILS OPEN, keyed on the DOCUMENT rather than on the term list. The seed is
	// compiled in, so an empty tenant layer still yields seven types — a gate reading
	// only that would enforce a vocabulary nobody chose, and every deployment that never
	// opened the ontology would start refusing writes it accepts today.
	if !hasOntology {
		return nil
	}
	effective := memrank.EffectiveOntology(tenantTerms, confirmed)
	if len(effective) == 0 {
		return nil
	}
	declared := make([]string, 0, len(effective))
	for _, t := range effective {
		if strings.EqualFold(t.Name, typ) {
			return nil
		}
		declared = append(declared, t.Name)
	}
	sort.Strings(declared)
	return refusal("upsert_chunk: " + strconv.Quote(typ) + " is not an entity type this tenant " +
		"declares, so an assertion typed with it becomes a node nobody can find. Declared: " +
		strings.Join(declared, ", ") + ". Use one of those, drop the type/subject pair to store " +
		"the claim without an entity node, or propose the type with op=propose_entity and let an " +
		"operator accept it.")
}
