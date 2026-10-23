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
