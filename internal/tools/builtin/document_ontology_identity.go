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
	"fmt"
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

// documentRolesMarkerName is the index that marks a document_roles table as the
// runtime's own.
const documentRolesMarkerName = "document_roles_trusted"

// documentRolesMarkerDDL creates the marker, right after the table it vouches for.
//
// WHY AN INDEX ON documents. Before document_roles was a document table, an agent
// holding tenant sql_scopes could create a table by that name and record its own
// document as the ontology; CREATE TABLE IF NOT EXISTS would then keep the plant. The
// marker has to be something no agent could have made on any release: every statement
// naming `documents` is refused to an agent's SQL (memory_sql_doc_guard.go), so an
// index ON documents can only be the runtime's. Partial on a never-true condition (id is
// the primary key), so it is empty and costs no write.
const documentRolesMarkerDDL = `CREATE INDEX IF NOT EXISTS ` + documentRolesMarkerName +
	` ON documents(id) WHERE id IS NULL`

// quarantineUntrustedRoles renames away a document_roles table that carries no marker:
// one the runtime did not create. The fresh table is then created by the DDL, and the
// ontology is re-derived by the name-migration rule — the operator's document at the
// reserved name, not whatever the plant recorded.
//
// WHY THE CATALOG and not an HMAC'd row: what an agent could never have made is an
// index ON documents, so the catalog answers "is this ours" exactly, with no key to
// manage and no second trust root. What the catalog must also answer is who else holds
// the marker's NAME: before the upgrade it was an ordinary name, so a table, view or
// index of the agent's could sit on it — read as the marker if only the name were
// checked, or making the DDL fail and wedge the scope.
func (d *Document) quarantineUntrustedRoles(ctx context.Context, key sqlmem.ScopeKey) error {
	marked, err := d.documentRolesMarked(ctx, key)
	if err != nil || marked {
		return err
	}
	if err := d.clearMarkerName(ctx, key); err != nil {
		return err
	}
	// Whether or not the scope has its document tables yet: a plant can predate them.
	_, err = d.docSQL().Query(ctx, key, `SELECT role FROM document_roles WHERE 1=0`, nil)
	if schemaMissing(err) {
		return nil // no table: the DDL makes the runtime's own
	}
	if err != nil {
		return err
	}
	quarantined := fmt.Sprintf("document_roles_quarantined_%d", time.Now().UnixNano())
	if _, err := d.docSQL().Exec(ctx, key, `ALTER TABLE document_roles RENAME TO `+quarantined, nil, 0); err != nil {
		return fmt.Errorf("quarantine an untrusted document_roles table: %w", err)
	}
	slog.WarnContext(ctx, "ontology: renamed a document_roles table the runtime did not create; the tenant ontology is re-derived from its reserved name",
		"tenant", key.Tenant, "scope", key.Scope, "renamed_to", quarantined)
	return nil
}

// clearMarkerName removes whatever object holds the marker's name, called only when
// the name is NOT the runtime's index on documents: a table is renamed away (it may hold
// someone's data), a view, index or trigger is dropped. An object of a kind this does
// not know fails provisioning rather than being trusted.
func (d *Document) clearMarkerName(ctx context.Context, key sqlmem.ScopeKey) error {
	postgres := d.docSQL().Tier() == "postgres"
	// SQLite object names are case-insensitive, so a squatter differing in case still
	// blocks CREATE INDEX; Postgres folds unquoted names, which the DDL uses.
	stmt := `SELECT type, name FROM sqlite_master WHERE name = ? COLLATE NOCASE`
	if postgres {
		stmt = `SELECT c.relkind::text, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		         WHERE n.nspname = current_schema() AND c.relname = ?`
	}
	res, err := d.docSQL().Query(ctx, key, d.docSQL().Rebind(stmt), []any{documentRolesMarkerName})
	if err != nil {
		return fmt.Errorf("look up the %s name: %w", documentRolesMarkerName, err)
	}
	for _, row := range res.Rows {
		if len(row) < 2 {
			continue
		}
		kind, name := asStr(row[0]), asStr(row[1])
		var drop string
		switch kind {
		case "table", "r", "p":
			drop = fmt.Sprintf(`ALTER TABLE "%s" RENAME TO %s_quarantined_%d`, name, documentRolesMarkerName, time.Now().UnixNano())
		case "view", "v":
			drop = `DROP VIEW "` + name + `"`
		case "m":
			drop = `DROP MATERIALIZED VIEW "` + name + `"`
		case "index", "i":
			drop = `DROP INDEX "` + name + `"`
		case "trigger":
			drop = `DROP TRIGGER "` + name + `"`
		case "S":
			drop = `DROP SEQUENCE "` + name + `"`
		default:
			return fmt.Errorf("an object of kind %q holds the %s name; remove it to provision this scope", kind, documentRolesMarkerName)
		}
		if _, err := d.docSQL().Exec(ctx, key, drop, nil, 0); err != nil {
			return fmt.Errorf("clear the %s name (%s): %w", documentRolesMarkerName, kind, err)
		}
		slog.WarnContext(ctx, "ontology: removed an object squatting on the document_roles marker name",
			"tenant", key.Tenant, "scope", key.Scope, "kind", kind, "name", name)
	}
	return nil
}

// documentRolesMarked reports whether the marker is exactly the runtime's: an index of
// that name ON documents. An object of the same name that is anything else is a
// squatter, which clearMarkerName removes.
func (d *Document) documentRolesMarked(ctx context.Context, key sqlmem.ScopeKey) (bool, error) {
	stmt := `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ? AND tbl_name = 'documents'`
	if d.docSQL().Tier() == "postgres" {
		stmt = `SELECT count(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = ? AND tablename = 'documents'`
	}
	res, err := d.docSQL().Query(ctx, key, d.docSQL().Rebind(stmt), []any{documentRolesMarkerName})
	if err != nil {
		return false, err
	}
	return len(res.Rows) > 0 && len(res.Rows[0]) > 0 && asInt(res.Rows[0][0]) > 0, nil
}

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

// reservedRootRefusal refuses a write to the ROOT of a document that holds a reserved
// tenant name: its title, status, body, tags and type name and describe the runtime's
// document, and are the operator's. Chunks under the root stay writable — the memory
// bundle's consolidator files facts there. Run AFTER applyClassRules, so an upsert is
// judged on the natural key the handler will use.
//
// Census of the dispatch ops that can write an existing chunk or a document as a whole
// (ontologyWriteOp's list; the rest are read-only or exempt, document_ontology_guard.go):
//
//	chunk-addressed, the root by id:  update_chunk delete_chunk move_chunk reorder_chunk
//	                                  supersede_chunk (id or supersedes_id) add_tags
//	                                  remove_tags set_asset
//	the root by natural key:          upsert_chunk
//	the whole document:               import_md (reimport / document_id / path / work_key),
//	                                  sync, set_remote, define_type, classify,
//	                                  add_tags / remove_tags with document_id alone
//	not here, and why:                create_chunk and import_canvas only add new chunks
//	                                  (import_canvas always makes a new document);
//	                                  delete_document and set_path are judged on the names
//	                                  the document holds or takes (reservedTargetRefusal);
//	                                  judge_fact rules on a fact, and a root is not one.
func (d *Document) reservedRootRefusal(ctx context.Context, key sqlmem.ScopeKey, in docInput) *tools.Result {
	if key.Scope != "tenant" || reservedNameWriter(ctx) {
		return nil
	}
	var docs []string
	rootOf := func(chunkID string) error {
		if chunkID = strings.TrimSpace(chunkID); chunkID == "" {
			return nil
		}
		res, err := d.query(ctx, key, `SELECT id FROM documents WHERE root_chunk_id = ? LIMIT 1`, chunkID)
		if err != nil {
			return err
		}
		if len(res.Rows) > 0 && len(res.Rows[0]) > 0 {
			docs = append(docs, asStr(res.Rows[0][0]))
		}
		return nil
	}
	var err error
	switch in.Op {
	case "update_chunk", "delete_chunk", "move_chunk", "reorder_chunk", "set_asset":
		err = rootOf(in.ID)
	case "supersede_chunk":
		if err = rootOf(in.ID); err == nil {
			err = rootOf(in.SupersedesID)
		}
	case "add_tags", "remove_tags":
		if strings.TrimSpace(in.ID) != "" {
			err = rootOf(in.ID)
		} else if doc := strings.TrimSpace(in.DocumentID); doc != "" {
			docs = append(docs, doc)
		}
	case "upsert_chunk":
		if nk := strings.TrimSpace(in.NaturalKey); nk != "" {
			var existing string
			if existing, err = d.chunkIDByNaturalKey(ctx, key, nk); err == nil {
				err = rootOf(existing)
			}
		}
	case "import_md":
		if strings.TrimSpace(in.DocumentID) != "" || strings.TrimSpace(in.ID) != "" {
			if id, derr := d.docIDFromInput(ctx, key, docInput{ID: in.ID, DocumentID: in.DocumentID}); derr == nil {
				docs = append(docs, id)
			}
		}
		if in.Reimport && in.Path != "" {
			if id, derr := d.docIDFromInput(ctx, key, docInput{Path: in.Path}); derr == nil {
				docs = append(docs, id)
			}
		}
		if wk := strings.TrimSpace(in.WorkKey); wk != "" {
			if id, _, _, werr := d.documentByWorkKey(ctx, key, wk); werr == nil && id != "" {
				docs = append(docs, id)
			}
		}
	case "sync", "set_remote", "define_type", "classify":
		if id, derr := d.docIDFromInput(ctx, key, in); derr == nil {
			docs = append(docs, id)
		}
	}
	if err != nil {
		r := errFrom(in.Op+": could not check the target against the reserved documents, so it was not written: "+err.Error(), err)
		return &r
	}
	for _, doc := range docs {
		held, err := d.reservedNameHeld(ctx, key, doc)
		if err != nil {
			r := errFrom(in.Op+": could not read the document's names, so it was not written: "+err.Error(), err)
			return &r
		}
		if held != "" {
			r := errPermission(in.Op+": this writes the document at "+held+", a reserved name in the tenant scope: "+
				"its root's title, status, body, tags and type, and the document as a whole, are the operator's.",
				"Write chunks under the root instead (create_chunk with its id as parent_id).")
			return &r
		}
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

// RecordTenantOntologies records the ontology's document id for every tenant scope
// that holds its ontology at the name and has no record yet: the one-time migration of
// an upgraded deployment (RFC FY Part C, D3). Run at boot so no tenant sits in the
// window before its first resolve; resolveOntologyID's lazy migration stays as the
// fallback for a scope this pass could not reach. Returns how many tenant scopes have
// an ontology.
func (d *Document) RecordTenantOntologies(ctx context.Context) (int, error) {
	if d == nil || d.Store == nil || d.SqlMem == nil {
		return 0, nil
	}
	scopes, err := d.SqlMem.ListScopes(ctx)
	if err != nil {
		return 0, err
	}
	found := 0
	for _, sk := range scopes {
		if sk.Scope != "tenant" {
			continue
		}
		// The dirent plane keys the tenant raw; SQL Memory stores "" as "default", so
		// that one SQL tenant may be either.
		for _, raw := range bodyTenantsFor(sk.Tenant) {
			tctx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: raw})
			id, rerr := d.resolveOntologyID(tctx, sk)
			if rerr != nil {
				// Per scope: one unreadable scope must not stop the pass; its first
				// resolve migrates it instead.
				slog.WarnContext(ctx, "ontology: boot pass could not resolve a tenant's ontology", "tenant", sk.Tenant, "err", rerr)
				break
			}
			if id != "" {
				found++
				break
			}
		}
	}
	return found, nil
}
