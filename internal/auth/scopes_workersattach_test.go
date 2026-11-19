package auth

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// workers:attach is in the catalog but implies nothing, no wider scope
// implies it, and only the superuser's scope satisfies it through HasScope —
// which is exactly why the attach decision must not read HasScope (see
// WorkerAttachment).
func TestScopeWorkersAttach_ImpliesNothingAndNoTenantOrUserScopeSatisfiesIt(t *testing.T) {
	if !ValidScope(ScopeWorkersAttach) {
		t.Fatal("workers:attach is not in the catalog")
	}
	for _, want := range []string{ScopeAdmin, ScopeTenant, ScopeUser, ScopeRunsCreate, ScopeRunsRead, ScopeChannelPublish,
		ScopeChannelRead, ScopeLLMGateway, ScopeRunsDelete, ScopeProvidersOperatorKey, ScopeAuthManage, ScopeSCIMProvision, ScopeMetricsRead} {
		if HasScope([]string{ScopeWorkersAttach}, want) {
			t.Errorf("workers:attach satisfies %q", want)
		}
	}
	if HasScope([]string{ScopeTenant}, ScopeWorkersAttach) || HasScope([]string{ScopeUser}, ScopeWorkersAttach) {
		t.Error("a tenant or user scope satisfies workers:attach")
	}
	if _, ok := tenantImplied[ScopeWorkersAttach]; ok {
		t.Error("workers:attach is tenant-implied")
	}
	if _, ok := userImplied[ScopeWorkersAttach]; ok {
		t.Error("workers:attach is user-implied")
	}
	for _, mode := range []string{"tenant", "isolated"} {
		scopes, err := GrantableUserScopes(mode)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(scopes, ScopeWorkersAttach) {
			t.Errorf("access_mode %s grants workers:attach to a person's token", mode)
		}
	}
}

func TestValidateScopeSet_RefusesWorkersAttachWithAnyOtherScope(t *testing.T) {
	if err := ValidateScopeSet([]string{ScopeWorkersAttach}); err != nil {
		t.Fatalf("alone: %v", err)
	}
	for _, other := range []string{ScopeAdmin, ScopeTenant, ScopeUser, ScopeRunsCreate, ScopeRunsRead, ScopeMetricsRead, ScopeAuthManage, ScopeSCIMProvision} {
		if err := ValidateScopeSet([]string{ScopeWorkersAttach, other}); err == nil {
			t.Errorf("workers:attach + %s accepted", other)
		}
	}
	if !HoldsSinglePurposeScope([]string{ScopeWorkersAttach}) {
		t.Error("workers:attach is not a single-purpose scope")
	}
}

// A principal holding workers:attach is a worker attacher whatever else it
// holds (the restrictive reading of a set that slipped past ValidateScopeSet).
// An admin is NOT one: substrate:admin satisfies every scope through HasScope,
// and reading the predicate that way would refuse an admin every surface.
func TestIsWorkerAttacher_HoldsOnlyForTheScopeItselfNeverForAnAdmin(t *testing.T) {
	cases := []struct {
		p    Principal
		want bool
	}{
		{Principal{Scopes: []string{ScopeWorkersAttach}}, true},
		{Principal{Scopes: []string{ScopeAdmin, ScopeWorkersAttach}}, true},
		{Principal{Scopes: []string{ScopeAdmin}}, false},
		{Principal{Scopes: []string{ScopeTenant}}, false},
		{Principal{Scopes: nil}, false},
		{Principal{Legacy: true, Scopes: []string{ScopeAdmin}}, false},
	}
	for _, c := range cases {
		if got := IsWorkerAttacher(c.p); got != c.want {
			t.Errorf("IsWorkerAttacher(%v) = %v, want %v", c.p.Scopes, got, c.want)
		}
	}
}

// The attach decision, every row of it. A worker sits on a machine that runs
// model-chosen commands, so the only credentials it may carry are a
// tenant-bound or an admin-minted shared workers:attach token: never the
// operator's own, never a person's.
func TestWorkerAttachment_ServesItsTenantOrEveryTenantAndRefusesTheRest(t *testing.T) {
	attach := []string{ScopeWorkersAttach}
	cases := []struct {
		name       string
		p          Principal
		wantErr    string // substring of the refusal; "" = admitted
		wantShared bool
		serves     []string // tenants it must serve
		refuses    []string // tenants it must not serve
	}{
		{name: "tenant-bound", p: Principal{TenantID: "acme", Subject: "worker-1", Scopes: attach, TokenDefID: "def_1"},
			serves: []string{"acme"}, refuses: []string{"globex", "", "ACME"}},
		{name: "shared", p: Principal{TenantID: "ops", Subject: "worker-pool", Scopes: attach, TokenDefID: "def_2", Restrictions: Restrictions{WorkerShared: true}},
			wantShared: true, serves: []string{"ops", "acme", "globex", ""}},
		{name: "config-declared tenant worker", p: Principal{TenantID: "acme", Subject: "w", Scopes: attach, TokenDefID: "cfg:w"},
			serves: []string{"acme"}, refuses: []string{"globex"}},

		{name: "admin token", p: Principal{TenantID: "ops", Subject: "root", Scopes: []string{ScopeAdmin}}, wantErr: "admin"},
		{name: "admin token holding the scope too", p: Principal{TenantID: "ops", Subject: "root", Scopes: []string{ScopeAdmin, ScopeWorkersAttach}}, wantErr: "admin"},
		{name: "legacy secret", p: Principal{TenantID: "default", Subject: "default", Scopes: []string{ScopeAdmin}, Legacy: true}, wantErr: "legacy"},
		{name: "legacy secret holding the scope", p: Principal{TenantID: "default", Subject: "default", Scopes: attach, Legacy: true}, wantErr: "legacy"},
		{name: "tenant operator", p: Principal{TenantID: "acme", Subject: "ops", Scopes: []string{ScopeTenant}}, wantErr: ScopeWorkersAttach},
		{name: "member", p: Principal{TenantID: "acme", Subject: "bob", Scopes: []string{ScopeRunsCreate, ScopeRunsRead}}, wantErr: ScopeWorkersAttach},
		{name: "metrics reader", p: Principal{TenantID: "acme", Subject: "prom", Scopes: []string{ScopeMetricsRead}}, wantErr: ScopeWorkersAttach},
		{name: "no scopes", p: Principal{TenantID: "acme", Subject: "x"}, wantErr: ScopeWorkersAttach},
		{name: "scope beside another", p: Principal{TenantID: "acme", Subject: "x", Scopes: []string{ScopeWorkersAttach, ScopeRunsRead}}, wantErr: "only scope"},
		{name: "no tenant", p: Principal{Subject: "x", Scopes: attach}, wantErr: "tenant"},
		{name: "gate-issued (a person's sign-in)", p: Principal{TenantID: "acme", Subject: "alice", Scopes: attach, IssuedByGate: "password"}, wantErr: "user"},
		{name: "app sign-in", p: Principal{TenantID: "acme", Subject: "alice", Scopes: attach, OAuthClient: "app", OAuthFamily: "f"}, wantErr: "user"},
		{name: "inherits its user's authority", p: Principal{TenantID: "acme", Subject: "alice", Scopes: attach, InheritsAuthority: true}, wantErr: "user"},
		{name: "a Web UI session", p: Principal{TenantID: "acme", Subject: "alice", Scopes: attach, Login: &Login{Gate: "pw", AuthTime: time.Unix(1, 0)}}, wantErr: "user"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := WorkerAttachment(c.p)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("admitted as %+v, want refused (%q)", got, c.wantErr)
				}
				if !errors.Is(err, ErrWorkerAttachRefused) {
					t.Errorf("err = %v, want it to wrap ErrWorkerAttachRefused", err)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("err = %q, want it to name %q", err, c.wantErr)
				}
				if got != (WorkerAttach{}) {
					t.Errorf("a refusal returned %+v, want the zero value", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got.Shared != c.wantShared || got.TenantID != c.p.TenantID {
				t.Errorf("got %+v, want shared=%v tenant=%q", got, c.wantShared, c.p.TenantID)
			}
			for _, tenant := range c.serves {
				if !got.Serves(tenant) {
					t.Errorf("does not serve tenant %q", tenant)
				}
			}
			for _, tenant := range c.refuses {
				if got.Serves(tenant) {
					t.Errorf("serves tenant %q", tenant)
				}
			}
		})
	}
}

// The zero value serves nobody, so a caller that ignores the error still
// places nothing on the worker.
func TestWorkerAttach_ZeroValueServesNoTenant(t *testing.T) {
	for _, tenant := range []string{"", "acme"} {
		if (WorkerAttach{}).Serves(tenant) {
			t.Errorf("the zero value serves %q", tenant)
		}
	}
}
