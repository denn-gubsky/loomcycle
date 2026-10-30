package builtin

// document_relation_gate.go — the ontology's hooks for the statement layer (RFC FO §4,
// seam agreed with RFC FN, recorded in RFC FO §10a).
//
// FO owns this file; FN's link_chunks / propose_statement / get_edges / graph_recall
// call it. The signatures are the contract between the two RFCs and change only with
// both sides agreeing (P1b added gateRelation's ScopeKey that way).
//
// THE COMPATIBILITY RULE. Every hook answers "nothing declared" unless the tenant's
// CONFIRMED ontology declares a Relations section: kinds pass, filters do not widen,
// walks cover every kind, no inverse is named. A deployment that never declares one
// sees exactly the behaviour it had before (RFC FO §8 P1b gate).

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// relationGate is the ontology's verdict on one relation (edge or statement).
type relationGate struct {
	// Refuse is non-nil only for an AUTHORED relation that violates a declared rule
	// (codes kind_not_declared | domain_range_violation | characteristic_violation |
	// qualifier_violation). Extracted relations are never refused, only annotated
	// (RFC FO D8).
	Refuse *tools.Result
	// Warnings names each violated rule, human-readable.
	Warnings []string
	// Valid is ontology_valid: true when no Relations section is declared, or when no
	// rule is violated.
	Valid bool
}

// relationView is the relation vocabulary in force for the run's tenant, plus the class
// tree domain/range are checked against. Built from the cached ontology read.
type relationView struct {
	active   bool
	byName   map[string]memrank.OntologyRelation
	parentOf map[string]string
	// classParent maps a lowercase class name to its lowercase parent ("" for a root).
	classParent map[string]string
	// conceptChunks holds the chunk ids of the tenant ontology's scheme concepts — what a
	// `concept` domain or range accepts (RFC FO P1c).
	conceptChunks map[string]bool
}

// relationView reads the tenant's relation vocabulary. Inactive — every hook a no-op —
// without a confirmed ontology that declares a Relations section, or on any fault
// (fail-open: a check that cannot run must not refuse writes).
func (d *Document) relationView(ctx context.Context) relationView {
	if d.Store == nil || d.SqlMem == nil {
		return relationView{}
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || read.DocumentID == "" || !read.Confirmed || !hasSection(read.Sections, memrank.SectionRelations) {
		return relationView{}
	}
	eff := memrank.EffectiveRelations(read.Relations, true, true)
	v := relationView{active: true, byName: make(map[string]memrank.OntologyRelation, len(eff)),
		parentOf: make(map[string]string, len(eff)), classParent: map[string]string{}}
	for _, r := range eff {
		v.byName[r.Name] = r
		v.parentOf[r.Name] = r.Parent
	}
	for _, c := range memrank.EffectiveClasses(read.Terms, true) {
		v.classParent[strings.ToLower(c.Name)] = strings.ToLower(c.Parent)
	}
	v.conceptChunks = map[string]bool{}
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			v.conceptChunks[c.ChunkID] = true
		}
	}
	return v
}

func hasSection(sections []string, kind string) bool {
	for _, s := range sections {
		if s == kind {
			return true
		}
	}
	return false
}

// gateRelation checks a relation of the given kind between two chunks of key's scope
// against the tenant's Relations section: the kind must be declared, its ends must be of
// the declared classes, and it must not break a declared characteristic.
//
// A RESERVED kind passes first, before any read — the runtime writes those edges (the
// memory consolidator files an `about` edge for every fact) and their meaning is fixed.
func (d *Document) gateRelation(ctx context.Context, key sqlmem.ScopeKey, kind, fromChunkID, toChunkID string, authored bool) relationGate {
	ok := relationGate{Valid: true}
	if memrank.IsReservedRelation(kind) {
		return ok
	}
	v := d.relationView(ctx)
	if !v.active {
		return ok
	}
	rel, declared := v.byName[kind]
	if !declared {
		return violation(authored, "kind_not_declared",
			fmt.Sprintf("%q is not a relation kind this tenant declares", kind), undeclaredKindHint(v, kind))
	}
	var problems []relationProblem
	problems = append(problems, d.checkRelationEnds(ctx, key, v, rel, fromChunkID, toChunkID)...)
	problems = append(problems, d.checkCharacteristics(ctx, key, rel, fromChunkID, toChunkID)...)
	if len(problems) == 0 {
		return ok
	}
	return violations(authored, problems)
}

// relationProblem is one violated rule.
type relationProblem struct {
	code, msg, hint string
}

func violation(authored bool, code, msg, hint string) relationGate {
	return violations(authored, []relationProblem{{code: code, msg: msg, hint: hint}})
}

// violations turns problems into a verdict: refused (with the first problem's code) when
// authored, annotated otherwise. Every problem is listed in Warnings either way.
func violations(authored bool, problems []relationProblem) relationGate {
	g := relationGate{Valid: false}
	for _, p := range problems {
		g.Warnings = append(g.Warnings, p.code+": "+p.msg)
	}
	if authored {
		first := problems[0]
		r := errValidation(first.code+": "+first.msg+".", first.hint)
		g.Refuse = &r
	}
	return g
}

// undeclaredKindHint points at the most likely intended kind: the one whose inverse was
// written (link from the other end), or the one an old name was renamed to.
func undeclaredKindHint(v relationView, kind string) string {
	for _, r := range sortedRelations(v) {
		if r.Inverse == kind {
			return fmt.Sprintf("%s is how %s reads from the other end — link from %s's target to its source with kind %s.",
				kind, r.Name, r.Name, r.Name)
		}
		if r.RenamedFrom == kind {
			return fmt.Sprintf("%s was renamed to %s; use %s.", kind, r.Name, r.Name)
		}
	}
	return "Use a declared relation kind (cites, based_on, implements, …), or propose a new one with op=propose_entity section=relations."
}

func sortedRelations(v relationView) []memrank.OntologyRelation {
	out := make([]memrank.OntologyRelation, 0, len(v.byName))
	for _, r := range v.byName {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// checkRelationEnds checks @domain and @range closed-world under the subclass closure:
// the chunk's type must be one of the listed classes or a subclass of one. `entity`
// accepts any chunk carrying entity metadata (a subject); `concept` accepts a concept
// chunk of the tenant ontology's Schemes section — which exists only in the tenant
// scope, so elsewhere a document is classified by its concept's tag instead. An untyped
// chunk, or one whose type is not a declared class, does not satisfy a constraint.
func (d *Document) checkRelationEnds(ctx context.Context, key sqlmem.ScopeKey, v relationView, rel memrank.OntologyRelation, from, to string) []relationProblem {
	var out []relationProblem
	for _, end := range []struct {
		label, chunkID string
		allowed        []string
	}{{"source", from, rel.Domain}, {"target", to, rel.Range}} {
		if len(end.allowed) == 0 {
			continue
		}
		typ, isEntity, ok := d.relationEndType(ctx, key, end.chunkID)
		if !ok {
			continue // the caller has already refused a missing chunk
		}
		if classAllowed(v, end.chunkID, typ, isEntity, end.allowed) {
			continue
		}
		got := "untyped"
		if typ != "" {
			got = "of type " + typ
		}
		out = append(out, relationProblem{
			code: "domain_range_violation",
			msg: fmt.Sprintf("%s needs its %s to be %s, but chunk %s is %s",
				rel.Name, end.label, strings.Join(end.allowed, " or "), end.chunkID, got),
			hint: relationEndHint(end.allowed),
		})
	}
	return out
}

// relationEndHint says how to satisfy an end constraint. A `concept` end cannot be met
// outside the tenant scope, where concepts live, so it points at the tag instead.
func relationEndHint(allowed []string) string {
	for _, a := range allowed {
		if strings.EqualFold(a, "concept") {
			return "Link to a concept of the ontology's Schemes section (tenant scope only); to classify a " +
				"document in another scope, tag it with the concept's tag (<scheme>/<path>) instead."
		}
	}
	return "Link chunks of the declared classes, or type the chunk with one of them first."
}

func (d *Document) relationEndType(ctx context.Context, key sqlmem.ScopeKey, chunkID string) (typ string, isEntity, ok bool) {
	res, err := d.query(ctx, key,
		`SELECT coalesce(c.type, ''), coalesce(m.subject, '') FROM chunks c
		   LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id WHERE c.id = ? LIMIT 1`, chunkID)
	if err != nil || len(res.Rows) == 0 || len(res.Rows[0]) < 2 {
		return "", false, false
	}
	return asStr(res.Rows[0][0]), asStr(res.Rows[0][1]) != "", true
}

// classAllowed reports whether a chunk type sits under one of the allowed classes.
// Bounded walk up the class tree: the tree cannot cycle, but this runs on a write path.
func classAllowed(v relationView, chunkID, typ string, isEntity bool, allowed []string) bool {
	want := map[string]bool{}
	for _, a := range allowed {
		switch strings.ToLower(a) {
		case "concept":
			if v.conceptChunks[chunkID] {
				return true
			}
		case "entity":
			if isEntity {
				return true
			}
		default:
			want[strings.ToLower(a)] = true
		}
	}
	cur := strings.ToLower(strings.TrimSpace(typ))
	for i := 0; cur != "" && i <= memrank.OntologyMaxDepth*2; i++ {
		if want[cur] {
			return true
		}
		parent, declared := v.classParent[cur]
		if !declared {
			return false
		}
		cur = parent
	}
	return false
}

// checkCharacteristics checks the characteristics a single new statement can break.
//
// asymmetric and functional look only at statements a reader is shown — asserted, not
// rejected, refuted, retired or ended — and never at the statement being re-linked: a
// repeated link_chunks is idempotent and must not trip a rule against itself. The
// explicit status test also excludes a status this build does not know, which the
// reader rule would treat as live; a refusal should err toward not firing.
func (d *Document) checkCharacteristics(ctx context.Context, key sqlmem.ScopeKey, rel memrank.OntologyRelation, from, to string) []relationProblem {
	var out []relationProblem
	if rel.Irreflexive && from == to {
		out = append(out, relationProblem{code: "characteristic_violation",
			msg:  rel.Name + " cannot relate a chunk to itself",
			hint: "Link two different chunks."})
	}
	if !rel.Asymmetric && !rel.Functional {
		return out
	}
	now := time.Now().UnixNano()
	vis, visArgs := statementVisibility{asOf: &now}.clause()
	live := "e.status = '" + edgeStatusAsserted + "' AND " + vis
	if rel.Asymmetric && from != to {
		args := append([]any{to, from, rel.Name}, visArgs...)
		if res, err := d.query(ctx, key, `SELECT e.id FROM chunk_edges e
			WHERE e.from_id = ? AND e.to_id = ? AND e.kind = ? AND `+live+` LIMIT 1`, args...); err == nil && len(res.Rows) > 0 {
			out = append(out, relationProblem{code: "characteristic_violation",
				msg:  fmt.Sprintf("%s is asymmetric and %s already %s %s", rel.Name, to, rel.Name, from),
				hint: "Retire or reject the opposite statement first if the direction was wrong."})
		}
	}
	if rel.Functional {
		args := append([]any{from, rel.Name, to}, visArgs...)
		if res, err := d.query(ctx, key, `SELECT e.to_id FROM chunk_edges e
			WHERE e.from_id = ? AND e.kind = ? AND e.to_id <> ? AND `+live+` LIMIT 1`, args...); err == nil && len(res.Rows) > 0 {
			out = append(out, relationProblem{code: "characteristic_violation",
				msg: fmt.Sprintf("%s allows one target per source and %s already has %s %s",
					rel.Name, from, rel.Name, asStr(res.Rows[0][0])),
				hint: "Retire the existing statement first, or correct it with a revision."})
		}
	}
	return out
}

// gateAttributes checks a relation's attributes against the kind's declared @qualifiers:
// a name the kind does not declare, or a value outside a declared enum. A kind that
// declares no qualifiers has no attribute schema, so nothing is checked for it.
func (d *Document) gateAttributes(ctx context.Context, kind string, attributes map[string]any, authored bool) relationGate {
	ok := relationGate{Valid: true}
	if len(attributes) == 0 || memrank.IsReservedRelation(kind) {
		return ok
	}
	v := d.relationView(ctx)
	if !v.active {
		return ok
	}
	rel, declared := v.byName[kind]
	if !declared || len(rel.Qualifiers) == 0 {
		return ok // an undeclared kind is gateRelation's to report
	}
	names := make([]string, 0, len(attributes))
	for name := range attributes {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems []relationProblem
	for _, name := range names {
		if statementLayerAttributes[name] {
			continue // the statement layer's own bookkeeping, not a qualifier
		}
		q, known := rel.QualifierNamed(name)
		if !known {
			problems = append(problems, relationProblem{code: "qualifier_violation",
				msg:  fmt.Sprintf("%s declares no qualifier %q", kind, name),
				hint: "Use the kind's qualifiers: " + qualifierNames(rel) + "."})
			continue
		}
		if len(q.Values) == 0 {
			continue
		}
		val := attributeString(attributes[name])
		if !containsFold(q.Values, val) {
			problems = append(problems, relationProblem{code: "qualifier_violation",
				msg:  fmt.Sprintf("%s=%q is not one of %s", name, val, strings.Join(q.Values, ", ")),
				hint: "Use one of the declared values."})
		}
	}
	if len(problems) == 0 {
		return ok
	}
	return violations(authored, problems)
}

// statementLayerAttributes are the attribute keys the statement layer writes for itself
// (the D8 annotation, a revision proposal's original, a synced statement's original
// method). They share the attributes object with a kind's qualifiers and are never
// checked against them.
var statementLayerAttributes = map[string]bool{"ontology_valid": true, "revision_of": true, "origin_method": true}

func qualifierNames(rel memrank.OntologyRelation) string {
	var out []string
	for _, q := range rel.Qualifiers {
		out = append(out, q.Name)
	}
	return strings.Join(out, ", ")
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// attributeString renders a scalar attribute value; a structure renders as "" (and so
// never matches an enum value or makes a discriminator).
func attributeString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	default:
		return ""
	}
}

// expandKindFilter widens a kind filter to the kinds' declared sub-kinds (a filter for
// `changes` also returning `amends`; `references` returning `cites`). expandedTo lists
// only the kinds ADDED, nil when nothing was.
func (d *Document) expandKindFilter(ctx context.Context, kinds []string) (expanded, expandedTo []string) {
	v := d.relationView(ctx)
	if !v.active || len(kinds) == 0 {
		return kinds, nil
	}
	// Twice the class cap: a tenant's own tree is capped at OntologyMaxDepth below the
	// section, and the seed nests a few levels under the reserved roots.
	return memrank.ExpandDescendants(kinds, v.parentOf, memrank.OntologyMaxDepth*2)
}

// autoAssertKind reports whether a rule-made, exact-anchor relation of this kind may be
// asserted without review (@auto_assert).
func (d *Document) autoAssertKind(ctx context.Context, kind string) bool {
	v := d.relationView(ctx)
	return v.active && v.byName[kind].AutoAssert
}

// walkKinds is the default graph-walk set (@walk). nil means no Relations section is
// declared, and the caller walks every kind, as it does today.
func (d *Document) walkKinds(ctx context.Context) []string {
	v := d.relationView(ctx)
	if !v.active {
		return nil
	}
	out := []string{}
	for _, r := range sortedRelations(v) {
		if r.Walk {
			out = append(out, r.Name)
		}
	}
	return out
}

// inverseKindName is the declared @inverse of a kind — a symmetric kind is its own — or
// "" when none is declared.
func (d *Document) inverseKindName(ctx context.Context, kind string) string {
	v := d.relationView(ctx)
	if !v.active {
		return ""
	}
	r, ok := v.byName[kind]
	if !ok {
		return ""
	}
	if r.Symmetric {
		return r.Name
	}
	return r.Inverse
}

// discriminatorFor is the value of the kind's declared @discriminator qualifier in
// attributes (e.g. mod_type), "" when the kind declares none — so two statements of one
// triple that differ in it (two separate amendments) are two rows, not one.
func (d *Document) discriminatorFor(ctx context.Context, kind string, attributes map[string]any) string {
	if len(attributes) == 0 {
		return ""
	}
	v := d.relationView(ctx)
	if !v.active {
		return ""
	}
	r, ok := v.byName[kind]
	if !ok || r.Discriminator == "" {
		return ""
	}
	return attributeString(attributes[r.Discriminator])
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
