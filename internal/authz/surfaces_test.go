package authz

import (
	"context"
	"testing"
)

func TestHTTPSurface_RepresentativeRoutes(t *testing.T) {
	cases := []struct {
		method, path string
		want         Surface
		governed     bool
	}{
		{"POST", "/v1/runs", typeLevel("run", "agent"), true},
		{"GET", "/v1/runs", typeLevel("read", "run"), true},
		{"POST", "/v1/runs:batch", typeLevel("run", "agent"), true},
		{"GET", "/v1/runs/r1/stream", typeLevel("read", "run"), true},
		{"POST", "/v1/runs/r1/input", typeLevel("steer", "run"), true},
		{"POST", "/v1/runs/r1/interrupts/i1/resolve", typeLevel("steer", "run"), true},
		{"DELETE", "/v1/runs/r1/tree", typeLevel("delete", "run"), true},
		{"POST", "/v1/sessions/s1/messages", typeLevel("run", "agent"), true},
		{"DELETE", "/v1/sessions/s1", typeLevel("delete", "run"), true},
		{"POST", "/v1/agents/a1/cancel", typeLevel("cancel", "run"), true},
		{"GET", "/v1/agents/chat/channels", typeLevel("subscribe", "channel"), true},
		{"POST", "/v1/users/u1/channels/ops/publish", instance("publish", "channel", "ops"), true},
		{"GET", "/v1/users/u1/channels/ops/peek", instance("subscribe", "channel", "ops"), true},
		{"POST", "/v1/chat/completions", instance("use", "gateway", "chat"), true},
		{"POST", "/v1/embeddings", instance("use", "gateway", "embeddings"), true},
		{"POST", "/v1/_agentdef", instance("read", "def", "agent/*"), true},
		// Every acldef op is a POST with the op in the body: the route asks
		// read, and the handler asks author or retire of the named list.
		{"POST", "/v1/_acldef", instance("read", "def", "acl/*"), true},
		{"GET", "/v1/_acldef/names", instance("read", "def", "acl/*"), true},
		{"GET", "/v1/_aclbinding", instance("read", "def", "acl/*"), true},
		{"POST", "/v1/_aclbinding", instance("author", "def", "acl/*"), true},
		{"DELETE", "/v1/_aclbinding/b1", instance("author", "def", "acl/*"), true},
		{"GET", "/v1/_a2aservercarddef/names", instance("read", "def", "a2aserver/*"), true},
		{"GET", "/v1/_agentdef/chat/overridable", typeLevel("read", "agent"), true},
		{"GET", "/v1/_teamdef/t1/channels", typeLevel("subscribe", "channel"), true},
		{"POST", "/v1/_operatortokendef", instance("mint", "principal", "token:*"), true},
		{"POST", "/v1/_channels/ops/publish", instance("publish", "channel", "ops"), true},
		{"PATCH", "/v1/_channels/ops", instance("manage", "channel", "ops"), true},
		{"POST", "/v1/_channels/_await", typeLevel("await", "channel"), true},
		{"GET", "/v1/_memory/scopes", typeLevel("read", "memory"), true},
		{"PUT", "/v1/_memory/scopes/user/u1/keys/k", typeLevel("write", "memory"), true},
		{"POST", "/v1/_memory/repair-tenant", instance("manage", "runtime", "tenants"), true},
		{"POST", "/v1/_document", typeLevel("read", "document"), true},
		{"POST", "/v1/_document/reindex", typeLevel("write", "document"), true},
		{"PUT", "/v1/_limits", instance("manage", "runtime", "limits"), true},
		{"GET", "/v1/_limits", instance("read", "runtime", "limits"), true},
		{"POST", "/v1/_users", instance("manage", "principal", "user:*"), true},
		{"POST", "/v1/_users/alice/tokens", instance("mint", "principal", "user:alice"), true},
		{"GET", "/v1/_users/alice", instance("read", "principal", "user:alice"), true},
		{"POST", "/v1/_snapshots", instance("manage", "runtime", "snapshot"), true},
		{"GET", "/v1/_events", instance("read", "runtime", "events"), true},
		{"POST", "/v1/_mcp", instance("use", "transport", "mcp"), true},
		{"POST", "/v1/_decide", instance("use", "gateway", "decide"), true},
		{"GET", "/v1/_me", Surface{}, false},
		{"GET", "/v1/config", Surface{}, false},
		{"GET", "/healthz", Surface{}, false},
		{"GET", "/metrics", instance("read", "runtime", "metrics"), true},
		{"GET", "/ui/", Surface{}, false},
		{"POST", "/v1/_authz/evaluation", Surface{}, false},
	}
	for _, c := range cases {
		got, governed := HTTPSurface(c.method, c.path)
		if governed != c.governed || got != c.want {
			t.Errorf("HTTPSurface(%s %s) = %+v, %v; want %+v, %v", c.method, c.path, got, governed, c.want, c.governed)
		}
	}
}

func TestSurfaces_EveryMappedPairIsInTheCatalog(t *testing.T) {
	routes := []struct{ m, p string }{
		{"POST", "/v1/runs"}, {"GET", "/v1/runs"}, {"POST", "/v1/runs/x/retune"}, {"POST", "/v1/runs/x/review"},
		{"POST", "/v1/runs/x/compact"}, {"POST", "/v1/runs/x/cancel"}, {"PUT", "/v1/runs/x/breakpoints"},
		{"POST", "/v1/_skilldef"}, {"POST", "/v1/_schedules/x/run-now"}, {"GET", "/v1/_schedules/list-all"},
		{"POST", "/v1/_channels"}, {"GET", "/v1/_channels"}, {"POST", "/v1/_channels/x/ack"}, {"POST", "/v1/_channels/_broadcast"},
		{"POST", "/v1/_path"}, {"GET", "/v1/_volumes"}, {"POST", "/v1/_volumedef"}, {"POST", "/v1/_credentialdef"},
		{"POST", "/v1/_crypto"}, {"GET", "/v1/_usage"}, {"DELETE", "/v1/_retention/overrides"}, {"POST", "/v1/_erasure"},
		{"GET", "/v1/_routing"}, {"POST", "/v1/_config/reload"}, {"GET", "/v1/_presets"}, {"GET", "/v1/_tenants"},
		{"POST", "/v1/_pause"}, {"GET", "/v1/_state"}, {"GET", "/v1/_metrics/summary"}, {"GET", "/v1/_resident"},
		{"POST", "/v1/_resident/x/close"}, {"GET", "/v1/_runnable-agents"}, {"GET", "/v1/_library/skills"},
		{"GET", "/v1/_library/mcp-servers"}, {"POST", "/v1/_history"}, {"POST", "/v1/_evaluation"},
		{"GET", "/v1/_ontology"}, {"POST", "/v1/_ontology/adopt"}, {"POST", "/v1/_webhooks/x/test"},
		{"GET", "/v1/_groups"}, {"POST", "/v1/_acldef"}, {"GET", "/v1/_acldef/names"},
	}
	for _, r := range routes {
		s, ok := HTTPSurface(r.m, r.p)
		if !ok {
			t.Errorf("%s %s is not governed", r.m, r.p)
			continue
		}
		if !ValidVerb(s.Type, s.Action) {
			t.Errorf("%s %s → %s %s: not a catalog pair", r.m, r.p, s.Action, s.Type)
		}
		if s.Name != AnyInstance {
			if _, err := parseSelector(s.Type + ":" + s.Name); err != nil {
				t.Errorf("%s %s → %s:%s: %v", r.m, r.p, s.Type, s.Name, err)
			}
		}
	}
	for _, rpc := range []string{"Run", "Continue", "StreamRun", "DeleteRunTree", "Decide", "PublishChannel", "AgentDef", "Memory", "OperatorTokenDef", "RestoreSnapshot", "UsageReport", "DirectoryTenants", "History", "Crypto", "TeamDef", "VolumeDef", "AwaitChannels"} {
		s, ok := GRPCSurface(rpc)
		if !ok || !ValidVerb(s.Type, s.Action) {
			t.Errorf("GRPCSurface(%s) = %+v, %v", rpc, s, ok)
		}
	}
	for _, rpc := range []string{"Health", "Config", "NoSuchRPC"} {
		if _, ok := GRPCSurface(rpc); ok {
			t.Errorf("GRPCSurface(%s) should be ungoverned", rpc)
		}
	}
}

func TestAnyInstance_MatchesOnlyCoveringSelectors(t *testing.T) {
	cases := map[string]bool{
		"agent:*": true, "agent:**": true, "agent:admin/*": false, "agent:x": false,
		"memory:*": true, "memory:tenant": false, "memory:*:x*": false,
		"document:*": true, "document:tenant:/kb/**": false,
		"run:*": true, "run:own": false,
		"def:*": true, "def:agent/*": false,
	}
	for sel, want := range cases {
		s, err := parseSelector(sel)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.matches(catalogByName[s.typ], AnyInstance); got != want {
			t.Errorf("%q matches AnyInstance = %v, want %v", sel, got, want)
		}
	}
}

// At the type level an allow on SOME instances makes the route reachable,
// a deny on SOME instances is nothing, and a deny on ALL instances refuses.
func TestDecide_TypeLevelSemantics(t *testing.T) {
	partialAllow := ACL{Name: "pa", Rules: []Rule{{Effect: Allow, Actions: []string{"read"}, Resources: []string{"run:own"}}}}
	partialDeny := ACL{Name: "pd", Rules: []Rule{{Effect: Deny, Actions: []string{"run"}, Resources: []string{"agent:admin/*"}}}}
	fullDeny := ACL{Name: "fd", Rules: []Rule{{Effect: Deny, Actions: []string{"invoke"}, Resources: []string{"tool:*"}}}}
	e, err := Compile(Input{Subject: Subject{TenantID: "t", ID: "u"},
		Bindings: []Binding{{ACL: "pa", SubjectKind: SubjectTenant}, {ACL: "pd", SubjectKind: SubjectTenant}, {ACL: "fd", SubjectKind: SubjectTenant}},
		ACLs:     map[string]ACLVersion{"pa": {ACL: partialAllow}, "pd": {ACL: partialDeny}, "fd": {ACL: fullDeny}}, Now: testNow})
	if err != nil {
		t.Fatal(err)
	}
	if got := ask(e, "read", "run", AnyInstance); got != Allowed {
		t.Errorf("partial allow at type level = %v, want allowed", got)
	}
	if got := ask(e, "run", "agent", AnyInstance); got != NoMatch {
		t.Errorf("partial deny at type level = %v, want no_match", got)
	}
	if got := ask(e, "invoke", "tool", AnyInstance); got != Denied {
		t.Errorf("full deny at type level = %v, want denied", got)
	}
	// The instance questions are unchanged.
	if ask(e, "run", "agent", "admin/x") != Denied || ask(e, "read", "run", "tenant") != NoMatch {
		t.Error("instance questions")
	}
}

func TestEvaluate_PerMode(t *testing.T) {
	deny := ACL{Name: "d", Rules: []Rule{{Effect: Deny, Actions: []string{"read"}, Resources: []string{"run:*"}}}}
	allow := ACL{Name: "a", Rules: []Rule{{Effect: Allow, Actions: []string{"steer"}, Resources: []string{"run:*"}}}}
	eff, err := Compile(Input{Subject: Subject{TenantID: "t", ID: "u"}, Bindings: []Binding{{ACL: "d", SubjectKind: SubjectTenant}, {ACL: "a", SubjectKind: SubjectTenant}},
		ACLs: map[string]ACLVersion{"d": {ACL: deny}, "a": {ACL: allow}}, Now: testNow})
	if err != nil {
		t.Fatal(err)
	}
	read := typeLevel("read", "run")
	steer := typeLevel("steer", "run")
	cancel := typeLevel("cancel", "run")
	for _, c := range []struct {
		mode             Mode
		surface          Surface
		eff              *Effective
		allowed, unavail bool
		diverged         bool
	}{
		{ModeCompat, read, eff, false, false, false},  // explicit deny holds in compat
		{ModeCompat, cancel, eff, true, false, false}, // no match → legacy (true)
		{ModeCompat, read, nil, true, false, false},   // no set → legacy
		{ModeShadow, read, eff, true, false, true},    // legacy decides, divergence noted
		{ModeShadow, steer, eff, true, false, false},
		{ModeEnforce, steer, eff, true, false, false},
		{ModeEnforce, cancel, eff, false, false, false}, // no match → deny
		{ModeEnforce, read, nil, false, true, false},    // no set → unavailable
	} {
		ctx := WithTransport(WithMode(context.Background(), c.mode), "http")
		res := EvaluateSurface(ctx, c.eff, c.surface, true)
		if res.Allowed != c.allowed || res.Unavailable != c.unavail || res.Diverged != c.diverged {
			t.Errorf("%s %s: %+v, want allowed=%v unavailable=%v diverged=%v", c.mode, c.surface.Action, res, c.allowed, c.unavail, c.diverged)
		}
	}
	if ModeFrom(context.Background()) != ModeCompat || TransportFrom(context.Background()) != "" {
		t.Error("ctx defaults")
	}
}
