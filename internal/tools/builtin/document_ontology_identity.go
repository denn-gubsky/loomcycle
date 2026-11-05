package builtin

// document_ontology_identity.go — the tenant ontology is a document identity, not a
// name (RFC FY Part C, decision D3=(b)).
//
// THE HOLE THIS CLOSES. The ontology guard found its document by the Path name
// /memory/ontology. A name is not an identity: an agent that could take the name off
// the document (Path rm / mv) and put another document under it (create_document,
// import_md, set_path) chose which document every run in the tenant reads as its
// vocabulary, and the guard then protected the impostor and left the real one open.
//
// Two halves, each useless alone:
//
//   - The ontology's document id is RECORDED in the tenant scope's own database when
//     the runtime creates the document or first finds it, and every reader resolves
//     by that id. The name only migrates a tenant that has no record yet.
//   - The names the runtime finds tenant-wide documents by are RESERVED: only the
//     operator's own off-run call may create, rename or remove them. That is what
//     keeps the one-time migration from being front-run, and what keeps an agent
//     from re-pointing the names that have no recorded id.
//
// WHY A TABLE IN THE SCOPE'S DATABASE rather than a store-level tenant setting: the
// id means something only beside the documents it names, so it belongs to the same
// database, snapshot, export and erasure. Every table docSchemaDDL creates is also
// out of reach of an agent's SQL (memory_sql_doc_guard.go), so the record is no
// easier to rewrite than the ontology itself. A store migration would have added a
// second plane to keep in step with the first, for one row per tenant.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// roleOntology is document_roles.role for the tenant ontology.
const roleOntology = "ontology"

// documentRolesDDL holds which document plays a runtime role in this scope.
const documentRolesDDL = `CREATE TABLE IF NOT EXISTS document_roles (
		role TEXT PRIMARY KEY, document_id TEXT NOT NULL, recorded_at BIGINT NOT NULL)`

// reservedNameWriter reports whether ctx may create, rename or remove a reserved
// tenant name: the operator's own off-run call. The marker counts only off-run, as
// in the ontology guard — the spawn boundary strips it, and a run id means an agent
// reached the call.
//
// The ONE place this question is answered, so moving to another caller-tier marker
// is a one-line change here.
func reservedNameWriter(ctx context.Context) bool {
	return tools.IsSubstrateOperator(ctx) && tools.RunID(ctx) == ""
}

// reservedProvisionKey carries the one reserved name ProvisionReservedDocument is
// creating. Unexported, so nothing outside this package can set it.
type reservedProvisionKey struct{}

// reservedNameRefusal is the refusal for an op that would create, rename or remove
// the reserved name canonical in scope, or nil when the call may.
func reservedNameRefusal(ctx context.Context, op, scope, canonical string) *tools.Result {
	if scope != "tenant" || !memrank.IsReservedTenantPath(canonical) || reservedNameWriter(ctx) {
		return nil
	}
	if p, _ := ctx.Value(reservedProvisionKey{}).(string); p == canonical {
		return nil
	}
	r := errPermission(op+": "+canonical+" is a reserved name in the tenant scope: the runtime finds a "+
		"tenant-wide document by it, so only an operator may create, rename or remove it.",
		"Use another path.")
	return &r
}

// reservedAtOrUnder lists the reserved tenant names at canonical or below it — what
// an rm or mv of canonical would take with it.
func reservedAtOrUnder(canonical string) []string {
	prefix := dirPrefix(canonical)
	var out []string
	for _, r := range memrank.ReservedTenantPaths() {
		if r == canonical || strings.HasPrefix(r, prefix) {
			out = append(out, r)
		}
	}
	return out
}

// reservedTargetRefusal refuses a Document call that would give a reserved tenant
// name to a document, or take one away by deleting the document that holds it.
// copy_documents / move_documents name and unname per document and are checked in
// planCopy. Every other op that writes a dirent only adds a name the runtime chose
// (remember, fact homing) or never removes one.
func (d *Document) reservedTargetRefusal(ctx context.Context, key sqlmem.ScopeKey, in docInput) *tools.Result {
	switch in.Op {
	case "create_document", "import_md", "import_canvas", "set_path":
		if in.Path == "" {
			return nil
		}
		canonical, err := normalizePath(in.Path)
		if err != nil {
			return nil // the op refuses a malformed path itself, with its own advice
		}
		return reservedNameRefusal(ctx, in.Op, key.Scope, canonical)
	case "delete_document":
		return d.reservedHeldRefusal(ctx, key, in)
	}
	return nil
}

// reservedHeldRefusal refuses deleting a document that holds a reserved tenant name:
// the delete drops every name the document has, so it would vacate the name for the
// next document to take. Mirrors Path's rm, which refuses the name itself.
func (d *Document) reservedHeldRefusal(ctx context.Context, key sqlmem.ScopeKey, in docInput) *tools.Result {
	if key.Scope != "tenant" || reservedNameWriter(ctx) {
		return nil
	}
	docID, err := d.docIDFromInput(ctx, key, in)
	if err != nil {
		return nil // the op reports a missing or unknown document itself
	}
	held, err := d.reservedNameHeld(ctx, key, docID)
	if err != nil {
		// FAILS CLOSED: not knowing the document's names must not read as "it has none".
		r := errFrom(in.Op+": could not read the document's names, so it was not deleted: "+err.Error(), err)
		return &r
	}
	if held == "" {
		return nil
	}
	return reservedNameRefusal(ctx, in.Op, key.Scope, held)
}

// reservedNameHeld returns a reserved tenant name docID holds in key's scope, or "".
func (d *Document) reservedNameHeld(ctx context.Context, key sqlmem.ScopeKey, docID string) (string, error) {
	if key.Scope != "tenant" {
		return "", nil
	}
	names, err := d.namesOfDocuments(ctx, key, []string{docID})
	if err != nil {
		return "", err
	}
	return firstReservedTenantPath(names[docID]), nil
}

// firstReservedTenantPath returns the first of names that is a reserved tenant name.
func firstReservedTenantPath(names []string) string {
	for _, n := range names {
		if memrank.IsReservedTenantPath(n) {
			return n
		}
	}
	return ""
}

// TenantIdentityRecorder records which document holds each tenant-wide role before
// an operator renames or removes a reserved name. The Path tool calls it: Path has
// no SQL Memory of its own, and an operator's rename of a tenant that was never
// resolved since the upgrade would otherwise lose track of the ontology.
type TenantIdentityRecorder interface {
	RecordTenantIdentities(ctx context.Context) error
}

// RecordTenantIdentities implements TenantIdentityRecorder for the ctx's tenant.
func (d *Document) RecordTenantIdentities(ctx context.Context) error {
	if d == nil || d.Store == nil || d.SqlMem == nil {
		return nil // no SQL Memory, no documents, nothing to lose track of
	}
	_, err := d.TenantOntologyID(ctx)
	return err
}

// TenantOntologyID is the ctx's tenant's ontology document id, "" when it has none.
// The server's ontology routes address the document by it, so an operator's rename
// leaves them on the same document.
func (d *Document) TenantOntologyID(ctx context.Context) (string, error) {
	key, _, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return "", err
	}
	return d.resolveOntologyID(ctx, key)
}

// ontologyDocumentID is resolveOntologyID for a reader, "" on a fault as well as
// for no ontology: a reader degrades to the base seed rather than failing.
func (d *Document) ontologyDocumentID(ctx context.Context, key sqlmem.ScopeKey) string {
	id, err := d.resolveOntologyID(ctx, key)
	if err != nil {
		return ""
	}
	return id
}

// resolveOntologyID returns the tenant ontology's document id, or "" when the
// tenant has none.
//
// The RECORDED id wins, whatever is named /memory/ontology now. The name is read
// only to migrate a scope with no record yet, and the first such read records it.
//
// Reading must not provision (see ontologyForKey): the name comes from the dirent
// plane, and SQL is touched only when that name exists — so the scope holds documents
// — or the scope already exists. A tenant with no documents is answered without
// creating its database.
func (d *Document) resolveOntologyID(ctx context.Context, key sqlmem.ScopeKey) (string, error) {
	atName, err := d.documentAtName(ctx, key, memrank.OntologyPath)
	if err != nil {
		return "", err
	}
	if atName == "" {
		ok, err := d.docSQL().ScopeExists(ctx, key)
		if err != nil || !ok {
			return "", err
		}
	}
	recorded, err := d.recordedRole(ctx, key, roleOntology)
	if err != nil || recorded != "" {
		return recorded, err
	}
	if atName == "" {
		return "", nil
	}
	// MIGRATION: a scope from before the record. The document at the name is the
	// ontology — only the operator can have put it there since the names were
	// reserved — provided it still exists: a name left behind by a deleted document
	// is not an ontology to record.
	res, err := d.docSQL().Query(ctx, key, d.docSQL().Rebind(`SELECT id FROM documents WHERE id = ? LIMIT 1`), []any{atName})
	if schemaMissing(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if len(res.Rows) == 0 {
		return "", nil
	}
	if err := d.recordRole(ctx, key, roleOntology, atName); err != nil {
		// Still the answer for this call; the next resolve records it.
		slog.WarnContext(ctx, "ontology: could not record the ontology document id", "tenant", key.Tenant, "err", err)
	}
	return atName, nil
}

// documentAtName is the document a name points at in key's scope, "" when the
// name is absent or names something other than a document.
func (d *Document) documentAtName(ctx context.Context, key sqlmem.ScopeKey, canonical string) (string, error) {
	parent, name, _ := splitPath(canonical)
	row, err := d.Store.DirentGet(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), parent, name)
	if err != nil {
		var nf *store.ErrNotFound
		if asNotFound(err, &nf) {
			return "", nil
		}
		return "", err
	}
	return direntDocumentID(row), nil
}

// recordedRole is the document recorded for role, "" when none is recorded or the
// recorded document is gone. Through the Manager rather than d.query, whose
// missing-table retry would provision: a scope from before document_roles answers
// "no record" here.
func (d *Document) recordedRole(ctx context.Context, key sqlmem.ScopeKey, role string) (string, error) {
	res, err := d.docSQL().Query(ctx, key, d.docSQL().Rebind(
		`SELECT r.document_id FROM document_roles r JOIN documents doc ON doc.id = r.document_id
		  WHERE r.role = ? LIMIT 1`), []any{role})
	if schemaMissing(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return "", nil
	}
	return asStr(res.Rows[0][0]), nil
}

// recordRole records docID as role's document. Through d.exec, whose missing-table
// retry provisions document_roles in a scope that predates it — the scope already
// holds the document being recorded, so nothing new is created but the table.
func (d *Document) recordRole(ctx context.Context, key sqlmem.ScopeKey, role, docID string) error {
	return d.exec(ctx, key, `INSERT INTO document_roles (role, document_id, recorded_at) VALUES (?, ?, ?)
		ON CONFLICT (role) DO UPDATE SET document_id = excluded.document_id, recorded_at = excluded.recorded_at`,
		role, docID, time.Now().UnixNano())
}

// ProvisionReservedDocument creates the tenant document a reserved name stands for,
// from markdown: the runtime's own first-reference provisioning of the ontology and
// of the deployment context. Besides the operator it is the only caller that may put
// a document at a reserved name, and only at the one it was asked for. Every other
// gate of an import_md applies, and the ontology's id is recorded as it is created.
func (d *Document) ProvisionReservedDocument(ctx context.Context, path, markdown string) (tools.Result, error) {
	if !memrank.IsReservedTenantPath(path) {
		return errValidation("provision: "+path+" is not a reserved tenant name", ""), nil
	}
	raw, err := json.Marshal(map[string]any{"op": "import_md", "scope": "tenant", "path": path, "markdown": markdown})
	if err != nil {
		return errFrom("provision: "+err.Error(), err), nil
	}
	res, err := d.Execute(context.WithValue(ctx, reservedProvisionKey{}, path), raw)
	if err != nil || res.IsError || path != memrank.OntologyPath {
		return res, err
	}
	if id := resultField(res, "document_id"); id != "" {
		key, _, kerr := d.ontologyTenantKey(ctx)
		if kerr == nil {
			kerr = d.recordRole(ctx, key, roleOntology, id)
		}
		if kerr != nil {
			// The document is at its name, so the next resolve records it.
			slog.WarnContext(ctx, "ontology: could not record the provisioned ontology document id", "err", kerr)
		}
	}
	return res, nil
}
