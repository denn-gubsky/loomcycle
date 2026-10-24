package authz

import "testing"

func askScope(e *Effective, action, typ, scope string) Outcome {
	return e.DecideScope(Request{Subject: e.Subject, Action: action, Resource: Resource{Type: typ, Name: scope, TenantID: e.Subject.TenantID}, Context: Context{Transport: "http", Now: testNow}}).Outcome
}

// A question about EVERY document of a scope (a search, a whole-scope op) is
// the mirror of the type-level one: a deny on any part of the scope refuses
// it, an allow must cover the whole scope.
func TestDecideScope_DenyOnAnyPartAllowOnlyOnTheWhole(t *testing.T) {
	alice := Subject{TenantID: "acme", ID: "alice"}
	partDeny := mustACL(t, `{"name":"no-secret","rules":[{"effect":"deny","actions":["read"],"resources":["document:tenant:/kb/secret/**"]}]}`)
	wholeAllow := mustACL(t, `{"name":"tenant-docs","rules":[{"effect":"allow","actions":["read","write"],"resources":["document:tenant"]}]}`)
	partAllow := mustACL(t, `{"name":"kb-only","rules":[{"effect":"allow","actions":["read"],"resources":["document:user:/kb/**"]}]}`)
	starAllow := mustACL(t, `{"name":"all-user","rules":[{"effect":"allow","actions":["read"],"resources":["document:*:/**"]}]}`)
	e := compile(t, alice, []Binding{
		{ACL: "no-secret", SubjectKind: SubjectUser, SubjectID: "alice"},
		{ACL: "tenant-docs", SubjectKind: SubjectUser, SubjectID: "alice"},
		{ACL: "kb-only", SubjectKind: SubjectUser, SubjectID: "alice"},
	}, partDeny, wholeAllow, partAllow)

	cases := []struct {
		action, scope string
		want          Outcome
	}{
		{"read", "tenant", Denied},   // a part is denied, so not every document is readable
		{"write", "tenant", Allowed}, // no deny on write; the allow covers the scope
		{"read", "user", NoMatch},    // the allow covers only /kb/**
		{"read", "agent", NoMatch},
	}
	for _, c := range cases {
		if got := askScope(e, c.action, "document", c.scope); got != c.want {
			t.Errorf("DecideScope(%s document:%s) = %v, want %v", c.action, c.scope, got, c.want)
		}
	}
	// An instance outside the denied part is still allowed by Decide; the
	// scope question is about all of them at once.
	if got := ask(e, "read", "document", "tenant:/kb/public/a"); got != Allowed {
		t.Errorf("Decide(read document:tenant:/kb/public/a) = %v, want allow", got)
	}

	// `**` under every scope covers each scope whole.
	e2 := compile(t, alice, []Binding{{ACL: "all-user", SubjectKind: SubjectUser, SubjectID: "alice"}}, starAllow)
	if got := askScope(e2, "read", "document", "user"); got != Allowed {
		t.Errorf("DecideScope(read document:user) under document:*:/** = %v, want allow", got)
	}
}

func TestHasInstanceRules_OnlyARuleNamingPartOfTheScope(t *testing.T) {
	alice := Subject{TenantID: "acme", ID: "alice"}
	whole := mustACL(t, `{"name":"whole","rules":[{"effect":"deny","actions":["write"],"resources":["document:agent","document:user:/**"]}]}`)
	part := mustACL(t, `{"name":"part","rules":[{"effect":"allow","actions":["read"],"resources":["document:*:/kb/**"]}]}`)
	e := compile(t, alice, []Binding{{ACL: "whole", SubjectKind: SubjectUser, SubjectID: "alice"}}, whole)
	for _, scope := range []string{"agent", "user", "tenant"} {
		if e.HasInstanceRules("document", scope) {
			t.Errorf("HasInstanceRules(document, %s) = true for scope-wide rules only", scope)
		}
	}
	e2 := compile(t, alice, []Binding{{ACL: "part", SubjectKind: SubjectUser, SubjectID: "alice"}}, part)
	for _, scope := range []string{"agent", "user", "tenant"} {
		if !e2.HasInstanceRules("document", scope) {
			t.Errorf("HasInstanceRules(document, %s) = false under document:*:/kb/**", scope)
		}
	}
	if e2.HasInstanceRules("memory", "tenant") {
		t.Error("HasInstanceRules(memory, tenant) = true with no memory rule")
	}
}

func askScopeOn(e *Effective, action, typ, scope, transport string) Outcome {
	return e.DecideScope(Request{Subject: e.Subject, Action: action, Resource: Resource{Type: typ, Name: scope, TenantID: e.Subject.TenantID}, Context: Context{Transport: transport, Now: testNow}}).Outcome
}

// A condition the scope question cannot decide for every instance (whose
// owner, which agent, how deep) is read the safe way round: a deny under it
// may apply to some instance, so it holds; an allow under it may not cover
// every instance, so it does not.
func TestDecideScope_UndecidableConditionsReadTheSafeWay(t *testing.T) {
	alice := Subject{TenantID: "acme", ID: "alice"}
	ownDeny := mustACL(t, `{"name":"own-deny","rules":[
		{"effect":"allow","actions":["read"],"resources":["document:tenant"]},
		{"effect":"deny","actions":["read"],"resources":["document:tenant:/**"],"when":{"owner_is_self":true}}]}`)
	e := compile(t, alice, []Binding{{ACL: "own-deny", SubjectKind: SubjectUser, SubjectID: "alice"}}, ownDeny)
	if got := askScope(e, "read", "document", "tenant"); got == Allowed {
		t.Error("an owner_is_self deny left the tenant scope wholly readable")
	}
	for _, c := range []string{`{"owner_is_self":true}`, `{"agent":"chat/*"}`, `{"depth":{"max":1}}`, `{"user_tier":["pro"]}`} {
		ownAllow := mustACL(t, `{"name":"cond-allow","rules":[{"effect":"allow","actions":["read"],"resources":["document:tenant"],"when":`+c+`}]}`)
		e2 := compile(t, alice, []Binding{{ACL: "cond-allow", SubjectKind: SubjectUser, SubjectID: "alice"}}, ownAllow)
		if got := askScope(e2, "read", "document", "tenant"); got == Allowed {
			t.Errorf("an allow under %s covered the whole tenant scope", c)
		}
		if !e2.HasInstanceRules("document", "tenant") {
			t.Errorf("HasInstanceRules = false with a rule under %s on the scope", c)
		}
	}
	// A condition the request does answer still decides: a transport deny
	// holds on its transport and not on another.
	mcpDeny := mustACL(t, `{"name":"mcp-deny","rules":[
		{"effect":"allow","actions":["read"],"resources":["document:tenant"]},
		{"effect":"deny","actions":["read"],"resources":["document:tenant:/kb/**"],"when":{"transport":["mcp"]}}]}`)
	e3 := compile(t, alice, []Binding{{ACL: "mcp-deny", SubjectKind: SubjectUser, SubjectID: "alice"}}, mcpDeny)
	if got := askScopeOn(e3, "read", "document", "tenant", "mcp"); got != Denied {
		t.Errorf("mcp transport: %v, want deny", got)
	}
	if got := askScopeOn(e3, "read", "document", "tenant", "http"); got != Allowed {
		t.Errorf("http transport: %v, want allow", got)
	}
}
