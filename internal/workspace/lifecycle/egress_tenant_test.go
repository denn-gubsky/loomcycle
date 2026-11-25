package lifecycle

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// Tenant routes at use (RFC GW §6.3, P4): a name on a workspace's row that is
// no operator route is the workspace's tenant's EgressRouteDef, resolved at
// each request when the operator turned tenant routes on, and held to the
// same rules as at creation.

// tenantRoute writes a tenant's route straight to the store, as
// EgressRouteDef create would after its checks: the tests here are about
// what use checks again.
func tenantRoute(t *testing.T, f *fixture, tenant, name, rawURL, cred string) {
	t.Helper()
	row := store.EgressRouteDefRow{TenantID: tenant, Name: name, URL: rawURL, Methods: []string{"GET", "HEAD"}}
	if cred != "" {
		row.Credential, row.Auth = cred, "bearer"
	}
	if _, err := f.st.EgressRouteDefCreate(context.Background(), row); err != nil {
		t.Fatal(err)
	}
}

// tenantEgress sets the fixture's manager up with a proxy (and creds), the
// operator's routes, and tenant routes on, accepting the upstream's port:
// httptest cannot listen on 443.
func tenantEgress(t *testing.T, f *fixture, up *countingUpstream, creds egress.Credentials, op map[string]config.EgressRoute, hosts []string) {
	t.Helper()
	f.m.SetEgress(testProxy(t, f.m, creds, egress.Limits{}), OperatorRoutes(op))
	f.m.SetTenantRoutes(f.st, hosts)
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	f.m.tenantTestPort = u.Port()
}

func refusalReason(err error) string {
	var r *egress.Refusal
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

// A workspace reaches its own tenant's route by name, never another
// tenant's: a name only another tenant wrote is refused before any network
// call, and a name both wrote reaches the workspace's tenant's URL.
func TestEgress_ATenantRouteResolvesOnlyInTheWorkspacesOwnTenant(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	up := newCountingUpstream(t)
	tenantEgress(t, f, up, nil, nil, nil)
	tenantRoute(t, f, "acme", "crm", up.URL+"/acme/", "")
	tenantRoute(t, f, "other", "crm", up.URL+"/other/", "")
	tenantRoute(t, f, "other", "only-other", up.URL+"/only-other/", "")
	o := f.run(t, "alice")
	row, err := f.m.Open(context.Background(), o, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"crm", "only-other"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := send(t, f.m, row, "only-other"); refusalStatus(err) != http.StatusForbidden {
		t.Fatalf("another tenant's route: %v, want a 403 refusal", err)
	}
	if n := up.hits.Load(); n != 0 {
		t.Fatalf("the upstream was reached %d times through another tenant's route", n)
	}
	status, body, err := send(t, f.m, row, "crm")
	if err != nil || status != http.StatusOK || body != "upstream:/acme/x" {
		t.Fatalf("the tenant's own route = %d %q, %v; want its own URL", status, body, err)
	}
}

// With tenant routes off (no SetTenantRoutes), a stored tenant route is not
// one the workspace holds.
func TestEgress_WithTenantRoutesOffAStoredTenantRouteIsRefused(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	up := newCountingUpstream(t)
	f.m.SetEgress(testProxy(t, f.m, nil, egress.Limits{}), nil)
	tenantRoute(t, f, "acme", "crm", up.URL+"/acme/", "")
	o := f.run(t, "alice")
	row, err := f.m.Open(context.Background(), o, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"crm"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := send(t, f.m, row, "crm"); refusalStatus(err) != http.StatusForbidden {
		t.Fatalf("%v, want a 403 refusal", err)
	}
	if up.hits.Load() != 0 {
		t.Fatal("the upstream was reached with tenant routes off")
	}
}

// An operator route wins a name a tenant also took (plan O14): an agent
// definition naming the operator's route is never sent to the tenant's URL.
func TestEgress_AnOperatorRouteWinsANameATenantAlsoTook(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	up := newCountingUpstream(t)
	tenantEgress(t, f, up, nil, map[string]config.EgressRoute{"pypi": {URL: up.URL + "/operator/"}}, nil)
	tenantRoute(t, f, "acme", "pypi", up.URL+"/tenant/", "")
	o := f.run(t, "alice")
	row, err := f.m.Open(context.Background(), o, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"pypi"}})
	if err != nil {
		t.Fatal(err)
	}
	status, body, err := send(t, f.m, row, "pypi")
	if err != nil || status != http.StatusOK || body != "upstream:/operator/x" {
		t.Fatalf("= %d %q, %v; want the operator's URL", status, body, err)
	}
}

// tenant_hosts is applied at use too: a route stored while its host was
// allowed is refused once the operator's list no longer names it.
func TestEgress_ATenantRouteOffTheTenantHostsIsRefusedAtUse(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	up := newCountingUpstream(t)
	tenantEgress(t, f, up, nil, nil, []string{"api.crm.example"})
	tenantRoute(t, f, "acme", "crm", up.URL+"/acme/", "")
	o := f.run(t, "alice")
	row, err := f.m.Open(context.Background(), o, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"crm"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = send(t, f.m, row, "crm")
	if refusalStatus(err) != http.StatusForbidden || !strings.Contains(refusalReason(err), "tenant_hosts") {
		t.Fatalf("%v, want a 403 refusal naming tenant_hosts", err)
	}
	if up.hits.Load() != 0 {
		t.Fatal("the upstream was reached through a host off the list")
	}
	f.m.SetTenantRoutes(f.st, []string{"api.crm.example", "127.0.0.1"})
	if status, _, err := send(t, f.m, row, "crm"); err != nil || status != http.StatusOK {
		t.Fatalf("with the host listed = %d, %v", status, err)
	}
}

// A tenant route reaches port 443 only, at use as at creation (plan O18).
func TestEgress_ATenantRouteOnAnotherPortIsRefusedAtUse(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	up := newCountingUpstream(t)
	tenantEgress(t, f, up, nil, nil, nil)
	f.m.tenantTestPort = "" // the rule as it runs outside this package's tests
	tenantRoute(t, f, "acme", "crm", up.URL+"/acme/", "")
	o := f.run(t, "alice")
	row, err := f.m.Open(context.Background(), o, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"crm"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = send(t, f.m, row, "crm")
	if refusalStatus(err) != http.StatusForbidden || !strings.Contains(refusalReason(err), "443") {
		t.Fatalf("%v, want a 403 refusal naming port 443", err)
	}
	if up.hits.Load() != 0 {
		t.Fatal("the upstream was reached on a port other than 443")
	}
}

// Every request through a tenant route is audited, credential or not (RFC
// GW §6.3).
func TestEgress_AnUncredentialedTenantRouteIsAudited(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	log := &auditLog{}
	f.m.SetAudit(log)
	up := newCountingUpstream(t)
	tenantEgress(t, f, up, nil, nil, nil)
	tenantRoute(t, f, "acme", "crm", up.URL+"/acme/", "")
	o := f.run(t, "alice")
	row, err := f.m.Open(context.Background(), o, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"crm"}})
	if err != nil {
		t.Fatal(err)
	}
	if status, _, err := send(t, f.m, row, "crm"); err != nil || status != http.StatusOK {
		t.Fatalf("= %d, %v", status, err)
	}
	if len(log.evs) != 1 || log.evs[0].EgressRoute != "crm" || log.evs[0].TargetTenant != "acme" {
		t.Fatalf("audit events = %+v, want one for crm in acme", log.evs)
	}
}

// A store fault reading the tenant's routes refuses the request: it never
// goes out on a guess.
func TestEgress_ATenantRouteStoreFaultRefusesTheRequest(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	up := newCountingUpstream(t)
	tenantEgress(t, f, up, nil, nil, nil)
	f.m.SetTenantRoutes(faultyRoutes{}, nil)
	row := store.WorkspaceRow{ID: "ws_x", TenantID: "acme", UserID: "alice", OwnerRunID: "run-x", State: store.WorkspaceReady, Egress: []string{"crm"}}
	_, _, err := send(t, f.m, row, "crm")
	if refusalStatus(err) != http.StatusForbidden || !strings.Contains(refusalReason(err), "cannot be read") {
		t.Fatalf("%v, want a 403 refusal saying the routes cannot be read", err)
	}
}

type faultyRoutes struct{}

func (faultyRoutes) EgressRouteDefList(context.Context, string) ([]store.EgressRouteDefRow, error) {
	return nil, errors.New("store down")
}

// A workspace owned by a principal (opened over the MCP server, RFC GW D35)
// sends nothing out while workspace.mcp.egress is empty, even with a route
// on its row.
func TestEgress_APrincipalOwnedWorkspaceSendsNothingOut(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	up := newCountingUpstream(t)
	tenantEgress(t, f, up, nil, map[string]config.EgressRoute{"pypi": {URL: up.URL + "/pypi/"}}, nil)
	row := store.WorkspaceRow{ID: "ws_p", TenantID: "acme", UserID: "alice", OwnerRunID: "principal-key", OwnerKind: store.WorkspaceOwnerPrincipal, State: store.WorkspaceReady, Egress: []string{"pypi"}}
	if _, _, err := send(t, f.m, row, "pypi"); refusalStatus(err) != http.StatusForbidden {
		t.Fatalf("%v, want a 403 refusal", err)
	}
	if up.hits.Load() != 0 {
		t.Fatal("a principal's workspace reached the upstream")
	}
}

// credEngine is a real credential engine over the fixture's store.
func credEngine(t *testing.T, f *fixture) *credential.Engine {
	t.Helper()
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	return credential.NewEngine(f.st, sealer)
}

func putCred(t *testing.T, e *credential.Engine, scope, scopeID string, allowed []string) {
	t.Helper()
	var al *[]string
	if allowed != nil {
		al = &allowed
	}
	if _, err := e.PutInline(context.Background(), credential.Identity{TenantID: "acme", Scope: scope, ScopeID: scopeID, Name: "crm"}, egressToken, nil, al, nil); err != nil {
		t.Fatal(err)
	}
}

// A credentialed tenant route sends the tenant's restricted credential; a
// user- or agent-scope credential of the same name, which outranks it for
// the run and was never checked when the route was created (plan O19), is
// checked at use: unrestricted, or restricted elsewhere, the request is
// refused before any network call and never falls back to the tenant's.
func TestEgress_ATenantRoutesCredentialIsCheckedAtUseInEveryScope(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, e *credential.Engine, up string, agent string)
		reason string // "" = the request goes out with the token
	}{
		{"tenant restricted", func(*testing.T, *credential.Engine, string, string) {}, ""},
		{"user unrestricted", func(t *testing.T, e *credential.Engine, _ string, _ string) {
			putCred(t, e, "user", "alice", nil)
		}, "not restricted"},
		{"user elsewhere", func(t *testing.T, e *credential.Engine, up string, _ string) {
			putCred(t, e, "user", "alice", []string{up + "/elsewhere"})
		}, "does not cover"},
		{"agent unrestricted", func(t *testing.T, e *credential.Engine, _ string, agent string) {
			putCred(t, e, "agent", agent, nil)
		}, "not restricted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
			up := newCountingUpstream(t)
			e := credEngine(t, f)
			tenantEgress(t, f, up, e, nil, nil)
			tenantRoute(t, f, "acme", "crm", up.URL+"/api/", "$cred:crm")
			putCred(t, e, "tenant", "", []string{up.URL + "/api"})
			o := f.run(t, "alice")
			run, err := f.st.GetRun(context.Background(), o.RunID)
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, e, up.URL, run.Agent)
			row, err := f.m.Open(context.Background(), o, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"crm"}})
			if err != nil {
				t.Fatal(err)
			}
			status, _, err := send(t, f.m, row, "crm")
			if tc.reason == "" {
				if err != nil || status != http.StatusOK || len(up.auth) != 1 || up.auth[0] != "Bearer "+egressToken {
					t.Fatalf("= %d, %v; upstream saw %q", status, err, up.auth)
				}
				return
			}
			if refusalStatus(err) != http.StatusForbidden || !strings.Contains(refusalReason(err), tc.reason) {
				t.Fatalf("%v, want a 403 refusal saying %q", err, tc.reason)
			}
			if up.hits.Load() != 0 {
				t.Fatal("the upstream was reached")
			}
			if strings.Contains(err.Error(), egressToken) {
				t.Fatal("the refusal carries the token")
			}
		})
	}
}

// Another tenant's credential of the route's name is never sent: the
// credential resolves in the workspace's tenant only.
func TestEgress_ATenantRouteNeverSendsAnotherTenantsCredential(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	up := newCountingUpstream(t)
	e := credEngine(t, f)
	tenantEgress(t, f, up, e, nil, nil)
	tenantRoute(t, f, "acme", "crm", up.URL+"/api/", "$cred:crm")
	allowed := []string{up.URL + "/api"}
	if _, err := e.PutInline(context.Background(), credential.Identity{TenantID: "other", Scope: "tenant", Name: "crm"}, egressToken, nil, &allowed, nil); err != nil {
		t.Fatal(err)
	}
	o := f.run(t, "alice")
	row, err := f.m.Open(context.Background(), o, OpenRequest{Name: "work", Toolchains: []string{"python"}, Egress: []string{"crm"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = send(t, f.m, row, "crm")
	if refusalStatus(err) != http.StatusForbidden || !strings.Contains(refusalReason(err), "not found") {
		t.Fatalf("%v, want a 403 refusal: the credential is not found for this run", err)
	}
	if up.hits.Load() != 0 {
		t.Fatal("the upstream was reached with another tenant's credential")
	}
}

// A workspace opened over the MCP server never uses a tenant's route, even
// one its list and row name (P4 plan O28: operator routes only).
func TestEgress_APrincipalsWorkspaceNeverUsesATenantRoute(t *testing.T) {
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	up := newCountingUpstream(t)
	tenantEgress(t, f, up, nil, nil, nil)
	tenantRoute(t, f, "acme", "crm", up.URL+"/acme/", "")
	f.m.SetPrincipalEgress([]string{"crm"})
	if _, _, err := send(t, f.m, principalRow("acme", "crm"), "crm"); refusalStatus(err) != http.StatusForbidden {
		t.Fatalf("%v, want a 403 refusal", err)
	}
	if up.hits.Load() != 0 {
		t.Fatal("a principal's workspace reached a tenant route")
	}
}
