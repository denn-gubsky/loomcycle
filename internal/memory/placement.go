package memory

import "strings"

// Placement decides which memory partition a fact belongs in, from the entity type its
// subject carries and the scope an operator declared on that type.
//
// WHY A SEPARATE UNIT. The decision has to be identical wherever it is made, and there is
// more than one writer: a fact is stored twice — a key/value row that semantic recall
// searches, and a chunk mirror that the graph walks. Two writers reaching the decision
// independently is how they drift, and a fact whose halves land in different partitions is
// worse than one that never moved: recall finds it in one scope and the graph in another.
//
// EVERY UNCERTAINTY RESOLVES TO "LEAVE IT WHERE THE CALLER PUT IT". That asymmetry is the
// whole design. Declining to move a fact costs what the system already costs today — the
// fact stays in one user's scope. Moving one wrongly puts a person's private sentence in
// front of everybody in the tenant, and there is no un-publishing it. So this function
// refuses on: no type, an unknown type, a type nobody declared, a subject that names the
// run's own user, a subject typed inconsistently, and an isolated member. It commits only
// when the operator's declaration is unambiguous.

// PlacementInput is everything the decision may read. Assembled by the caller so this
// stays a pure function — it is the piece under test, and a version that queried a store
// could not be tested without one.
type PlacementInput struct {
	// DeclaredType is the entity type the writer is recording the subject under, e.g.
	// "service". Empty means the writer named no type, which is the common case on a
	// small local model and is not an error.
	DeclaredType string
	// Subject is the thing the fact is about, as the writer spelled it.
	Subject string
	// Terms is the EFFECTIVE ontology — layered, pinned and inheritance-resolved. Pass
	// what a run would be told, not the raw tenant document, or a subclass inheriting
	// its scope from an ancestor will read as undeclared.
	Terms []OntologyTerm
	// CallerScope is where the writer asked to put it, and the answer whenever this
	// declines to move it.
	CallerScope string
	// UserID identifies the end-user the run belongs to, for the self-subject guard.
	UserID string
	// SubjectTypes are the entity types this subject ALREADY carries in the store,
	// including DeclaredType if it is already recorded. One subject spelled under two
	// types is the failure mode that makes routing unsafe, so the caller supplies what
	// it knows and this refuses when they disagree.
	SubjectTypes []string
	// Isolated is the run's server-derived isolation bit. An isolated member has no
	// tenant plane at all.
	Isolated bool
	// SelfNames are the names the user declared for themselves in their user-root
	// Document's Identity section. Empty is normal — most profiles are unedited — and
	// means the self guard falls back to catching literal self-reference only.
	SelfNames []string
	// GrantedScopes is the calling agent's own memory_scopes.
	//
	// THIS IS THE ENABLE SWITCH, and deliberately not a separate flag. Placement writes
	// to a plane every user in the tenant reads, so it must be something an operator
	// turned on rather than a side effect of editing a taxonomy. The grant is that act:
	// an agent without `tenant` in its memory_scopes places nothing there, and a
	// declaration alone changes nothing until the operator also grants the writer.
	//
	// Deciding it HERE rather than letting the write fail at the gate is what makes the
	// fallback graceful. The alternative is a consolidator that resolves `tenant`,
	// attempts the write, and errors on a pass that would otherwise have stored the fact
	// perfectly well in the user's own scope.
	//
	// Empty means "not supplied" and is treated as granting nothing, so a caller that
	// forgets to pass it places nothing rather than everything.
	GrantedScopes []string
	// GrantedSqlScopes is the calling agent's own sql_scopes, and it is required for a
	// TENANT placement even though the fact itself is a key/value row.
	//
	// A fact is stored twice: the row semantic recall searches, and a chunk mirror the
	// graph walks. The mirror is a Document write, and a Document write at tenant scope
	// needs the tenant grant on BOTH planes — chunk structure lives in SQL Memory,
	// chunk bodies in k/v Memory — because "half of a tenant store is structure with no
	// text", as the Document gate puts it.
	//
	// Checking only memory_scopes would let this answer "tenant" for a caller that can
	// write the row but not the mirror, and the result is the exact failure the whole
	// one-decision design exists to prevent: a fact whose halves are in different
	// partitions. Better to decline the move.
	//
	// Only consulted for a tenant target. Document leaves agent and user scope ungated,
	// so a user placement needs nothing here.
	GrantedSqlScopes []string
}

// PlacementDecision is where the fact goes and why.
//
// Reason is populated on every decision, including the refusals, because "why did this
// fact not move" is the question an operator asks first and the one a silent policy cannot
// answer.
type PlacementDecision struct {
	// Scope is the partition to write to. Always set: a refusal returns CallerScope
	// rather than "", so no caller has to remember which is which.
	Scope string
	// Moved reports whether Scope differs from the caller's own.
	Moved bool
	// Reason is a short operator-facing explanation, always set.
	Reason string
	// Advisory is set when the store's own data blocked a declared placement — a subject
	// typed two ways. Distinct from Reason because this one is a data problem an operator
	// can go and fix, not a normal outcome.
	Advisory string
}

// ResolvePlacement is the whole decision.
func ResolvePlacement(in PlacementInput) PlacementDecision {
	stay := func(reason string) PlacementDecision {
		return PlacementDecision{Scope: in.CallerScope, Moved: false, Reason: reason}
	}

	typ := TrimOntologyName(in.DeclaredType)
	if typ == "" {
		return stay("the write names no entity type, so no declaration applies")
	}
	if strings.TrimSpace(in.Subject) == "" {
		// type and subject travel together by the extractor's own contract; a type with
		// no subject is a malformed write and not something to route on.
		return stay("the write names a type but no subject")
	}

	term, ok := findTerm(in.Terms, typ)
	if !ok {
		return stay("no type named " + typ + " is in force, so nothing declares where it belongs")
	}
	target := EffectiveMemoryScope(term)
	if target == "" {
		return stay(typ + " declares no memory scope")
	}

	// THE SELF GUARD, and it is deliberately placed before the declaration is honoured.
	// A fact about the person whose run this is belongs to them whatever their ontology
	// says, because the cost of being wrong here is the one cost this design cannot
	// accept.
	if target != MemoryScopeUserName && IsSelfSubject(in.Subject, in.UserID, in.SelfNames) {
		return stay("the subject is the run's own user, which is never placed outside their scope")
	}

	// A subject spelled under two types is the store telling us it does not know what
	// this thing is. Placing the fact would pick one of the two arbitrarily, and the
	// arbitrary pick is visible to everybody when it lands in the tenant.
	if other, conflict := conflictingScope(in, typ, target); conflict {
		return PlacementDecision{
			Scope: in.CallerScope, Moved: false,
			Reason: "left in " + in.CallerScope + " scope: this subject is typed inconsistently",
			Advisory: "subject " + strings.TrimSpace(in.Subject) + " is recorded as both " + typ +
				" (→ " + target + ") and " + other.typeName + " (→ " + other.scope + "). " +
				"Give it ONE type before facts about it can be placed automatically.",
		}
	}

	if !grants(in.GrantedScopes, target) {
		return stay("this agent is not granted " + target + " scope, so nothing is placed there")
	}
	// The tenant plane needs BOTH grants, because placing a fact there also places its
	// chunk mirror, and that is a Document write. One grant would place half of it.
	if target == MemoryScopeTenantName && !grants(in.GrantedSqlScopes, target) {
		return stay("this agent has memory_scopes: [tenant] but not sql_scopes: [tenant], " +
			"and a tenant fact needs both — its chunk mirror is a Document write")
	}

	if target == MemoryScopeTenantName && in.Isolated {
		// The tenant plane is refused to an isolated member at the tool gate anyway.
		// Deciding it here means the write succeeds in the caller's own scope instead of
		// failing, which is what an agent written once and run by both kinds of user
		// needs.
		return stay("this member is isolated from the tenant plane")
	}

	if target == in.CallerScope {
		return PlacementDecision{Scope: target, Moved: false,
			Reason: typ + " declares " + target + " scope, which is where this was already going"}
	}
	return PlacementDecision{Scope: target, Moved: true,
		Reason: typ + " declares " + target + " scope"}
}

// Scope names this package decides between. Spelled here rather than imported from the
// store package to keep this unit free of it — the two must agree, and a test asserts it.
const (
	MemoryScopeUserName   = "user"
	MemoryScopeTenantName = "tenant"
)

// grants reports whether the caller may write the target scope at all.
func grants(scopes []string, target string) bool {
	for _, s := range scopes {
		if strings.EqualFold(strings.TrimSpace(s), target) {
			return true
		}
	}
	return false
}

type scopedType struct{ typeName, scope string }

// conflictingScope reports whether the subject's OTHER recorded types would place it
// somewhere else.
//
// Only a disagreement about the SCOPE counts. A subject typed both `service` and
// `internal-service` is not a problem when both resolve to the tenant — the ontology is
// imprecise but the placement is not in doubt.
func conflictingScope(in PlacementInput, typ, target string) (scopedType, bool) {
	for _, other := range in.SubjectTypes {
		o := TrimOntologyName(other)
		if o == "" || strings.EqualFold(o, typ) {
			continue
		}
		term, ok := findTerm(in.Terms, o)
		if !ok {
			// An unknown type declares nothing, so it cannot contradict a declaration.
			continue
		}
		got := EffectiveMemoryScope(term)
		if got == "" || got == target {
			continue
		}
		return scopedType{typeName: o, scope: got}, true
	}
	return scopedType{}, false
}

func findTerm(terms []OntologyTerm, name string) (OntologyTerm, bool) {
	for _, t := range terms {
		if strings.EqualFold(t.Name, name) {
			return t, true
		}
	}
	return OntologyTerm{}, false
}

// IsSelfSubject reports whether a subject names the end-user of the run rather than some
// third party.
//
// Three ways it can know, in order of how much they cover:
//
//   - selfNames — what the user wrote in their user-root Document's Identity section.
//     This is the one that closes the case the other two cannot: a fact recorded under
//     the user's own name. "Ada prefers Go" and "Maria owns the release process" are
//     the same shape, and only a declared name separates them.
//   - the literal self-reference the extractor actually emits. A live store holds
//     entities titled "user" carrying facts like "The user resides in Cluj-Napoca,
//     Romania".
//   - the run's own user id, which some writers use as the subject.
//
// ⚠️ STILL NOT COMPLETE, and the residue is now a KNOWN and LOCATABLE gap rather than a
// structural one: a user who has not filled in their Identity section is in exactly the
// position everybody was before — their name means nothing here. That is visible (the
// profile is empty), fixable by the person it affects, and it fails toward refusing to
// place rather than toward placing wrongly.
func IsSelfSubject(subject, userID string, selfNames []string) bool {
	s := strings.ToLower(strings.TrimSpace(subject))
	s = strings.TrimPrefix(s, "the ")
	if s == "" {
		return false
	}
	for _, self := range []string{"user", "me", "i", "myself", "operator", "current user"} {
		if s == self {
			return true
		}
	}
	for _, n := range selfNames {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" && s == n {
			return true
		}
	}
	return userID != "" && strings.EqualFold(s, strings.TrimSpace(userID))
}
