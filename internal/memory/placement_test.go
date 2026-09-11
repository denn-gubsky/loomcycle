package memory

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// placementOntology is a small confirmed ontology with the two declarations that matter
// and one subclass, resolved the way a run would see it.
func placementOntology() []OntologyTerm {
	return ResolveInheritance([]OntologyTerm{
		{Name: "service", MemoryScope: "tenant"},
		{Name: "internal-service", Parent: "service"}, // inherits tenant
		{Name: "policy", MemoryScope: "tenant"},
		{Name: "person", MemoryScope: "user"},
		{Name: "location"}, // in force, declares nothing
		{Name: "incident"}, // ditto
	})
}

func base(in PlacementInput) PlacementInput {
	if in.Terms == nil {
		in.Terms = placementOntology()
	}
	if in.CallerScope == "" {
		in.CallerScope = "user"
	}
	if in.UserID == "" {
		in.UserID = "u_alice"
	}
	// The fixture grants both, so each test exercises the guard it is about rather than
	// the grant. TestResolvePlacement_AnUngrantedScopeIsNeverPlaced covers the grant.
	if in.GrantedScopes == nil {
		in.GrantedScopes = []string{"agent", "user", "tenant"}
	}
	if in.GrantedSqlScopes == nil {
		in.GrantedSqlScopes = []string{"agent", "user", "tenant"}
	}
	// And the fixture's subjects are ALREADY ADOPTED, for the same reason: the curator
	// gate would otherwise fire first and every test below would report it instead of
	// the guard it is named for. TestResolvePlacement_AnUnadoptedSubjectIsNotMinted
	// covers the gate, and builds its input directly so it can say false.
	in.SubjectKnownToTenant = true
	return in
}

func TestResolvePlacement_HonoursTheDeclarationIncludingThroughInheritance(t *testing.T) {
	for _, tc := range []struct{ name, typ, subject, want string }{
		{"declared tenant", "service", "checkout-api", "tenant"},
		{"inherited tenant", "internal-service", "billing-worker", "tenant"},
		{"declared tenant, another type", "policy", "release approvals", "tenant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolvePlacement(base(PlacementInput{DeclaredType: tc.typ, Subject: tc.subject}))
			if got.Scope != tc.want {
				t.Errorf("scope = %q, want %q (%s)", got.Scope, tc.want, got.Reason)
			}
			if !got.Moved {
				t.Errorf("want a move away from the caller's scope, got %+v", got)
			}
			if got.Reason == "" {
				t.Error("every decision must carry a reason")
			}
		})
	}
}

// Each of these is a reason to leave the fact alone. Together they are the design: an
// uncertain placement is not a placement.
func TestResolvePlacement_EveryUncertaintyLeavesTheFactWhereItWas(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   PlacementInput
	}{
		{"no type named", PlacementInput{Subject: "checkout-api"}},
		{"type but no subject", PlacementInput{DeclaredType: "service"}},
		{"type not in force", PlacementInput{DeclaredType: "spacecraft", Subject: "voyager"}},
		{"type declares nothing", PlacementInput{DeclaredType: "location", Subject: "Cluj-Napoca"}},
		{"isolated member", PlacementInput{DeclaredType: "service", Subject: "checkout-api", Isolated: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base(tc.in)
			got := ResolvePlacement(in)
			if got.Scope != in.CallerScope {
				t.Errorf("scope = %q, want the caller's %q", got.Scope, in.CallerScope)
			}
			if got.Moved {
				t.Errorf("must not report a move: %+v", got)
			}
			if got.Reason == "" {
				t.Error("a refusal has to say why — it is the first thing an operator asks")
			}
		})
	}
}

// TestResolvePlacement_NeverPlacesTheRunsOwnUserOutsideTheirScope is the safety guard,
// and it must beat the declaration rather than defer to it.
func TestResolvePlacement_NeverPlacesTheRunsOwnUserOutsideTheirScope(t *testing.T) {
	// A live store types the end-user's own entity as `person` AND as `location` — the
	// second is what carries "The user resides in Cluj-Napoca, Romania". An operator
	// declaring location→tenant is being entirely reasonable, and it must not publish a
	// home city.
	terms := ResolveInheritance([]OntologyTerm{
		{Name: "location", MemoryScope: "tenant"},
		{Name: "person", MemoryScope: "tenant"}, // even declared tenant, the user is exempt
	})
	for _, subject := range []string{"user", "the user", "User", "me", "u_alice", " user "} {
		got := ResolvePlacement(base(PlacementInput{
			DeclaredType: "location", Subject: subject, Terms: terms,
		}))
		if got.Moved || got.Scope != "user" {
			t.Errorf("subject %q was placed in %q — a fact about the run's own user must stay put: %s",
				subject, got.Scope, got.Reason)
		}
	}
	// A third party under the same type is placed, or the guard would be a blanket veto.
	got := ResolvePlacement(base(PlacementInput{
		DeclaredType: "location", Subject: "Cluj-Napoca office", Terms: terms,
	}))
	if !got.Moved || got.Scope != "tenant" {
		t.Errorf("a genuine location should be placed in tenant, got %+v", got)
	}
}

// TestResolvePlacement_ADeclaredNameClosesTheNamedSelfReference is the gap that used to be
// structural. A fact recorded about the user under their OWN NAME is the same shape as a
// fact about a colleague — "Ada prefers Go" against "Maria owns the release process" — so
// nothing in the type system could separate them. A name the user declared can.
func TestResolvePlacement_ADeclaredNameClosesTheNamedSelfReference(t *testing.T) {
	terms := ResolveInheritance([]OntologyTerm{{Name: "person", MemoryScope: "tenant"}})

	// Declared: the user's own fact stays in their scope even though person→tenant.
	got := ResolvePlacement(base(PlacementInput{
		DeclaredType: "person", Subject: "Ada Lovelace", Terms: terms,
		SelfNames: []string{"Ada Lovelace", "Ada"},
	}))
	if got.Moved || got.Scope != "user" {
		t.Errorf("a fact about the user under their declared name was placed in %q: %s",
			got.Scope, got.Reason)
	}

	// A colleague under the same type is still placed, or the guard would be a veto on
	// every person fact and the declaration would be pointless.
	got = ResolvePlacement(base(PlacementInput{
		DeclaredType: "person", Subject: "Maria", Terms: terms,
		SelfNames: []string{"Ada Lovelace", "Ada"},
	}))
	if !got.Moved || got.Scope != "tenant" {
		t.Errorf("a colleague should still be placed in tenant, got %+v", got)
	}
}

// TestResolvePlacement_AnUndeclaredNameIsStillIndistinguishable keeps the residue honest.
//
// A user who has not filled in their Identity section is exactly where everybody was
// before: their name means nothing here, and a fact about them under it is placed like a
// fact about anyone else. That is now a LOCATABLE gap — the profile is visibly empty and
// the person it affects can close it — rather than a structural one, but it is not zero
// and the suite should say so out loud.
func TestResolvePlacement_AnUndeclaredNameIsStillIndistinguishable(t *testing.T) {
	terms := ResolveInheritance([]OntologyTerm{{Name: "person", MemoryScope: "tenant"}})
	got := ResolvePlacement(base(PlacementInput{
		DeclaredType: "person", Subject: "Ada Lovelace", Terms: terms, SelfNames: nil,
	}))
	if !got.Moved {
		t.Skip("the guard now catches an undeclared name — update this test and IsSelfSubject's comment")
	}
	t.Log("KNOWN, and narrowed rather than closed: with no Identity section declared, a " +
		"fact about the user under their own name is placed like a fact about a colleague. " +
		"Filling in the user-root Document's Identity section closes it.")
}

// TestResolvePlacement_InconsistentlyTypedSubjectIsRefusedWithSomethingActionable is the
// guard for the defect a real store already has.
func TestResolvePlacement_InconsistentlyTypedSubjectIsRefusedWithSomethingActionable(t *testing.T) {
	got := ResolvePlacement(base(PlacementInput{
		DeclaredType: "service",
		Subject:      "loomboard",
		SubjectTypes: []string{"service", "person"}, // person → user, service → tenant
	}))
	if got.Moved {
		t.Errorf("a subject typed two ways must not be placed: %+v", got)
	}
	if got.Advisory == "" {
		t.Fatal("this is a data problem an operator can fix, so it must produce an advisory")
	}
	for _, want := range []string{"loomboard", "service", "person", "tenant", "user"} {
		if !strings.Contains(got.Advisory, want) {
			t.Errorf("the advisory must name %q so the operator can act on it: %s", want, got.Advisory)
		}
	}
}

// Types that disagree about the NAME but agree about the SCOPE are not a conflict. The
// ontology is imprecise; the placement is not in doubt.
func TestResolvePlacement_TypesAgreeingOnScopeAreNotAConflict(t *testing.T) {
	got := ResolvePlacement(base(PlacementInput{
		DeclaredType: "service",
		Subject:      "checkout-api",
		SubjectTypes: []string{"service", "internal-service", "spacecraft"}, // both tenant; one unknown
	}))
	if !got.Moved || got.Scope != "tenant" {
		t.Errorf("agreeing types should still place: %+v (advisory %q)", got, got.Advisory)
	}
	if got.Advisory != "" {
		t.Errorf("no conflict, so no advisory: %q", got.Advisory)
	}
}

// A declaration that matches where the write was already going is honoured without being
// reported as a move — a caller logging every move would otherwise log every user-scope
// write on a store that declares person→user.
func TestResolvePlacement_DeclaringTheCallersOwnScopeIsNotAMove(t *testing.T) {
	got := ResolvePlacement(base(PlacementInput{
		DeclaredType: "person", Subject: "Maria", CallerScope: "user",
	}))
	if got.Scope != "user" {
		t.Errorf("scope = %q, want user", got.Scope)
	}
	if got.Moved {
		t.Errorf("same scope is not a move: %+v", got)
	}
}

// The scope names in this package must be the store's own, or a decision made here would
// name a partition the writer cannot resolve.
func TestResolvePlacement_ScopeNamesMatchTheStore(t *testing.T) {
	if MemoryScopeUserName != string(store.MemoryScopeUser) {
		t.Errorf("user scope name drifted: %q vs %q", MemoryScopeUserName, store.MemoryScopeUser)
	}
	if MemoryScopeTenantName != string(store.MemoryScopeTenant) {
		t.Errorf("tenant scope name drifted: %q vs %q", MemoryScopeTenantName, store.MemoryScopeTenant)
	}
}

// TestResolvePlacement_AnUngrantedScopeIsNeverPlaced is the enable switch. A declaration
// on its own must change nothing until the operator also grants the writer — otherwise
// editing a taxonomy would start publishing to the shared plane as a side effect.
func TestResolvePlacement_AnUngrantedScopeIsNeverPlaced(t *testing.T) {
	for _, granted := range [][]string{
		nil,               // not supplied at all: grants nothing
		{},                // explicitly empty
		{"agent", "user"}, // the shipped consolidator's grant today
	} {
		// Constructed WITHOUT base(), which fills a nil grant and would defeat the
		// first case — the one that matters most, since a caller that forgets the
		// field must place nothing rather than everything.
		got := ResolvePlacement(PlacementInput{
			DeclaredType: "service", Subject: "checkout-api", GrantedScopes: granted,
			GrantedSqlScopes: []string{"tenant"},
			Terms:            placementOntology(), CallerScope: "user", UserID: "u_alice",
		})
		if got.Moved || got.Scope != "user" {
			t.Errorf("granted %v: placed in %q — an ungranted scope must never be written: %s",
				granted, got.Scope, got.Reason)
		}
		if !strings.Contains(got.Reason, "not granted") {
			t.Errorf("granted %v: the reason should name the missing grant, got %q", granted, got.Reason)
		}
	}
	// And with the grant, the same fact is placed — or the guard would be a blanket veto.
	got := ResolvePlacement(base(PlacementInput{
		DeclaredType: "service", Subject: "checkout-api",
		GrantedScopes: []string{"user", "tenant"}, GrantedSqlScopes: []string{"tenant"},
	}))
	if !got.Moved || got.Scope != "tenant" {
		t.Errorf("with the grant the fact should be placed, got %+v", got)
	}
}

// TestResolvePlacement_TenantNeedsBothGrants is the half-placement guard.
//
// A fact is stored twice, and the chunk mirror is a Document write — which at tenant scope
// needs the tenant grant on BOTH planes. An agent holding only memory_scopes could write
// the row and not the mirror, leaving the two halves in different partitions: the exact
// failure the one-decision design exists to prevent. Declining the move is the right
// answer, not attempting half of it.
func TestResolvePlacement_TenantNeedsBothGrants(t *testing.T) {
	in := PlacementInput{
		DeclaredType: "service", Subject: "checkout-api",
		Terms: placementOntology(), CallerScope: "user", UserID: "u_alice",
		GrantedScopes:    []string{"user", "tenant"}, // k/v yes...
		GrantedSqlScopes: []string{"user"},           // ...SQL no
		// Adopted, so this test reports the grant and not the curator gate.
		SubjectKnownToTenant: true,
	}
	got := ResolvePlacement(in)
	if got.Moved || got.Scope != "user" {
		t.Errorf("placed in %q with only half the grants: %s", got.Scope, got.Reason)
	}
	if !strings.Contains(got.Reason, "sql_scopes") {
		t.Errorf("the reason must name the missing grant so an operator can fix it, got %q", got.Reason)
	}

	// With both, it moves.
	in.GrantedSqlScopes = []string{"user", "tenant"}
	if got := ResolvePlacement(in); !got.Moved || got.Scope != "tenant" {
		t.Errorf("both grants present, so it should move: %+v", got)
	}

	// A USER target needs nothing from sql_scopes — Document leaves user scope ungated,
	// so requiring it there would refuse placements that are perfectly writable.
	userTarget := PlacementInput{
		DeclaredType: "person", Subject: "Maria",
		Terms: placementOntology(), CallerScope: "agent", UserID: "u_alice",
		GrantedScopes: []string{"agent", "user"}, GrantedSqlScopes: nil,
	}
	if got := ResolvePlacement(userTarget); got.Scope != "user" || !got.Moved {
		t.Errorf("a user-scope placement must not need sql_scopes: %+v", got)
	}
}

// TestResolvePlacement_AnUnadoptedSubjectIsNotMinted is the curator gate.
//
// A tenant entity is what every user's facts about that thing attach to, so whoever
// mints one shapes everybody's memory — and the subject's NAME arrives from a model
// reading one user's untrusted transcript. That the consolidator is deterministic
// code does not help: it is trustworthy code carrying an untrustworthy string.
//
// So the split is between ADDING to a subject the tenant already knows, which is
// ordinary work, and MINTING one, which is a curator's act. The unadopted fact is not
// refused and not lost — it stays in the caller's own scope, exactly as every other
// placement guard leaves it.
func TestResolvePlacement_AnUnadoptedSubjectIsNotMinted(t *testing.T) {
	unadopted := PlacementInput{
		DeclaredType: "service", Subject: "checkout-api",
		Terms: placementOntology(), CallerScope: "user", UserID: "u_alice",
		GrantedScopes:    []string{"user", "tenant"},
		GrantedSqlScopes: []string{"user", "tenant"},
		// The whole point: everything else permits the move.
		SubjectKnownToTenant: false,
	}

	got := ResolvePlacement(unadopted)
	if got.Moved || got.Scope != "user" {
		t.Fatalf("a subject the tenant has never seen was minted from one user's transcript: %+v", got)
	}
	// The reason has to name the subject and the remedy, or an operator cannot act on
	// it — a fact that quietly stays home is indistinguishable from one nothing declared.
	if !strings.Contains(got.Reason, "checkout-api") {
		t.Errorf("the reason must name the subject to adopt, got %q", got.Reason)
	}
	if !strings.Contains(got.Reason, "adopts") {
		t.Errorf("the reason must say how to adopt it, got %q", got.Reason)
	}

	// ADOPTED: the same fact places.
	adopted := unadopted
	adopted.SubjectKnownToTenant = true
	if got := ResolvePlacement(adopted); !got.Moved || got.Scope != "tenant" {
		t.Errorf("an adopted subject's fact should place: %+v", got)
	}

	// THE CURATOR PATH: an operator-plane write has no transcript behind it, so it
	// mints. This is what makes adoption possible at all.
	curator := unadopted
	curator.CuratorWrite = true
	if got := ResolvePlacement(curator); !got.Moved || got.Scope != "tenant" {
		t.Errorf("a curator must be able to place a subject the tenant does not know yet: %+v", got)
	}
}

// TestResolvePlacement_TheCuratorGateDoesNotReachTheUserPlane. The gate is about the
// SHARED plane — a type declaring `user` scope routes a fact within the caller's own
// world, where there is no registry to poison and nobody else to affect.
func TestResolvePlacement_TheCuratorGateDoesNotReachTheUserPlane(t *testing.T) {
	got := ResolvePlacement(PlacementInput{
		DeclaredType: "person", Subject: "Maria",
		Terms: placementOntology(), CallerScope: "agent", UserID: "u_alice",
		GrantedScopes:        []string{"agent", "user"},
		GrantedSqlScopes:     []string{"agent", "user"},
		SubjectKnownToTenant: false,
	})
	if !got.Moved || got.Scope != "user" {
		t.Errorf("a user-scope placement was blocked by a gate meant for the tenant plane: %+v", got)
	}
}
