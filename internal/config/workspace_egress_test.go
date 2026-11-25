package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/credential"
)

// RFC GW §6.3: the operator's routes load as written, and Resolved fills the
// defaults (GET and HEAD, bearer with a credential, the two limits) without
// touching what was loaded.
func TestWorkspaceEgress_LoadsRoutesAndResolvesTheirDefaults(t *testing.T) {
	cfg, err := loadConversion(t, "workspace:\n  egress:\n    routes:\n"+
		"      pypi: {url: \"https://pypi.org/\"}\n"+
		"      crm: {url: \"https://api.crm.example/v2/\", methods: [GET, POST], credential: \"$cred:crm_token\"}\n"+
		"      ledger: {url: \"https://ledger.example:8443/api/\", credential: \"$cred:ledger\", auth: header, header: X-Api-Key}\n"+
		"    tenant_routes: true\n    tenant_hosts: [api.crm.example, books.example]\n"+
		"    limits: {rate_per_minute: 60}\n")
	if err != nil {
		t.Fatal(err)
	}
	written := cfg.Workspace.Egress
	if got := written.Routes["pypi"]; !reflect.DeepEqual(got, EgressRoute{URL: "https://pypi.org/"}) {
		t.Errorf("pypi as written = %+v", got)
	}
	r := cfg.Workspace.Resolved().Egress
	want := map[string]EgressRoute{
		"pypi":   {URL: "https://pypi.org/", Methods: []string{"GET", "HEAD"}},
		"crm":    {URL: "https://api.crm.example/v2/", Methods: []string{"GET", "POST"}, Credential: "$cred:crm_token", Auth: EgressAuthBearer},
		"ledger": {URL: "https://ledger.example:8443/api/", Methods: []string{"GET", "HEAD"}, Credential: "$cred:ledger", Auth: EgressAuthHeader, Header: "X-Api-Key"},
	}
	if !reflect.DeepEqual(r.Routes, want) {
		t.Errorf("resolved routes = %+v, want %+v", r.Routes, want)
	}
	if !r.TenantRoutes || !reflect.DeepEqual(r.TenantHosts, []string{"api.crm.example", "books.example"}) {
		t.Errorf("tenant ceiling = %v %v", r.TenantRoutes, r.TenantHosts)
	}
	if r.Limits != (WorkspaceEgressLimits{RatePerMinute: 60, MaxBytes: DefaultWorkspaceEgressMaxBytes}) {
		t.Errorf("limits = %+v", r.Limits)
	}
	// Resolving built new values: the loaded block is as written.
	if m := cfg.Workspace.Egress.Routes["pypi"].Methods; m != nil {
		t.Errorf("Resolved wrote into the loaded config: pypi methods = %v", m)
	}
	if cfg.Workspace.Egress.Limits.MaxBytes != 0 {
		t.Errorf("Resolved wrote into the loaded limits: %+v", cfg.Workspace.Egress.Limits)
	}
}

// Absent, there are no routes, tenants may write none, and the limits are
// the documented defaults.
func TestWorkspaceEgress_AbsentHasNoRoutesAndTheDefaultLimits(t *testing.T) {
	cfg, err := loadConversion(t, "")
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Workspace.Resolved().Egress
	if len(r.Routes) != 0 || r.TenantRoutes || len(r.TenantHosts) != 0 {
		t.Errorf("absent egress = %+v", r)
	}
	if r.Limits != (WorkspaceEgressLimits{RatePerMinute: 600, MaxBytes: 4 << 30}) {
		t.Errorf("limits = %+v", r.Limits)
	}
}

// Every rule of §3.1 of the P4 plan refuses at load, naming the key.
func TestWorkspaceEgress_RefusesAnInvalidRoute(t *testing.T) {
	route := func(name, body string) string {
		return "workspace:\n  egress:\n    routes:\n      " + name + ": " + body + "\n"
	}
	for _, c := range []struct{ block, want string }{
		{"workspace:\n  egress:\n    proxy: true\n", `workspace.egress: unknown key "proxy"`},
		{"workspace:\n  egress:\n    limits: {rate: 5}\n", `workspace.egress.limits: unknown key "rate"`},
		{route("pypi", "{url: \"https://pypi.org/\", mirror: true}"), `workspace.egress.routes.pypi: unknown key "mirror"`},
		// The name.
		{route("PyPI", "{url: \"https://pypi.org/\"}"), `workspace.egress.routes: "PyPI" is not a route name`},
		{route("1pypi", "{url: \"https://pypi.org/\"}"), `"1pypi" is not a route name`},
		{route("py_pi", "{url: \"https://pypi.org/\"}"), `"py_pi" is not a route name`},
		{route(strings.Repeat("a", 64), "{url: \"https://pypi.org/\"}"), "is not a route name"},
		// The URL.
		{route("pypi", "{}"), "workspace.egress.routes.pypi.url: required"},
		{route("pypi", "{url: \"http://pypi.org/\"}"), "only https"},
		{route("pypi", "{url: \"https://u:p@pypi.org/\"}"), "user or password"},
		{route("pypi", "{url: \"https://pypi.org/?a=1\"}"), "query or a fragment"},
		{route("pypi", "{url: \"https://pypi.org/#x\"}"), "query or a fragment"},
		{route("pypi", "{url: \"https://pypi.org/a/../b/\"}"), ". or .. path segment"},
		{route("pypi", "{url: \"https://pypi.org/a/./b/\"}"), ". or .. path segment"},
		{route("pypi", "{url: \"https://pypi.org/a%2fb/\"}"), "percent-encoding"},
		{route("pypi", "{url: \"https://pypi.org/a\\\\b/\"}"), "backslash"},
		{route("pypi", "{url: \"https://pypi.org/a\\tb/\"}"), "control character"},
		{route("pypi", "{url: \"https://pypi.org/simple\"}"), "must end with /"},
		{route("pypi", "{url: \"https://pypi.org\"}"), "must end with /"},
		{route("pypi", "{url: \"https://PyPI.org/\"}"), `is not written in its canonical form (want "https://pypi.org/")`},
		{route("pypi", "{url: \"https://pypi.org:443/\"}"), `is not written in its canonical form (want "https://pypi.org/")`},
		{route("pypi", "{url: \"https://pypi.org//simple/\"}"), "canonical form"},
		// Methods.
		{route("pypi", "{url: \"https://pypi.org/\", methods: [GET, CONNECT]}"), `workspace.egress.routes.pypi.methods[1]: "CONNECT" is not one of`},
		{route("pypi", "{url: \"https://pypi.org/\", methods: [get]}"), `"get" is not one of`},
		{route("pypi", "{url: \"https://pypi.org/\", methods: [GET, GET]}"), "methods[1]: \"GET\" is listed twice"},
		{route("pypi", "{url: \"https://pypi.org/\", methods: []}"), "methods: an empty list allows nothing"},
		// The credential and how it is sent.
		{route("crm", "{url: \"https://a.example/\", auth: bearer}"), "workspace.egress.routes.crm.auth: bearer needs a credential"},
		{route("crm", "{url: \"https://a.example/\", credential: \"$cred:x\", auth: digest}"), `auth: "digest" is not one of`},
		{route("crm", "{url: \"https://a.example/\", credential: \"sk-literal\"}"), `credential takes only a stored credential, written "$cred:<name>"`},
		{route("crm", "{url: \"https://a.example/\", credential: \"$cred:x\", auth: header}"), "header: required with auth header"},
		{route("crm", "{url: \"https://a.example/\", credential: \"$cred:x\", header: X-Key}"), "header: set only with auth header"},
		{route("crm", "{url: \"https://a.example/\", credential: \"$cred:x\", auth: header, header: \"X Key\"}"), "is not an HTTP header name"},
		{route("crm", "{url: \"https://a.example/\", credential: \"$cred:x\", auth: header, header: \"X:Key\"}"), "is not an HTTP header name"},
		{route("crm", "{url: \"https://a.example/\", credential: \"$cred:x\", auth: header, header: Host}"), "may not carry a credential"},
		{route("crm", "{url: \"https://a.example/\", credential: \"$cred:x\", auth: header, header: cookie}"), "may not carry a credential"},
		{route("crm", "{url: \"https://a.example/\", credential: \"$cred:x\", auth: header, header: Proxy-Authorization}"), "may not carry a credential"},
		{route("crm", "{url: \"https://a.example/\", credential: \"$cred:x\", auth: header, header: Connection}"), "may not carry a credential"},
		{route("crm", "{url: \"https://a.example/\", credential: \"$cred:x\", auth: header, header: Transfer-Encoding}"), "may not carry a credential"},
		{route("crm", "{url: \"https://a.example/\", credential: \"$cred:x\", auth: header, header: X-Forwarded-For}"), "may not carry a credential"},
		{route("crm", "{url: \"https://a.example/\", credential: \"$cred:x\", auth: header, header: Forwarded}"), "may not carry a credential"},
		{route("crm", "{url: \"https://a.example/\", credential: \"$cred:x\", auth: header, header: Content-Length}"), "may not carry a credential"},
		// The tenant ceiling.
		{"workspace:\n  egress:\n    tenant_hosts: [api.example]\n", "workspace.egress.tenant_hosts: set, but tenant_routes is off"},
		{"workspace:\n  egress:\n    tenant_routes: true\n    tenant_hosts: [\"https://api.example\"]\n", `tenant_hosts[0]: "https://api.example" is not a bare host name`},
		{"workspace:\n  egress:\n    tenant_routes: true\n    tenant_hosts: [\"api.example:443\"]\n", "is not a bare host name"},
		{"workspace:\n  egress:\n    tenant_routes: true\n    tenant_hosts: [\"api.example/v2\"]\n", "is not a bare host name"},
		{"workspace:\n  egress:\n    tenant_routes: true\n    tenant_hosts: [API.example]\n", "is not a bare host name"},
		{"workspace:\n  egress:\n    tenant_routes: true\n    tenant_hosts: [\"\"]\n", "is not a bare host name"},
		{"workspace:\n  egress:\n    tenant_routes: true\n    tenant_hosts: [.example]\n", "is not a bare host name"},
		{"workspace:\n  egress:\n    tenant_routes: true\n    tenant_hosts: [a.example, a.example]\n", "tenant_hosts[1]: \"a.example\" is listed twice"},
		// Limits.
		{"workspace:\n  egress:\n    limits: {rate_per_minute: -1}\n", "workspace.egress.limits: rate_per_minute and max_bytes may not be negative"},
		{"workspace:\n  egress:\n    limits: {max_bytes: -1}\n", "may not be negative"},
	} {
		if _, err := loadConversion(t, c.block); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want one saying %q", c.block, err, c.want)
		}
	}
}

// A refused credential is never quoted: a literal there is a secret.
func TestWorkspaceEgress_RefusalNeverQuotesALiteralCredential(t *testing.T) {
	_, err := loadConversion(t, "workspace:\n  egress:\n    routes:\n      crm: {url: \"https://a.example/\", credential: \"sk-canary-7f3a\"}\n")
	if err == nil || strings.Contains(err.Error(), "sk-canary-7f3a") {
		t.Fatalf("err = %v, want a refusal that does not quote the value", err)
	}
}

// A route URL is in the form a credential's allowed_urls stores, so a
// credential restricted to exactly the route's URL covers it (the check at use
// is the proxy's: credentials live in the store, not in config).
func TestWorkspaceEgress_RouteURLIsCoveredByAnAllowedURLsEntryOfItself(t *testing.T) {
	for _, u := range []string{"https://pypi.org/", "https://api.crm.example/v2/", "https://h.example:8443/a/b/", "https://[2001:db8::5]/x/"} {
		if err := validateEgressRouteURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
			continue
		}
		allowed := []string{strings.TrimSuffix(u, "/")}
		norm, err := credential.NormalizeAllowedURLs(&allowed)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		if !credential.URLAllowed(norm, u) || !credential.URLAllowed(norm, u+"simple/x") {
			t.Errorf("%s: a credential restricted to it does not cover it", u)
		}
	}
}

// workspace.mcp.egress lists the operator routes a workspace opened over the
// MCP server may use (RFC GW D35; P4 plan O28 as ruled 2026-11-25). Absent,
// none: such a workspace has no egress.
func TestWorkspaceMCPEgress_LoadsTheOperatorRoutesItNames(t *testing.T) {
	cfg, err := loadConversion(t, "workspace:\n  egress:\n    routes:\n      pypi: {url: \"https://pypi.org/\"}\n      npm: {url: \"https://registry.npmjs.org/\"}\n"+
		"  mcp:\n    enabled: true\n    egress: [pypi, npm]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Workspace.Resolved().MCP.Egress; !reflect.DeepEqual(got, []string{"pypi", "npm"}) {
		t.Fatalf("mcp.egress = %v", got)
	}
	cfg, err = loadConversion(t, "workspace:\n  mcp:\n    enabled: true\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Workspace.Resolved().MCP.Egress; len(got) != 0 {
		t.Fatalf("absent mcp.egress = %v, want none", got)
	}
}

func TestWorkspaceMCPEgress_RefusesANameThatIsNoOperatorRoute(t *testing.T) {
	routes := "workspace:\n  egress:\n    routes:\n      pypi: {url: \"https://pypi.org/\"}\n  mcp:\n"
	for _, c := range []struct{ block, want string }{
		{routes + "    egress: [PyPI]\n", `workspace.mcp.egress[0]: "PyPI" is not a route name`},
		{routes + "    egress: [pypi, pypi]\n", `workspace.mcp.egress[1]: "pypi" is listed twice`},
		{routes + "    egress: [crm]\n", `workspace.mcp.egress[0]: "crm" is not an operator route (workspace.egress.routes)`},
		{routes + "    egress: {pypi: true}\n", "cannot unmarshal"},
	} {
		if _, err := loadConversion(t, c.block); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want one saying %q", c.block, err, c.want)
		}
	}
}
