package builtin

// document_ontology_fields.go — typed classes on the write path (RFC FO P1a §3.2).
//
// A chunk or document whose type names a declared class gets two things from it:
//
//   - FIELD WARNINGS. Unknown fields, values of the wrong type and missing @required
//     fields come back as `warnings` on the response. Never a refusal: free-text fields
//     are valid today, and the RFC BZ §10.2 warn-never-rewrite decision applies to them.
//   - AN IDENTITY KEY. A class with @identity fields makes `<class>:<v1>|<v2>` its
//     natural key. It is stored in chunks.identity_key (unique per scope), and a write
//     that would give a second chunk the same key is REFUSED — the one refusal RFC FO
//     allows, because two chunks claiming one identity is a corruption nobody can undo
//     by reading.
//
// WHERE IT RUNS. At the single dispatch point (Execute), before the op and after it,
// for the four top-level write ops — the same placement as the ontology guard and the
// ACL gate, and for the same reason: a check inside each op is one op away from being
// forgotten. Internal callers (import_md, sync) call the op handlers directly and do
// not pass through here, so they write no identity key; re-importing a document as a
// new copy must not fail halfway on its own identities. Keyed reimport is RFC FN P2's.
//
// WHAT TURNS IT ON. Field warnings need a CONFIRMED, SECTIONED ontology — a document
// with no @section directive reads exactly as before, write responses included, so a
// consolidator writing into a flat ontology sees byte-identical tool results. Identity
// needs only a confirmed ontology whose class declares @identity: the directive is the
// opt-in.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// migrateIdentityKey adds chunks.identity_key and its unique index.
//
// NULLABLE, and almost every chunk leaves it NULL; both tiers treat NULLs as distinct
// in a unique index, so ordinary chunks never collide (asserted by a test, as for
// chunk_memory_meta.natural_key). Probe-then-ALTER like its siblings: ensureSchema is on
// every op's hot path, so the fast case is one 0-row SELECT.
func (d *Document) migrateIdentityKey(ctx context.Context, key sqlmem.ScopeKey) error {
	if _, err := d.query(ctx, key, `SELECT identity_key FROM chunks WHERE 1=0`); err != nil {
		if aerr := d.exec(ctx, key, `ALTER TABLE chunks ADD COLUMN identity_key TEXT`); aerr != nil {
			return aerr
		}
	}
	return d.exec(ctx, key, `CREATE UNIQUE INDEX IF NOT EXISTS chunks_identity_key ON chunks(identity_key)`)
}

// migrateOntologyVersion adds chunk_memory_meta.ontology_version — the revision of the
// tenant ontology's root chunk a fact was written under (RFC FO §3.6). Existing rows
// stay NULL: what vocabulary they were written under is unknown and must not be claimed.
func (d *Document) migrateOntologyVersion(ctx context.Context, key sqlmem.ScopeKey) error {
	if _, err := d.query(ctx, key, `SELECT ontology_version FROM chunk_memory_meta WHERE 1=0`); err == nil {
		return nil
	}
	return d.exec(ctx, key, `ALTER TABLE chunk_memory_meta ADD COLUMN ontology_version BIGINT`)
}

// ontologyClassView is one read of the tenant's classes, taken once per write op.
type ontologyClassView struct {
	// active is false when there is no confirmed ontology document (or it could not be
	// read): nothing is checked, which is the fail-open default.
	active bool
	// sectioned is true when the document declares at least one @section; field
	// warnings only apply then.
	sectioned bool
	// hasDocClasses is true when the tenant declares a document class beyond the seed;
	// only then does a document's type have to name one.
	hasDocClasses bool
	byName        map[string]memrank.OntologyTerm // lowercase name → resolved class
}

func (v ontologyClassView) class(typ string) (memrank.OntologyTerm, bool) {
	t, ok := v.byName[strings.ToLower(strings.TrimSpace(typ))]
	return t, ok
}

// classView reads the run's tenant ontology for the write path. Fails open: a store
// fault yields an inactive view and the write proceeds unchecked.
func (d *Document) classView(ctx context.Context) ontologyClassView {
	if d.Store == nil || d.SqlMem == nil {
		return ontologyClassView{}
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || read.DocumentID == "" || !read.Confirmed {
		return ontologyClassView{}
	}
	v := ontologyClassView{active: true, sectioned: len(read.Sections) > 0, byName: map[string]memrank.OntologyTerm{}}
	for _, t := range memrank.EffectiveClasses(read.Terms, true) {
		v.byName[strings.ToLower(t.Name)] = t
		if t.IsDocumentClass && t.Source == "tenant" {
			v.hasDocClasses = true
		}
	}
	return v
}

// tenantOntologyConfirmed is the cheap probe in front of classView: does the run's
// tenant have an ontology document, and is it confirmed? Two point lookups, no chunk
// walk. Fails closed to false (nothing checked) on any fault.
func (d *Document) tenantOntologyConfirmed(ctx context.Context) bool {
	if d.Store == nil || d.SqlMem == nil {
		return false
	}
	key := sqlmem.ScopeKey{Tenant: sqlScopeTenant(ctx), Scope: "tenant", ScopeID: sqlScopeTenant(ctx)}
	docID, err := d.docIDFromInput(ctx, key, docInput{Path: memrank.OntologyPath})
	if err != nil {
		return false
	}
	res, err := d.query(ctx, key, `SELECT coalesce(status, '') FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(asStr(res.Rows[0][0])), memrank.OntologyConfirmedStatus)
}

// classRuleResult is what the pre-write pass decided.
type classRuleResult struct {
	warnings    []string
	identityKey string // "" = this write sets no identity key
	// clearIdentity is set when an update leaves the chunk with no identity (its type
	// changed, or an identity field was emptied) so a stale key cannot keep blocking the
	// thing that now legitimately holds it.
	clearIdentity bool
}

// classRulesOps are the ops the class rules apply to.
var classRulesOps = map[string]bool{
	"create_chunk": true, "update_chunk": true, "upsert_chunk": true, "create_document": true,
}

// applyClassRules is the pre-write half: it computes warnings and the identity key, and
// refuses an identity collision BEFORE anything is written. It may fill in.NaturalKey
// for upsert_chunk when the caller passed none and the class can derive one.
func (d *Document) applyClassRules(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in *docInput, raw json.RawMessage) (classRuleResult, *tools.Result) {
	if !classRulesOps[in.Op] {
		return classRuleResult{}, nil
	}
	// An untyped create needs nothing, whatever the ontology says; check that before
	// any read. Every other case first asks the two-query probe whether a confirmed
	// ontology exists at all — most tenants have a draft one (it is provisioned on first
	// reference), and a draft governs nothing, so they must not pay for a full read.
	if in.Op != "update_chunk" && (strings.TrimSpace(in.Type) == "" || memrank.IsReservedStructuralType(in.Type)) {
		return classRuleResult{}, nil
	}
	if !d.tenantOntologyConfirmed(ctx) {
		return classRuleResult{}, nil
	}
	typ, fields, title, selfID, isRoot, ok := d.effectiveClassInputs(ctx, key, mscope, in, raw)
	if !ok {
		return classRuleResult{}, nil
	}
	view := d.classView(ctx)
	if !view.active {
		return classRuleResult{}, nil
	}
	// An update that ends with no identity clears any key the chunk held.
	res := classRuleResult{clearIdentity: selfID != ""}
	if strings.TrimSpace(typ) == "" || memrank.IsReservedStructuralType(typ) {
		return res, nil
	}
	class, declared := view.class(typ)
	if isRoot && view.sectioned && view.hasDocClasses {
		switch {
		case !declared:
			res.warnings = append(res.warnings, "type "+typ+" names no class this tenant declares; a document's type should be a document class")
		case !class.IsDocumentClass:
			res.warnings = append(res.warnings, "type "+typ+" is a chunk class, not a document class")
		}
	}
	if !declared {
		return res, nil
	}
	if view.sectioned {
		res.warnings = append(res.warnings, memrank.ValidateFields(class, fields, title)...)
	}
	idKey, hasKey := memrank.IdentityKey(class, fields, title)
	if !hasKey {
		return res, nil
	}
	if in.Op == "upsert_chunk" && strings.TrimSpace(in.NaturalKey) == "" {
		// The class derives the natural key, so upsert's idempotency handle is the
		// thing's identity rather than a caller-invented string (RFC FO §3.2).
		in.NaturalKey = idKey
	}
	if refused := d.identityCollision(ctx, key, in, idKey, selfID); refused != nil {
		return classRuleResult{}, refused
	}
	res.identityKey, res.clearIdentity = idKey, false
	return res, nil
}

// effectiveClassInputs is the type, fields and title the chunk will have AFTER this
// write, and the chunk it is (selfID, "" for a new one). update_chunk merges the stored
// row with what the call supplies, the way updateChunk itself does.
func (d *Document) effectiveClassInputs(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in *docInput, raw json.RawMessage) (typ string, fields map[string]any, title, selfID string, isRoot, ok bool) {
	switch in.Op {
	case "create_chunk", "upsert_chunk":
		return in.Type, decodeFields(in.Fields), in.Title, "", false, true
	case "create_document":
		return in.Type, decodeFields(in.Fields), in.Title, "", true, true
	case "update_chunk":
		if in.ID == "" {
			return "", nil, "", "", false, false
		}
		row, found, err := d.getChunkRow(ctx, key, in.ID)
		if err != nil || !found {
			return "", nil, "", "", false, false // the op reports it
		}
		var present map[string]json.RawMessage
		_ = json.Unmarshal(raw, &present)
		typ, title = row.Type, row.Title
		if _, has := present["type"]; has {
			typ = in.Type
		}
		if _, has := present["title"]; has {
			title = in.Title
		}
		if _, has := present["fields"]; has {
			fields = decodeFields(in.Fields)
		} else {
			if cb, rerr := d.readBody(ctx, mscope, key, in.ID); rerr == nil {
				fields = decodeFields(cb.Fields)
			}
		}
		return typ, fields, title, in.ID, row.ParentID == "", true
	}
	return "", nil, "", "", false, false
}

func decodeFields(raw json.RawMessage) map[string]any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// identityCollision refuses a write that would give a second chunk an identity another
// chunk already holds. selfID is the chunk being updated; an upsert is not colliding
// with the chunk it is about to update (the holder's natural key is the upsert's own).
func (d *Document) identityCollision(ctx context.Context, key sqlmem.ScopeKey, in *docInput, idKey, selfID string) *tools.Result {
	res, err := d.query(ctx, key,
		`SELECT c.id, coalesce(m.natural_key, '') FROM chunks c
		   LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id
		  WHERE c.identity_key = ? LIMIT 2`, idKey)
	if err != nil {
		return nil // fail open; the unique index is the backstop
	}
	for _, row := range res.Rows {
		if len(row) < 2 {
			continue
		}
		holder, holderKey := asStr(row[0]), asStr(row[1])
		if holder == selfID {
			continue
		}
		if in.Op == "upsert_chunk" && holderKey != "" && holderKey == strings.TrimSpace(in.NaturalKey) {
			continue
		}
		r := errBusiness("identity_collision: chunk "+holder+" already holds the identity "+idKey+
			" — two chunks cannot be the same thing. Nothing was written.",
			"Update chunk "+holder+" instead, or change the identity fields if this is a different thing.")
		return &r
	}
	return nil
}

// finishClassRules is the post-write half: it records the identity key on the chunk
// the op wrote and attaches the warnings to its response. A successful response only;
// an error response passes through untouched.
func (d *Document) finishClassRules(ctx context.Context, key sqlmem.ScopeKey, in docInput, cr classRuleResult, res tools.Result) tools.Result {
	if res.IsError {
		return res
	}
	if cr.clearIdentity && in.ID != "" {
		// Best-effort: a key left behind only over-refuses, it never lets a duplicate in.
		_ = d.exec(ctx, key, `UPDATE chunks SET identity_key = NULL WHERE id = ? AND identity_key IS NOT NULL`, in.ID)
	}
	if cr.identityKey == "" && len(cr.warnings) == 0 {
		return res
	}
	var out map[string]any
	dec := json.NewDecoder(strings.NewReader(res.Text))
	// UseNumber: responses carry unix-nano timestamps above 2^53, which a float64
	// round trip would silently corrupt.
	dec.UseNumber()
	if dec.Decode(&out) != nil {
		return res
	}
	if cr.identityKey != "" {
		chunkID := firstString(out, "id", "chunk_id", "root_chunk_id")
		if in.Op == "create_document" {
			chunkID = firstString(out, "root_chunk_id")
		}
		if chunkID != "" {
			if err := d.exec(ctx, key, `UPDATE chunks SET identity_key = ? WHERE id = ?`, cr.identityKey, chunkID); err != nil {
				// A concurrent writer claimed the identity between the pre-check and
				// here; the unique index refused this chunk the key. The content is
				// written, so say so rather than pretend the write failed.
				cr.warnings = append(cr.warnings, "identity "+cr.identityKey+
					" was claimed by another chunk while this one was being written, so this chunk carries no identity key")
			} else {
				out["identity_key"] = cr.identityKey
			}
		}
	}
	if len(cr.warnings) > 0 {
		out["warnings"] = cr.warnings
	}
	b, err := json.Marshal(out)
	if err != nil {
		return res
	}
	res.Text = string(b)
	return res
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
