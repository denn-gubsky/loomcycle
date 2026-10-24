package authz

import (
	"reflect"
	"testing"
)

// TestCatalog_Shape pins the closed set: a type or verb added or removed
// shows up here, so the change is reviewed with the surfaces that enforce
// it (RFC EY §4).
func TestCatalog_Shape(t *testing.T) {
	want := []string{"a2a", "agent", "channel", "client", "credential", "crypto", "def", "document", "evaluation", "gateway", "git", "history", "interruption", "localapi", "mcp", "mcp-tool", "memory", "model", "principal", "run", "runtime", "search", "skill", "sqlmem", "team", "tier", "tool", "transport", "volume"}
	if got := Types(); !reflect.DeepEqual(got, want) {
		t.Errorf("Types() = %v\nwant %v", got, want)
	}
	for _, spec := range catalog {
		if len(spec.Verbs) == 0 {
			t.Errorf("%s has no verbs", spec.Name)
		}
		if spec.isEnum() && spec.Sep != 0 {
			t.Errorf("%s: enum with a separator", spec.Name)
		}
		for _, v := range spec.Verbs {
			if !ValidVerb(spec.Name, v) {
				t.Errorf("ValidVerb(%s, %s) = false", spec.Name, v)
			}
		}
	}
	if ValidVerb("agent", "invoke") || ValidVerb("nosuch", "run") {
		t.Error("ValidVerb admits a wrong pair")
	}
	if _, ok := TypeSpecOf("agent"); !ok {
		t.Error("TypeSpecOf(agent)")
	}
}

func TestSplitSelector(t *testing.T) {
	typ, name, err := SplitSelector("document:tenant:/kb/x")
	if err != nil || typ != "document" || name != "tenant:/kb/x" {
		t.Errorf("= %q %q %v", typ, name, err)
	}
	for _, bad := range []string{"", ":x", "agent", "nosuch:x"} {
		if _, _, err := SplitSelector(bad); err == nil {
			t.Errorf("SplitSelector(%q) should fail", bad)
		}
	}
}

func TestBaselines_ValidateAgainstTheCatalog(t *testing.T) {
	for name, a := range Baselines() {
		if a.Name != name || !IsBuiltin(name) {
			t.Errorf("%s: name %q", name, a.Name)
		}
		if err := a.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if IsBuiltin("support") || !IsBuiltin("builtin:x") {
		t.Error("IsBuiltin")
	}
}

func TestBaselines_ClassesDifferWhereTheyShould(t *testing.T) {
	bl := Baselines()
	compileClass := func(name string) *Effective {
		e, err := Compile(Input{Subject: Subject{TenantID: "t", ID: "alice"}, Bindings: []Binding{{ACL: name, SubjectKind: SubjectTenant}}, ACLs: map[string]ACLVersion{name: {ACL: bl[name]}}, Now: testNow})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	admin, op, member, iso := compileClass(BaselineAdmin), compileClass(BaselineTenantOperator), compileClass(BaselineMember), compileClass(BaselineIsolated)
	check := func(e *Effective, action, typ, name string, want Outcome) {
		t.Helper()
		if got := ask(e, action, typ, name); got != want {
			t.Errorf("%s: %s %s:%s = %v, want %v", e.Subject.ID, action, typ, name, got, want)
		}
	}
	// Admin: everything.
	check(admin, "mint", "principal", "token:x", Allowed)
	check(admin, "manage", "runtime", "snapshot", Allowed)
	// Tenant operator: not the operator plane.
	check(op, "mint", "principal", "user:x", Allowed)
	check(op, "manage", "runtime", "snapshot", NoMatch)
	check(op, "mint", "principal", "token:x", NoMatch)
	check(op, "author", "def", "acl/x", Allowed)
	// Member: the collaboration plane, not controls.
	check(member, "run", "agent", "chat/x", Allowed)
	check(member, "author", "def", "agent/x", Allowed)
	check(member, "author", "def", "acl/x", NoMatch)
	check(member, "manage", "runtime", "limits", NoMatch)
	check(member, "read", "memory", "tenant", Allowed)
	check(member, "mint", "principal", "user:x", NoMatch)
	// Isolated: own scope only; the shared keyspace is an explicit deny.
	check(iso, "run", "agent", "chat/x", Allowed)
	check(iso, "read", "memory", "user", Allowed)
	check(iso, "read", "memory", "tenant", Denied)
	check(iso, "read", "document", "tenant:/kb/x", Denied)
	// The gateway is the scope gate's to refuse (llm:gateway by name; the
	// decision call on runs:create): a baseline deny refused both even in
	// compat (TestACLBaseline_HTTPRoutesMatchTheLegacyGatePerClass).
	check(iso, "use", "gateway", "chat", Allowed)
	check(iso, "read", "run", "tenant", NoMatch)
	// Member browse of a colleague's user scope: owner_is_self only (D7).
	self := member.Decide(Request{Subject: member.Subject, Action: "browse", Resource: Resource{Type: "memory", Name: "user:alice", Owner: "alice"}})
	other := member.Decide(Request{Subject: member.Subject, Action: "browse", Resource: Resource{Type: "memory", Name: "user:bob", Owner: "bob"}})
	if self.Outcome != Allowed || other.Outcome != NoMatch {
		t.Errorf("browse self=%v other=%v", self.Outcome, other.Outcome)
	}
}

// A walk op asks run, read or cancel of team:<name> (TeamDef run/poll/cancel).
// Every class whose legacy gates let it start a walk — an isolated user
// included: a substrate:user caller's walk runs isolated — must be admitted
// by its baseline, or enforce with no lists authored would refuse what
// compat admits. The run-plane list's deny on run is deliberate (RFC EF: a
// token limited to certain agents may not reach others through a walk).
func TestBaselines_EveryWalkingClassMayRunReadAndCancelAWalk(t *testing.T) {
	bl := Baselines()
	compile := func(names ...string) *Effective {
		in := Input{Subject: Subject{TenantID: "t", ID: "alice"}, ACLs: map[string]ACLVersion{}, Now: testNow}
		for _, n := range names {
			in.Bindings = append(in.Bindings, Binding{ACL: n, SubjectKind: SubjectTenant})
			in.ACLs[n] = ACLVersion{ACL: bl[n]}
		}
		e, err := Compile(in)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	for _, class := range []string{BaselineAdmin, BaselineTenantOperator, BaselineMember, BaselineIsolated} {
		e := compile(class)
		for _, verb := range []string{"run", "read", "cancel"} {
			if got := ask(e, verb, "team", "rev"); got != Allowed {
				t.Errorf("%s: %s team:rev = %v, want allowed", class, verb, got)
			}
		}
	}
	runPlane := compile(BaselineMember, BaselineRunPlane)
	if got := ask(runPlane, "run", "team", "rev"); got != Denied {
		t.Errorf("run-plane: run team:rev = %v, want denied", got)
	}
	if got := ask(runPlane, "cancel", "team", "rev"); got != Allowed {
		t.Errorf("run-plane: cancel team:rev = %v, want allowed", got)
	}
}
