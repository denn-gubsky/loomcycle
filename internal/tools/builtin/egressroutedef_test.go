package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// fakeRouteCreds answers EgressRouteDef's metadata lookups from a map keyed
// "<tenant>/<scope>/<scope_id>/<name>". It has no way to open a secret, which
// is the point: creation reads metadata only.
type fakeRouteCreds map[string]credential.Meta

func (f fakeRouteCreds) Get(_ context.Context, id credential.Identity) (credential.Meta, error) {
	m, ok := f[id.TenantID+"/"+id.Scope+"/"+id.ScopeID+"/"+id.Name]
	if !ok {
		return credential.Meta{}, &store.ErrNotFound{Kind: "credential_def", ID: id.Name}
	}
	return m, nil
}

func restrictedTo(urls ...string) credential.Meta { return credential.Meta{AllowedURLs: &urls} }

func egressRouteFixture(t *testing.T, eg config.WorkspaceEgressConfig, creds fakeRouteCreds) *EgressRouteDef {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := &config.Config{}
	cfg.Workspace.Egress = eg
	return &EgressRouteDef{Store: s, Cfg: cfg, Credentials: creds}
}

// routeOperatorCtx is a tenant operator calling from an operator surface: no
// run on ctx, a substrate:tenant principal.
func routeOperatorCtx(tenant string) context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_http-admin", TenantID: tenant})
	return auth.WithPrincipal(ctx, auth.Principal{TenantID: tenant, Subject: "op-" + tenant, Scopes: []string{auth.ScopeTenant}})
}

func runRoute(t *testing.T, tool *EgressRouteDef, ctx context.Context, input string) (map[string]any, string, bool) {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		return nil, res.Text, true
	}
	return decodeResult(t, res.Text), res.Text, false
}

func wantRouteRefused(t *testing.T, tool *EgressRouteDef, ctx context.Context, input, want string) {
	t.Helper()
	_, text, isErr := runRoute(t, tool, ctx, input)
	if !isErr || !strings.Contains(text, want) {
		t.Fatalf("%s\n got (error=%v): %s\nwant an error containing %q", input, isErr, text, want)
	}
}

var tenantRoutesOn = config.WorkspaceEgressConfig{TenantRoutes: true}

func TestEgressRouteDef_CreateRefusedWhileTenantRoutesAreOff(t *testing.T) {
	tool := egressRouteFixture(t, config.WorkspaceEgressConfig{}, nil)
	wantRouteRefused(t, tool, routeOperatorCtx("acme"),
		`{"op":"create","name":"crm","url":"https://api.crm.example/v2/"}`, "workspace.egress.tenant_routes")
}

func TestEgressRouteDef_CreateRefusesAHostOutsideTenantHosts(t *testing.T) {
	tool := egressRouteFixture(t, config.WorkspaceEgressConfig{TenantRoutes: true, TenantHosts: []string{"api.crm.example"}}, nil)
	ctx := routeOperatorCtx("acme")
	wantRouteRefused(t, tool, ctx, `{"op":"create","name":"other","url":"https://evil.example/"}`, "tenant_hosts")
	// The list is exact: a subdomain of a listed host is another host.
	wantRouteRefused(t, tool, ctx, `{"op":"create","name":"sub","url":"https://x.api.crm.example/"}`, "tenant_hosts")
	if _, text, isErr := runRoute(t, tool, ctx, `{"op":"create","name":"crm","url":"https://api.crm.example/v2/"}`); isErr {
		t.Fatalf("a listed host refused: %s", text)
	}
}

func TestEgressRouteDef_CreateRefusesAnOperatorRoutesName(t *testing.T) {
	tool := egressRouteFixture(t, config.WorkspaceEgressConfig{TenantRoutes: true,
		Routes: map[string]config.EgressRoute{"pypi": {URL: "https://pypi.org/"}}}, nil)
	wantRouteRefused(t, tool, routeOperatorCtx("acme"),
		`{"op":"create","name":"pypi","url":"https://mirror.example/pypi/"}`, "operator's route")
}

// The route's fields pass the same checks as an operator's route in yaml.
func TestEgressRouteDef_CreateAppliesTheOperatorRouteRules(t *testing.T) {
	tool := egressRouteFixture(t, tenantRoutesOn, nil)
	ctx := routeOperatorCtx("acme")
	for _, c := range []struct{ input, want string }{
		{`{"op":"create","name":"crm","url":"http://api.crm.example/"}`, "url:"},
		{`{"op":"create","name":"crm","url":"https://api.crm.example/v2"}`, "must end with /"},
		{`{"op":"create","name":"crm","url":"https://api.crm.example/","methods":["CONNECT"]}`, "methods[0]"},
		{`{"op":"create","name":"crm","url":"https://api.crm.example/","credential":"sk-live-literal"}`, "stored credential"},
		{`{"op":"create","name":"Bad_Name","url":"https://api.crm.example/"}`, "not a route name"},
	} {
		wantRouteRefused(t, tool, ctx, c.input, c.want)
	}
}

func TestEgressRouteDef_CreateRefusesACredentialThatDoesNotCoverTheURL(t *testing.T) {
	creds := fakeRouteCreds{
		"acme/tenant//open":   {AllowedURLs: nil},
		"acme/tenant//other":  restrictedTo("https://api.other.example"),
		"acme/tenant//narrow": restrictedTo("https://api.crm.example/v2/contacts"),
		"acme/user/alice/crm": restrictedTo("https://api.crm.example"),
		"acme/tenant//crm":    restrictedTo("https://api.crm.example"),
	}
	tool := egressRouteFixture(t, tenantRoutesOn, creds)
	ctx := routeOperatorCtx("acme")
	route := func(cred string) string {
		return `{"op":"create","name":"crm","url":"https://api.crm.example/v2/","credential":"$cred:` + cred + `"}`
	}
	wantRouteRefused(t, tool, ctx, route("open"), "not restricted to URLs")
	wantRouteRefused(t, tool, ctx, route("other"), "do not cover")
	// A restriction narrower than the route would let the route reach URLs
	// the credential's owner never allowed.
	wantRouteRefused(t, tool, ctx, route("narrow"), "do not cover")
	wantRouteRefused(t, tool, ctx, route("missing"), "no tenant credential")
	if _, text, isErr := runRoute(t, tool, ctx, route("crm")); isErr {
		t.Fatalf("a covering tenant credential refused: %s", text)
	}
}

func TestEgressRouteDef_CreateDefaultsMethodsAndReportsWrites(t *testing.T) {
	tool := egressRouteFixture(t, tenantRoutesOn, nil)
	ctx := routeOperatorCtx("acme")
	out, text, isErr := runRoute(t, tool, ctx, `{"op":"create","name":"docs","url":"https://docs.example/"}`)
	if isErr {
		t.Fatalf("create: %s", text)
	}
	route := out["route"].(map[string]any)
	if got, _ := json.Marshal(route["methods"]); string(got) != `["GET","HEAD"]` || route["writes"] != false || route["created_by"] != "op-acme" {
		t.Errorf("created route = %v; want methods GET, HEAD, writes false, created_by op-acme", route)
	}
	if _, text, isErr := runRoute(t, tool, ctx, `{"op":"create","name":"crm","url":"https://api.crm.example/","methods":["GET","POST"]}`); isErr {
		t.Fatalf("create crm: %s", text)
	}
	got, _, _ := runRoute(t, tool, ctx, `{"op":"get","name":"crm"}`)
	if got["route"].(map[string]any)["writes"] != true {
		t.Errorf("get crm = %v; want writes true", got)
	}
	list, _, _ := runRoute(t, tool, ctx, `{"op":"list"}`)
	routes := list["routes"].([]any)
	if len(routes) != 2 || routes[0].(map[string]any)["name"] != "crm" || routes[0].(map[string]any)["writes"] != true ||
		routes[1].(map[string]any)["writes"] != false {
		t.Errorf("list = %v; want crm (writes) then docs (no writes)", list)
	}
}

func TestEgressRouteDef_CreateRefusesANameTheTenantHolds(t *testing.T) {
	tool := egressRouteFixture(t, tenantRoutesOn, nil)
	ctx := routeOperatorCtx("acme")
	if _, text, isErr := runRoute(t, tool, ctx, `{"op":"create","name":"crm","url":"https://api.crm.example/"}`); isErr {
		t.Fatalf("create: %s", text)
	}
	wantRouteRefused(t, tool, ctx, `{"op":"create","name":"crm","url":"https://elsewhere.example/"}`, "already")
}

func TestEgressRouteDef_AnotherTenantCannotGetListOrDeleteARoute(t *testing.T) {
	tool := egressRouteFixture(t, tenantRoutesOn, nil)
	acme, globex := routeOperatorCtx("acme"), routeOperatorCtx("globex")
	if _, text, isErr := runRoute(t, tool, acme, `{"op":"create","name":"crm","url":"https://api.crm.example/"}`); isErr {
		t.Fatalf("create: %s", text)
	}
	wantRouteRefused(t, tool, globex, `{"op":"get","name":"crm"}`, "not found")
	if list, _, _ := runRoute(t, tool, globex, `{"op":"list"}`); len(list["routes"].([]any)) != 0 {
		t.Errorf("globex list = %v; want none", list)
	}
	wantRouteRefused(t, tool, globex, `{"op":"delete","name":"crm"}`, "not found")
	if _, text, isErr := runRoute(t, tool, acme, `{"op":"get","name":"crm"}`); isErr {
		t.Fatalf("acme's route gone after globex's delete: %s", text)
	}
	if _, text, isErr := runRoute(t, tool, acme, `{"op":"delete","name":"crm"}`); isErr {
		t.Fatalf("acme delete: %s", text)
	}
	wantRouteRefused(t, tool, acme, `{"op":"get","name":"crm"}`, "not found")
}

// RFC GW §6.3 "Who may not write one": an agent. The tool is never in a run's
// tools (it is not in the boot catalog), and refuses a call with a run on ctx
// as well, so a wiring mistake cannot hand a model its own way out.
func TestEgressRouteDef_AnAgentCannotCreateARoute(t *testing.T) {
	tool := egressRouteFixture(t, tenantRoutesOn, nil)
	inRun := tools.WithRunID(routeOperatorCtx("acme"), "r_agent")
	wantRouteRefused(t, tool, inRun, `{"op":"create","name":"crm","url":"https://api.crm.example/"}`, "not available inside a run")
	wantRouteRefused(t, tool, inRun, `{"op":"list"}`, "not available inside a run")
}

// A member (substrate:user) reaching the tool through a surface that forgot
// its gate is refused: routes are written by the tenant's operators.
func TestEgressRouteDef_AMemberCannotWriteARoute(t *testing.T) {
	tool := egressRouteFixture(t, tenantRoutesOn, nil)
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme"})
	ctx = auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeUser}})
	wantRouteRefused(t, tool, ctx, `{"op":"create","name":"crm","url":"https://api.crm.example/"}`, auth.ScopeTenant)
}
