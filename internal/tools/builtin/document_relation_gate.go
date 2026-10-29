package builtin

// document_relation_gate.go — the ontology's hooks for the statement layer (RFC FO §4,
// seam agreed with RFC FN, recorded in RFC FO §10a).
//
// FO owns this file; FN's link_chunks / propose_statement / get_edges / graph_recall
// call it. The signatures are the contract between the two RFCs and must not change
// without both sides agreeing.
//
// P1a ships ontologyVersion for real and the relation hooks as no-ops: with no
// Relations section — the only state P1a can produce, since it does not read one —
// every hook's answer is "nothing declared", which is exactly what these return. P1b
// replaces the bodies, not the signatures.

import (
	"context"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// relationGate is the ontology's verdict on one relation (edge or statement).
type relationGate struct {
	// Refuse is non-nil only for an AUTHORED relation that violates a declared rule
	// (codes kind_not_declared | domain_range_violation). Extracted relations are never
	// refused, only annotated (RFC FO D8).
	Refuse *tools.Result
	// Warnings names each violated rule, human-readable.
	Warnings []string
	// Valid is ontology_valid: true when no Relations section is declared, or when no
	// rule is violated.
	Valid bool
}

// gateRelation checks a relation of the given kind between two chunks against the
// tenant's Relations section.
func (d *Document) gateRelation(ctx context.Context, kind, fromChunkID, toChunkID string, authored bool) relationGate {
	return relationGate{Valid: true}
}

// expandKindFilter widens a kind filter to the kinds' declared sub-kinds (a filter for
// `changes` also returning `amends`). expandedTo lists only the kinds ADDED, nil when
// nothing was.
func (d *Document) expandKindFilter(ctx context.Context, kinds []string) (expanded, expandedTo []string) {
	return kinds, nil
}

// autoAssertKind reports whether a rule-made, exact-anchor relation of this kind may be
// asserted without review (@auto_assert).
func (d *Document) autoAssertKind(ctx context.Context, kind string) bool {
	return false
}

// walkKinds is the default graph-walk set (@walk). nil means no Relations section is
// declared, and the caller walks every kind, as it does today.
func (d *Document) walkKinds(ctx context.Context) []string {
	return nil
}

// inverseKindName is the declared @inverse of a kind, or "" when none is declared.
func (d *Document) inverseKindName(ctx context.Context, kind string) string {
	return ""
}

// ontologyVersion is the revision of the tenant ontology document's root chunk — the
// ontology_version stamped on every fact and statement at write (RFC FO §3.6), so the
// panel can count what was written under an older vocabulary. 0 means the tenant has no
// ontology document; callers store NULL for it.
//
// Read on the run's server-stamped tenant without a grant check, for the reason
// tenantOntologyForRun gives: the ontology is operator-authored config, and nothing from
// the tenant scope is returned beyond one integer.
//
// FAILS OPEN to 0: a version stamp is provenance, and a write must never fail because
// the ontology could not be read.
//
// Two point lookups and no chunk walk: this runs on every fact write, and the
// consolidator writes facts in bulk.
func (d *Document) ontologyVersion(ctx context.Context) int {
	if d.Store == nil || d.SqlMem == nil {
		return 0
	}
	key := sqlmem.ScopeKey{Tenant: sqlScopeTenant(ctx), Scope: "tenant", ScopeID: sqlScopeTenant(ctx)}
	docID, err := d.docIDFromInput(ctx, key, docInput{Path: memrank.OntologyPath})
	if err != nil {
		return 0
	}
	res, err := d.query(ctx, key,
		`SELECT c.revision FROM documents doc JOIN chunks c ON c.id = doc.root_chunk_id WHERE doc.id = ? LIMIT 1`, docID)
	if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return 0
	}
	return asInt(res.Rows[0][0])
}

// ontologyVersionArg is ontologyVersion as a bind argument: NULL when there is no
// ontology, so "written with no vocabulary" never reads as version 0 of one.
func (d *Document) ontologyVersionArg(ctx context.Context) any {
	if v := d.ontologyVersion(ctx); v > 0 {
		return int64(v)
	}
	return nil
}
