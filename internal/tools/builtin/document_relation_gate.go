package builtin

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// document_relation_gate.go — the relation vocabulary's seam (RFC FN ↔ RFC FO).
//
// RFC FO's Relations section declares which relation kinds exist, their domain
// and range, sub-kinds, inverses, which kinds a walk follows and which may be
// asserted without review. RFC FN's statement ops consult it through these
// functions and nothing else. Until FO lands they answer "no Relations section is
// declared", which is exactly the behaviour every scope has today: any kind is
// accepted, nothing expands, a walk follows every kind.
//
// FO owns the bodies; whichever of the two lands second keeps FO's.

// relationGate is the vocabulary's verdict on one relation.
type relationGate struct {
	// Refuse is non-nil only for an authored write that violates a declared rule
	// (kind_not_declared | domain_range_violation).
	Refuse *tools.Result
	// Warnings names each violated rule, for a statement that is kept anyway.
	Warnings []string
	// Valid is ontology_valid: true when no Relations section is declared, or no
	// rule is violated.
	Valid bool
}

// gateRelation checks kind between two chunks against the tenant's Relations
// section. authored is a principal's own link (refused on violation); a rule- or
// model-made statement is annotated instead.
func (d *Document) gateRelation(ctx context.Context, kind, fromChunkID, toChunkID string, authored bool) relationGate {
	return relationGate{Valid: true}
}

// expandKindFilter widens each kind to its declared sub-kinds; expandedTo lists
// what was added, nil when nothing was.
func (d *Document) expandKindFilter(ctx context.Context, kinds []string) (expanded, expandedTo []string) {
	return kinds, nil
}

// ontologyVersion is the tenant ontology root's revision, 0 when there is none.
func (d *Document) ontologyVersion(ctx context.Context) int { return 0 }

// autoAssertKind reports whether a rule-made statement of kind may be asserted
// without review (@auto_assert).
func (d *Document) autoAssertKind(ctx context.Context, kind string) bool { return false }

// walkKinds is the default walk set (@walk); nil walks every kind.
func (d *Document) walkKinds(ctx context.Context) []string { return nil }

// inverseKindName is kind's declared inverse, "" when undeclared.
func (d *Document) inverseKindName(ctx context.Context, kind string) string { return "" }

// discriminatorFor is the value of kind's declared discriminating qualifier
// (e.g. mod_type), "" when none is declared: statements that differ in it coexist.
func (d *Document) discriminatorFor(ctx context.Context, kind string, attributes map[string]any) string {
	return ""
}

// gateAttributes checks a statement's attributes against kind's declared
// qualifiers, with gateRelation's verdict shape.
func (d *Document) gateAttributes(ctx context.Context, kind string, attributes map[string]any, authored bool) relationGate {
	return relationGate{Valid: true}
}
