package mcp

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EY §11 / RFC EZ §2 — the baseline equivalence test, MCP half: for
// every meta-tool the /v1/_mcp server lists or classifies and every
// principal class, the class's built-in baseline reproduces what
// principalMayCallTool (the run-plane list, the class allowlists and
// toolRequiredScope) admits. The run-plane classes are checked in the
// narrowing direction only, as on HTTP. Opening the session itself is the HTTP route
// POST /v1/_mcp, asserted by the HTTP half, which also says why the two
// directions are asserted the way they are.

// noACLRows is an ACL store with nothing authored: every principal's set is
// its class baseline alone, compiled by the real resolver.
type noACLRows struct{}

func (noACLRows) GroupsOfSubject(context.Context, string, string) ([]string, error) { return nil, nil }
func (noACLRows) GroupGraph(context.Context, string) (map[string][]string, error)   { return nil, nil }
func (noACLRows) AclBindingsFor(context.Context, string, string, []string, string) ([]store.AclBindingRow, error) {
	return nil, nil
}
func (noACLRows) AclDefGet(context.Context, string, string) (store.AclDefRow, error) {
	return store.AclDefRow{}, &store.ErrNotFound{}
}
func (noACLRows) AclVersion(context.Context, string) (int64, error) { return 1, nil }

type eqClass struct {
	name    string
	scopes  []string
	agents  []string
	maximal bool
}

var eqMemberScopes = []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeRunsDelete,
	auth.ScopeChannelPublish, auth.ScopeChannelRead, auth.ScopeLLMGateway, auth.ScopeProvidersOperatorKey}

func eqClasses() []eqClass {
	return []eqClass{
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

// baselineWiderThanGateMCP lists the (class, tool) pairs where the class's
// baseline allows invoking the meta-tool though principalMayCallTool refuses
// even the widest token of the class. Each needs a reason; keep it near-empty.
var baselineWiderThanGateMCP = map[string]string{}

func (c eqClass) principal(t *testing.T, mode authz.Mode) auth.Principal {
	t.Helper()
	p := auth.Principal{TenantID: "acme", Subject: c.name, TokenDefID: "def_" + c.name, Scopes: c.scopes,
		Restrictions: auth.Restrictions{Agents: c.agents}}
	out, err := aclres.New(noACLRows{}, mode, 0).Resolve(context.Background(), p)
	if err != nil || out.Effective == nil {
		t.Fatalf("class %s: resolve: %v", c.name, err)
	}
	return out
}

// eqTools is every meta-tool name the server lists, and every name its
// classification tables name (a dispatchable tool with no descriptor is
// still gated).
func eqTools() []string {
	set := map[string]bool{}
	for _, td := range toolDescriptors() {
		set[td.Name] = true
	}
	for _, m := range []map[string]bool{tenantConfinableTools, adminOnlyTools, userSelfServiceTools, runPlaneTools} {
		for name := range m {
			set[name] = true
		}
	}
	for name := range toolRequiredScope {
		set[name] = true
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestACLBaseline_MCPToolsMatchTheLegacyGatePerClass(t *testing.T) {
	var narrowing, widening []string
	admitted := map[string]int{}
	tools := eqTools()
	if len(tools) < 40 {
		t.Fatalf("only %d tools; the enumeration is not reading the descriptors", len(tools))
	}
	for _, tool := range tools {
		surface := authz.MCPToolSurface(tool)
		for _, c := range eqClasses() {
			bare := c.principal(t, authz.ModeCompat)
			bare.Effective = nil
			legacy := principalMayCallTool(auth.WithPrincipal(context.Background(), bare), tool)
			if legacy {
				admitted[c.name]++
			}
			for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
				p := c.principal(t, mode)
				ctx := auth.WithPrincipal(authz.WithTransport(authz.WithMode(context.Background(), mode), "mcp"), p)
				if got := principalMayCallTool(ctx, tool); got != legacy {
					d := authz.EvaluateSurface(authz.WithMode(context.Background(), authz.ModeEnforce), p.Effective, surface, false).Decision
					narrowing = append(narrowing, fmt.Sprintf("%s: %s — the legacy gate admits=%v, %s mode admits=%v; asks %s %s:%s → %s %v",
						c.name, tool, legacy, mode, got, surface.Action, surface.Type, surface.Name, d.Outcome, d.Reasons))
				}
			}
			if !legacy && c.maximal {
				p := c.principal(t, authz.ModeEnforce)
				res := authz.EvaluateSurface(authz.WithMode(context.Background(), authz.ModeEnforce), p.Effective, surface, false)
				key := c.name + "|" + tool
				_, listed := baselineWiderThanGateMCP[key]
				switch {
				case res.Allowed && !listed:
					widening = append(widening, fmt.Sprintf("%s: %s — principalMayCallTool refuses the widest token of the class, the baseline allows %s %s:%s (%v)",
						c.name, tool, surface.Action, surface.Type, surface.Name, res.Decision.Reasons))
				case !res.Allowed && listed:
					widening = append(widening, fmt.Sprintf("%s is allowlisted as baseline-wider but the baseline no longer allows it; remove the entry", key))
				}
				// Each tool is its own question (invoke mcp-tool:<name>), so
				// no two tools collide.
			}
		}
	}
	sort.Strings(narrowing)
	sort.Strings(widening)
	for _, d := range narrowing {
		t.Errorf("narrowing: %s", d)
	}
	for _, d := range widening {
		t.Errorf("widening: %s", d)
	}
	for _, c := range eqClasses() {
		// An isolated run-plane token reaches no meta-tool: its class list
		// (self-service) and the run plane share none.
		if c.name != "isolated+runplane" && admitted[c.name] == 0 {
			t.Errorf("class %s was admitted to no tool; the fixture is wrong", c.name)
		}
	}
	if admitted["admin"] != len(tools) {
		t.Errorf("admin admitted to %d of %d tools", admitted["admin"], len(tools))
	}
}
