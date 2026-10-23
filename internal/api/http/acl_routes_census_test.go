package http

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// aclRouteOps is every HTTP route of the access-control management surface,
// the family and op its gRPC / MCP twin calls it by, and the type-level
// question the shared operation asks itself (aclEnter). For acldef every op
// rides POST /v1/_acldef, so the route lists one representative op.
var aclRouteOps = []struct {
	method, pattern string
	family, op      string
	surface         func() *authz.Surface
}{
	{"GET", "/v1/_groups", "groupdef", "list", aclReadGroups},
	{"POST", "/v1/_groups", "groupdef", "create", aclManageGroups},
	{"GET", "/v1/_groups/{name}", "groupdef", "get", aclReadGroups},
	{"GET", "/v1/_groups/{name}/effective", "groupdef", "effective", aclReadGroups},
	{"DELETE", "/v1/_groups/{name}", "groupdef", "delete", aclManageGroups},
	{"POST", "/v1/_groups/{name}/members", "groupdef", "add_member", aclManageGroups},
	{"DELETE", "/v1/_groups/{name}/members/{kind}/{id}", "groupdef", "remove_member", aclManageGroups},
	{"POST", "/v1/_acldef", "acldef", "create", aclReadLists},
	{"GET", "/v1/_acldef/names", "acldef", "names", aclReadLists},
	{"GET", "/v1/_aclbinding", "aclbinding", "list", aclReadLists},
	{"POST", "/v1/_aclbinding", "aclbinding", "create", aclAuthorLists},
	{"DELETE", "/v1/_aclbinding/{id}", "aclbinding", "delete", aclAuthorLists},
	{"POST", "/v1/_authz/evaluation", "authz", "evaluation", nil},
	{"POST", "/v1/_authz/evaluations", "authz", "evaluations", nil},
	{"POST", "/v1/_authz/explain", "authz", "explain", nil},
	{"POST", "/v1/_authz/simulate", "authz", "simulate", nil},
}

// Every management route the mux registers is in aclRouteOps, and every
// entry is a registered route: a route added later has to say which op its
// twins call it by and which question the operation asks.
func TestACLRouteOps_CoverEveryRegisteredManagementRoute(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, m := range muxPatternRe.FindAllStringSubmatch(string(src), -1) {
		pattern := m[2]
		if isACLManagementPath(pattern) || strings.HasPrefix(pattern, "/v1/_authz/") {
			registered[strings.TrimSpace(m[1])+" "+pattern] = true
		}
	}
	if len(registered) < 10 {
		t.Fatalf("found only %d management routes in server.go; the census regexp no longer matches", len(registered))
	}
	listed := map[string]bool{}
	for _, r := range aclRouteOps {
		listed[r.method+" "+r.pattern] = true
	}
	var missing, stale []string
	for k := range registered {
		if !listed[k] {
			missing = append(missing, k)
		}
	}
	for k := range listed {
		if !registered[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("management routes with no entry in aclRouteOps: %v", missing)
	}
	if len(stale) > 0 {
		t.Errorf("aclRouteOps entries no route registers: %v", stale)
	}
}

// The question each shared operation asks itself is the one its HTTP route
// asks (authz.HTTPSurface), so the gRPC and MCP twins, which reach the
// operation without that route, get the same answer as HTTP.
func TestACLRouteSurfaces_MatchTheRoutes(t *testing.T) {
	placeholder := regexp.MustCompile(`\{[^}]+\}`)
	for _, r := range aclRouteOps {
		path := placeholder.ReplaceAllString(r.pattern, "x")
		route, governed := authz.HTTPSurface(r.method, path)
		if r.surface == nil {
			if governed {
				t.Errorf("%s %s asks %+v at the route, but its operation asks nothing", r.method, r.pattern, route)
			}
			continue
		}
		if !governed || *r.surface() != route {
			t.Errorf("%s %s: the route asks %+v (governed=%v), the operation asks %+v", r.method, r.pattern, route, governed, *r.surface())
		}
	}
}

// The twins' op table and the HTTP routes describe one surface: every op a
// route is listed under is in the table, and every op in the table has a
// route — the acldef ops other than names all ride POST /v1/_acldef, whose
// body carries the op.
func TestACLRouteOps_EveryTwinOpHasARoute(t *testing.T) {
	routed := map[string]bool{}
	for _, r := range aclRouteOps {
		ops := connector.ACLManagementOps(r.family)
		if ops == nil {
			t.Errorf("%s %s names family %q, which the op table does not have", r.method, r.pattern, r.family)
			continue
		}
		if !slices.Contains(ops, r.op) {
			t.Errorf("%s %s is op %s.%s, which the op table does not list", r.method, r.pattern, r.family, r.op)
		}
		routed[r.family+"."+r.op] = true
	}
	for _, family := range connector.ACLManagementFamilies() {
		for _, op := range connector.ACLManagementOps(family) {
			if routed[family+"."+op] || family == connector.ACLFamilyAclDef && op != "names" {
				continue
			}
			t.Errorf("op %s.%s has no HTTP route: a twin may not carry an operation HTTP does not", family, op)
		}
	}
}
