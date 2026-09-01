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

// TestResolvePlacement_SelfGuardCannotCatchTheUsersOwnName documents the hole rather than
// pretending it is closed. A fact about the end-user recorded under their own name is
// indistinguishable from a fact about a colleague, so automatic placement WILL sometimes
// publish a personal fact.
//
// Asserted so the limitation is visible in the suite and cannot be forgotten when someone
// reasons about how safe automatic placement is.
func TestResolvePlacement_SelfGuardCannotCatchTheUsersOwnName(t *testing.T) {
	terms := ResolveInheritance([]OntologyTerm{{Name: "person", MemoryScope: "tenant"}})
	got := ResolvePlacement(base(PlacementInput{
		DeclaredType: "person", Subject: "Denn", Terms: terms, UserID: "u_alice",
	}))
	if !got.Moved {
		t.Skip("the guard has been widened to catch a named self-reference — update this test and the comment on IsSelfSubject")
	}
	if got.Scope != "tenant" {
		t.Fatalf("unexpected scope %q", got.Scope)
	}
	t.Log("KNOWN AND UNCLOSED: a fact about the end-user under their own name is placed " +
		"like a fact about anyone else. This is why an operator should see a placement " +
		"before it goes live.")
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
