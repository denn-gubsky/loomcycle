package auth

import (
	"errors"
	"strings"
	"testing"
)

// RFC GW P13 (D-P13-1): a personal worker token is a workers:attach token
// marked worker_personal and bound to its subject. The attach value names
// the token's (tenant, user), and it serves that user's runs and nobody
// else's: not another user of the tenant, not the tenant as a whole.
func TestWorkerAttachment_PersonalTokenServesItsUserOnly(t *testing.T) {
	p := Principal{TenantID: "acme", Subject: "alice", Scopes: []string{ScopeWorkersAttach},
		Restrictions: Restrictions{WorkerPersonal: true}, TokenDefID: "def-1"}
	att, err := WorkerAttachment(p)
	if err != nil {
		t.Fatalf("a personal worker token: %v", err)
	}
	if !att.Personal || att.TenantID != "acme" || att.UserID != "alice" || att.Shared {
		t.Fatalf("attach = %+v, want personal acme/alice", att)
	}
	if !att.ServesUser("acme", "alice") {
		t.Error("it does not serve its own user")
	}
	for _, c := range [][2]string{{"acme", "bob"}, {"acme", ""}, {"other", "alice"}, {"", "alice"}} {
		if att.ServesUser(c[0], c[1]) {
			t.Errorf("it serves %s/%q", c[0], c[1])
		}
	}
	// Tenant-wide serving, which every tenant worker's caller asks today,
	// never admits a personal worker: until a caller asks by user it is
	// served nothing.
	if att.Serves("acme") {
		t.Error("a personal worker serves the whole tenant")
	}
	// A tenant worker still serves by tenant and is user-blind.
	tw, err := WorkerAttachment(Principal{TenantID: "acme", Subject: "w", Scopes: []string{ScopeWorkersAttach}})
	if err != nil || !tw.ServesUser("acme", "bob") || tw.Personal {
		t.Errorf("a tenant worker = %+v, %v", tw, err)
	}
}

// A personal token with no subject or no tenant names no user to serve, and
// a person's sign-in credential never attaches even with the marker.
func TestWorkerAttachment_PersonalTokenNeedsItsUserAndTenant(t *testing.T) {
	base := Principal{TenantID: "acme", Subject: "alice", Scopes: []string{ScopeWorkersAttach}, Restrictions: Restrictions{WorkerPersonal: true}}
	noSubject, noTenant, gate := base, base, base
	noSubject.Subject = ""
	noTenant.TenantID = ""
	gate.IssuedByGate = "google"
	for name, p := range map[string]Principal{"no subject": noSubject, "no tenant": noTenant, "gate-issued": gate} {
		if _, err := WorkerAttachment(p); !errors.Is(err, ErrWorkerAttachRefused) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
}

// worker_personal fits only a token whose scopes are exactly
// [workers:attach], never beside worker_shared, and is never passed on from
// a minter.
func TestRestrictions_PersonalMarkerOnlyOnAWorkersAttachTokenNeverSharedNeverInherited(t *testing.T) {
	r := Restrictions{WorkerPersonal: true}
	if err := r.CheckScopes([]string{ScopeWorkersAttach}); err != nil {
		t.Errorf("[workers:attach]: %v", err)
	}
	for _, scopes := range [][]string{nil, {ScopeTenant}, {ScopeUser}, {ScopeAdmin}, {ScopeWorkersAttach, ScopeRunsRead}} {
		if err := r.CheckScopes(scopes); err == nil {
			t.Errorf("%v: accepted", scopes)
		}
	}
	if err := (Restrictions{WorkerPersonal: true, WorkerShared: true}).CheckScopes([]string{ScopeWorkersAttach}); err == nil {
		t.Error("personal and shared together: accepted")
	}
	if held, err := (Restrictions{}).Within(Restrictions{WorkerPersonal: true}); err != nil || held.WorkerPersonal {
		t.Errorf("a minter's worker_personal was passed on: %+v, %v", held, err)
	}
	if r.IsZero() {
		t.Error("a record holding worker_personal reads as zero")
	}
	s, err := MarshalRestrictions(r)
	if err != nil || s != `{"worker_personal":true}` {
		t.Fatalf("stored %q, %v", s, err)
	}
	if back, err := ParseRestrictions(s); err != nil || !back.WorkerPersonal {
		t.Errorf("read back %+v, %v", back, err)
	}
	if !strings.Contains(r.Describe(), "worker_personal") {
		t.Errorf("Describe = %q", r.Describe())
	}
}
