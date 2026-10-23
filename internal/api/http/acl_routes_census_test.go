package http

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/api/memberpath"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// aclRouteOps is every HTTP route of the access-control management surface,
// the family and op its gRPC / MCP twin calls it by, and the type-level
// question the shared operation asks itself (aclEnter) — the family's
// weakest verb, which the route and the RPC ask too; a write then asks its
// own verb (manage / author / retire) inside the operation, which
// TestACLOperations_AskTheRouteQuestionThemselves holds. For acldef every op
// rides POST /v1/_acldef, so the route lists one representative op.
var aclRouteOps = []struct {
	method, pattern string
	family, op      string
	surface         func() *authz.Surface
}{
	{"GET", "/v1/_groups", "groupdef", "list", aclReadGroups},
	{"POST", "/v1/_groups", "groupdef", "create", aclReadGroups},
	{"GET", "/v1/_groups/{name}", "groupdef", "get", aclReadGroups},
	{"GET", "/v1/_groups/{name}/effective", "groupdef", "effective", aclReadGroups},
	{"DELETE", "/v1/_groups/{name}", "groupdef", "delete", aclReadGroups},
	{"POST", "/v1/_groups/{name}/members", "groupdef", "add_member", aclReadGroups},
	{"DELETE", "/v1/_groups/{name}/members/{kind}/{id}", "groupdef", "remove_member", aclReadGroups},
	{"POST", "/v1/_acldef", "acldef", "create", aclReadLists},
	{"GET", "/v1/_acldef/names", "acldef", "names", aclReadLists},
	{"GET", "/v1/_aclbinding", "aclbinding", "list", aclReadLists},
	{"POST", "/v1/_aclbinding", "aclbinding", "create", aclReadLists},
	{"DELETE", "/v1/_aclbinding/{id}", "aclbinding", "delete", aclReadLists},
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
		if memberpath.IsACLManagementPath(pattern) || strings.HasPrefix(pattern, "/v1/_authz/") {
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

// POST /v1/_acldef takes every acldef op the twins take: the op table is
// the one list both read, so an op the table has and the route's body
// dispatch lacks fails here. An unknown op's refusal names the table's ops.
func TestACLDefRoute_DispatchesEveryOpInTheTable(t *testing.T) {
	_, ts, _ := aclMgmtServer(t, authz.ModeCompat)
	rules := `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`
	for _, op := range connector.ACLManagementOps(connector.ACLFamilyAclDef) {
		code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"`+op+`","name":"probe","rules":`+rules+`}`)
		if code >= 500 || strings.Contains(body, "unknown_op") {
			t.Errorf("POST /v1/_acldef op=%s: %d %s", op, code, body)
		}
	}
	code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"no_such_op"}`)
	if code != 400 || !strings.Contains(body, "unknown_op") {
		t.Fatalf("unknown op: %d %s", code, body)
	}
	for _, op := range connector.ACLManagementOps(connector.ACLFamilyAclDef) {
		if !strings.Contains(body, op) {
			t.Errorf("the unknown-op refusal does not name %q: %s", op, body)
		}
	}
	// The loop retired the probe list; bring it back and ask for the names.
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"put","name":"probe","rules":`+rules+`}`); code != 201 {
		t.Fatalf("put: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"names"}`); code != 200 || !strings.Contains(body, `"names":["probe"]`) {
		t.Errorf("op=names: %d %s", code, body)
	}
}
