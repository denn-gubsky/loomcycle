package http

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EY §11 / RFC EZ §2 — the baseline equivalence test, HTTP half. For
// every registered route and every principal class, the built-in baseline
// list of that class must reproduce what the legacy gates admit, so that
// LOOMCYCLE_ACL=enforce (or compat, which honours a baseline's explicit
// denies) with no lists authored behaves as the scope gates alone did.
//
// Both answers come from the real code: the legacy answer is the real
// authMiddleware on a server with no resolver wired (the pre-EY gate — scope,
// member path, self-service, gateway member path, run plane), and the mode
// answers are the same middleware with the resolver wired in that mode. The
// routes are the mux's own (server.go, the census regexp), plus the webhook
// triage routes Mux mounts through its webhook hook.
//
// Two directions are asserted:
//   - Narrowing (no allowlist): wherever the legacy gate admits a principal,
//     compat and enforce admit it too. A failure here is enforce mode, or
//     compat through a baseline deny, refusing something the deployment
//     serves today.
//   - Widening, at each class's MAXIMAL scope set: wherever the legacy gate
//     refuses even the widest token of the class, the baseline does not
//     allow the route's question. Token scopes stay a ceiling (RFC EY §3.6),
//     so a narrower token of the class is held by its scopes, not by the
//     baseline; the baseline is the class's reach. Two kinds of difference
//     are not failures, and both are logged for review:
//       - a COLLISION: the gate asks the same question of a route this class
//         IS admitted to, so no baseline can tell the two apart (a surface
//         granularity finding for authz.HTTPSurface, not a baseline one);
//       - an entry of baselineWiderThanGateHTTP, each with its reason.
//     The run-plane classes are not checked in this direction: their reach
//     is the token's agent restriction, which RFC EY §3.6 keeps as a ceiling
//     the gate applies (runPlaneRoute); the run-plane baseline carries only
//     denies, and denying the rest of the catalog there would strip the
//     in-run reach of every run such a token starts.

// eqClass is one principal class of the matrix: a token's scopes and, for a
// credential limited to certain agents (RFC EF), its agent allowlist.
type eqClass struct {
	name    string
	scopes  []string
	agents  []string
	legacy  bool // the LOOMCYCLE_AUTH_TOKEN secret, not a minted token
	maximal bool // the widest token of its class: the widening direction is asserted for it
}

// The non-tenant scopes a member or an isolated user can be granted.
var eqMemberScopes = []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeRunsDelete,
	auth.ScopeChannelPublish, auth.ScopeChannelRead, auth.ScopeLLMGateway, auth.ScopeProvidersOperatorKey}

func eqClasses() []eqClass {
	return []eqClass{
		{name: "legacy", legacy: true, maximal: true},
		{name: "admin", scopes: []string{auth.ScopeAdmin}, maximal: true},
		{name: "tenant", scopes: []string{auth.ScopeTenant}, maximal: true},
		{name: "tenant+runplane", scopes: []string{auth.ScopeTenant}, agents: []string{"chat"}},
		{name: "member-full", scopes: eqMemberScopes, maximal: true},
		{name: "member-runs", scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
		{name: "member-read", scopes: []string{auth.ScopeRunsRead}},
		{name: "member-channels", scopes: []string{auth.ScopeChannelPublish, auth.ScopeChannelRead}},
		{name: "member-none", scopes: []string{}},
		{name: "member+runplane", scopes: eqMemberScopes, agents: []string{"chat"}},
		{name: "isolated-full", scopes: append([]string{auth.ScopeUser}, eqMemberScopes...), maximal: true},
		{name: "isolated", scopes: []string{auth.ScopeUser}},
		{name: "isolated+runplane", scopes: append([]string{auth.ScopeUser}, eqMemberScopes...), agents: []string{"chat"}},
	}
}

func (c eqClass) bearer() string { return "eq-" + c.name }

// baselineWiderThanGateHTTP lists the (class, question) pairs where the
// class's baseline allows a route's question though the legacy gate refuses
// even the widest token of the class, and no route the class is admitted to
// asks the same question. Keyed "<class>|<action> <type>:<name>". Each needs
// a reason; keep it near-empty.
var baselineWiderThanGateHTTP = map[string]string{
	// An isolated user's own runs read and write its user and agent
	// keyspaces, documents, volumes and skills: the in-run intersection
	// (tools.NarrowScopesByACL, NarrowVolumesByACL, the Skill list) asks
	// these questions of the same baseline. The off-run operator routes
	// (/v1/_memory/*, /v1/_document, /v1/_path, /v1/_ontology, /v1/_volumes,
	// /v1/_library/skills) ask the same TYPE-level question, and the scope
	// gate keeps an isolated user off them. Finer surfaces (RFC EZ §1.7
	// `browse`) would let the two be told apart.
	"isolated-full|read memory:*":    inRunReach,
	"isolated-full|write memory:*":   inRunReach,
	"isolated-full|read document:*":  inRunReach,
	"isolated-full|write document:*": inRunReach,
	"isolated-full|read volume:*":    inRunReach,
	"isolated-full|read skill:*":     inRunReach,
	// GET /v1/_users (the roster, `read principal:user:*`) admits a member;
	// GET /v1/_users/{subject} is a member carve-out. A selector that admits
	// the roster question admits every user under it, so the carve-out is
	// held by the scope gate.
	"member-full|read principal:user:x": "the roster question cannot be granted without each user's; the member carve-out holds at the scope gate",
}

const inRunReach = "the class's in-run reach asks the same question as an off-run operator route the scope gate refuses it"

// baselineNarrowerThanGateHTTP lists the (class, route) pairs where enforce
// mode is DELIBERATELY narrower than the legacy gate: the legacy reach is a
// reported defect, and the baseline does not reproduce it. Keyed
// "<class>|<METHOD> <pattern>". compat still matches the legacy gate there.
var baselineNarrowerThanGateHTTP = map[string]string{
	// The roster of the whole tenant is not an isolated user's to read; the
	// route admits any authenticated principal and the handler confines to
	// the tenant, not to the caller.
	"isolated|GET /v1/_users":      "an isolated user may list its tenant's roster through the legacy gate (reported); the isolated baseline does not grant read principal:user",
	"isolated-full|GET /v1/_users": "an isolated user may list its tenant's roster through the legacy gate (reported); the isolated baseline does not grant read principal:user",
}

func questionKey(s authz.Surface) string { return s.Action + " " + s.Type + ":" + s.Name }

// eqRoute is one registered route as a request the gate can be asked.
type eqRoute struct{ method, pattern, path string }

var eqWebhookTriageRe = regexp.MustCompile(`reg\.Handle\("([A-Z]+) ([^"]+)"`)

func eqRoutes(t *testing.T) []eqRoute {
	t.Helper()
	var out []eqRoute
	add := func(method, pattern string) {
		path := regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(pattern, "x")
		if p := strings.TrimSuffix(path, "/"); p != "" {
			path = p
		}
		out = append(out, eqRoute{method, pattern, path})
	}
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range muxPatternRe.FindAllStringSubmatch(string(src), -1) {
		method := strings.TrimSpace(m[1])
		if method == "" {
			// A method-less pattern serves every method: ask the read and
			// the write question.
			add("GET", m[2])
			add("POST", m[2])
			continue
		}
		add(method, m[2])
	}
	// The webhook triage routes are mounted by Mux through the webhook hook,
	// behind authMiddleware (the receivers themselves are unauthenticated).
	triage, err := os.ReadFile("../webhook/triage.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range eqWebhookTriageRe.FindAllStringSubmatch(string(triage), -1) {
		add(m[1], m[2])
	}
	if len(out) < 150 {
		t.Fatalf("only %d routes; the enumeration no longer reads server.go", len(out))
	}
	return out
}

// eqServers builds the two servers one configuration needs: the main one
// (the legacy secret and every non-admin token) and the admin one (an
// admin-scoped token only, since minting one retires the legacy secret).
// mode nil = no resolver wired: the legacy gate alone.
func eqServers(t *testing.T, mode *authz.Mode) (main, admin *Server) {
	t.Helper()
	build := func(legacySecret string, classes []eqClass) *Server {
		s, st := tokenAuthServer(t, legacySecret)
		for _, c := range classes {
			if c.legacy {
				continue
			}
			restrictions := ""
			if len(c.agents) > 0 {
				raw, err := json.Marshal(auth.Restrictions{Agents: c.agents})
				if err != nil {
					t.Fatal(err)
				}
				restrictions = string(raw)
			}
			if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
				DefID: "def_" + c.name, Name: c.name, TenantID: "acme", Subject: c.name,
				TokenHash: auth.HashToken(testTokenPepper, c.bearer()), AllowedScopes: c.scopes,
				RestrictionsJSON: restrictions,
			}); err != nil {
				t.Fatal(err)
			}
		}
		if mode != nil {
			s.SetACLResolver(aclres.New(st, *mode, 0))
		}
		return s
	}
	var nonAdmin, adminOnly []eqClass
	for _, c := range eqClasses() {
		if c.name == "admin" {
			adminOnly = append(adminOnly, c)
		} else {
			nonAdmin = append(nonAdmin, c)
		}
	}
	return build("eq-legacy", nonAdmin), build("", adminOnly)
}

func eqServerFor(c eqClass, main, admin *Server) *Server {
	if c.name == "admin" {
		return admin
	}
	return main
}

// eqEngineAllows is the engine's own answer for the class at the route's
// question (enforce semantics: an allow must match), from the principal's
// set as the real resolver compiled it.
func eqEngineAllows(t *testing.T, s *Server, c eqClass, surface authz.Surface) (bool, authz.Decision) {
	t.Helper()
	p, ok := s.resolvePrincipal(context.Background(), c.bearer())
	if !ok {
		t.Fatalf("class %s: the bearer did not resolve", c.name)
	}
	if p.Effective == nil {
		t.Fatalf("class %s: no compiled set", c.name)
	}
	ctx := authz.WithMode(context.Background(), authz.ModeEnforce)
	res := authz.EvaluateSurface(ctx, p.Effective, surface, false)
	return res.Allowed, res.Decision
}

func TestACLBaseline_HTTPRoutesMatchTheLegacyGatePerClass(t *testing.T) {
	routes := eqRoutes(t)
	classes := eqClasses()
	legacyMain, legacyAdmin := eqServers(t, nil)
	compat, enforce := authz.ModeCompat, authz.ModeEnforce
	compatMain, compatAdmin := eqServers(t, &compat)
	enforceMain, enforceAdmin := eqServers(t, &enforce)

	// The legacy answer of every (route, class), and the questions each
	// class is admitted to ask, read first: a widening is told from a
	// collision by them.
	legacyOf := map[string]bool{}
	askedOK := map[string]map[string]string{} // class → question → a route that asks it
	for _, r := range routes {
		surface, governed := authz.HTTPSurface(r.method, r.path)
		for _, c := range classes {
			ok, _ := runMW(t, eqServerFor(c, legacyMain, legacyAdmin), r.method, r.path, c.bearer())
			legacyOf[c.name+"|"+r.method+" "+r.pattern] = ok
			if ok && governed {
				if askedOK[c.name] == nil {
					askedOK[c.name] = map[string]string{}
				}
				askedOK[c.name][questionKey(surface)] = r.method + " " + r.pattern
			}
		}
	}

	var narrowing, widening, collisions []string
	usedAllow, usedNarrow := map[string]bool{}, map[string]bool{}
	admitted := map[string]int{}
	for _, r := range routes {
		surface, governed := authz.HTTPSurface(r.method, r.path)
		for _, c := range classes {
			legacy := legacyOf[c.name+"|"+r.method+" "+r.pattern]
			if legacy {
				admitted[c.name]++
			}
			for _, m := range []struct {
				name       string
				main, admn *Server
			}{{"compat", compatMain, compatAdmin}, {"enforce", enforceMain, enforceAdmin}} {
				got, code := runMW(t, eqServerFor(c, m.main, m.admn), r.method, r.path, c.bearer())
				if got == legacy {
					continue
				}
				if key := c.name + "|" + r.method + " " + r.pattern; m.name == "enforce" && legacy && baselineNarrowerThanGateHTTP[key] != "" {
					usedNarrow[key] = true
					continue
				}
				q := "(ungoverned)"
				if governed {
					_, d := eqEngineAllows(t, eqServerFor(c, enforceMain, enforceAdmin), c, surface)
					q = fmt.Sprintf("asks %s %s:%s → %s %v", surface.Action, surface.Type, surface.Name, d.Outcome, d.Reasons)
				}
				narrowing = append(narrowing, fmt.Sprintf("%s: %s %s — the legacy gate admits=%v, %s mode admits=%v (HTTP %d); %s",
					c.name, r.method, r.pattern, legacy, m.name, got, code, q))
			}
			if !legacy && governed && c.maximal {
				allowed, d := eqEngineAllows(t, eqServerFor(c, enforceMain, enforceAdmin), c, surface)
				if !allowed {
					continue
				}
				q := questionKey(surface)
				key := c.name + "|" + q
				switch {
				case askedOK[c.name][q] != "":
					collisions = append(collisions, fmt.Sprintf("%s: %s %s asks %q, as %s does", c.name, r.method, r.pattern, q, askedOK[c.name][q]))
				case baselineWiderThanGateHTTP[key] != "":
					usedAllow[key] = true
				default:
					widening = append(widening, fmt.Sprintf("%s: %s %s — the legacy gate refuses the widest token of the class, the baseline allows %s (%v)",
						c.name, r.method, r.pattern, q, d.Reasons))
				}
			}
		}
	}
	for key := range baselineWiderThanGateHTTP {
		if !usedAllow[key] {
			widening = append(widening, fmt.Sprintf("%s is allowlisted as baseline-wider but no route needs it any more; remove the entry", key))
		}
	}
	for key := range baselineNarrowerThanGateHTTP {
		if !usedNarrow[key] {
			narrowing = append(narrowing, fmt.Sprintf("%s is allowlisted as baseline-narrower but enforce now admits it; remove the entry", key))
		}
	}
	reportEquivalence(t, narrowing, widening, collisions)
	// The matrix must exercise something: every class is admitted somewhere,
	// and the admin classes everywhere they can be.
	for _, c := range classes {
		if admitted[c.name] == 0 {
			t.Errorf("class %s was admitted to no route; the fixture is not authenticating it", c.name)
		}
	}
	if admitted["admin"] < len(routes)/2 || admitted["legacy"] < len(routes)/2 {
		t.Errorf("admin admitted to %d and legacy to %d of %d routes; the fixture is wrong", admitted["admin"], admitted["legacy"], len(routes))
	}
}

// reportEquivalence fails on every narrowing and unlisted widening, and logs
// the collisions for review (go test -v shows them).
func reportEquivalence(t *testing.T, narrowing, widening, collisions []string) {
	t.Helper()
	sort.Strings(narrowing)
	sort.Strings(widening)
	sort.Strings(collisions)
	for _, d := range narrowing {
		t.Errorf("narrowing: %s", d)
	}
	for _, d := range widening {
		t.Errorf("widening: %s", d)
	}
	for _, d := range collisions {
		t.Logf("collision (the gate asks one question of routes with different reach): %s", d)
	}
}
