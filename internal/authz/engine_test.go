package authz

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC) // a Tuesday

func mustACL(t *testing.T, src string) ACL {
	t.Helper()
	a, err := ParseACL([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func compile(t *testing.T, subj Subject, bindings []Binding, acls ...ACL) *Effective {
	t.Helper()
	in := Input{Subject: subj, Bindings: bindings, ACLs: map[string]ACLVersion{}, Now: testNow, Version: 7}
	for i, a := range acls {
		in.ACLs[a.Name] = ACLVersion{ACL: a, Version: i + 1}
	}
	e, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func ask(e *Effective, action, typ, name string) Outcome {
	return e.Decide(Request{Subject: e.Subject, Action: action, Resource: Resource{Type: typ, Name: name, TenantID: e.Subject.TenantID}, Context: Context{Transport: "http", Now: testNow}}).Outcome
}

func TestDecide_TheOwnersThreeCases(t *testing.T) {
	// Run certain agents by mask, read/write certain volumes, call certain
	// tools — bound to a GROUP, with a per-user deny on top.
	support := mustACL(t, supportACL)
	noTenantMemory := mustACL(t, `{"name":"contractor","rules":[{"effect":"deny","actions":["read","write","recall"],"resources":["memory:tenant"]}]}`)
	alice := Subject{TenantID: "acme", ID: "alice", Groups: []string{"support"}}
	e := compile(t, alice, []Binding{
		{ACL: "support-tier-1", SubjectKind: SubjectGroup, SubjectID: "support"},
		{ACL: "contractor", SubjectKind: SubjectUser, SubjectID: "alice"},
	}, support, noTenantMemory)

	cases := []struct {
		action, typ, name string
		want              Outcome
	}{
		{"run", "agent", "chat/support", Allowed},
		{"run", "agent", "support/l2/escalation", Allowed},
		{"run", "agent", "admin/users", Denied},
		{"run", "agent", "ops/deploy", NoMatch},
		{"read", "volume", "shared-ro", Allowed},
		{"write", "volume", "shared-ro", NoMatch},
		{"write", "volume", "support-scratch", Allowed},
		{"invoke", "tool", "Read", Allowed}, // Tuesday 10:30 UTC = 13:30 Kyiv, inside the window
		{"invoke", "tool", "Bash", Denied},  // the deny wins over tool:*
		{"invoke", "tool", "Bashbox", Denied},
		{"read", "memory", "tenant", Denied},
		{"read", "memory", "user", NoMatch},
	}
	for _, c := range cases {
		if got := ask(e, c.action, c.typ, c.name); got != c.want {
			t.Errorf("%s %s:%s = %v, want %v", c.action, c.typ, c.name, got, c.want)
		}
	}
	// Outside the window the conditional allow does not hold.
	d := e.Decide(Request{Subject: alice, Action: "invoke", Resource: Resource{Type: "tool", Name: "Read"}, Context: Context{Transport: "http", Now: testNow.Add(14 * time.Hour)}})
	if d.Outcome != NoMatch {
		t.Errorf("outside the window: %v, want no_match", d.Outcome)
	}
	// On another transport it does not hold either.
	d = e.Decide(Request{Subject: alice, Action: "invoke", Resource: Resource{Type: "tool", Name: "Read"}, Context: Context{Transport: "grpc", Now: testNow}})
	if d.Outcome != NoMatch {
		t.Errorf("on grpc: %v, want no_match", d.Outcome)
	}
}

func TestDecide_ReasonsNameEveryMatchingRuleDeniesFirst(t *testing.T) {
	a := mustACL(t, `{"name":"a","rules":[
		{"effect":"allow","actions":["invoke"],"resources":["tool:*"]},
		{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]},
		{"effect":"deny","actions":["invoke"],"resources":["tool:Ba*"]}]}`)
	e := compile(t, Subject{TenantID: "t", ID: "u"}, []Binding{{ACL: "a", SubjectKind: SubjectTenant}}, a)
	d := e.Decide(Request{Subject: e.Subject, Action: "invoke", Resource: Resource{Type: "tool", Name: "Bash"}})
	want := []Reason{{ACL: "a", Version: 1, RuleIndex: 1, Effect: Deny}, {ACL: "a", Version: 1, RuleIndex: 2, Effect: Deny}, {ACL: "a", Version: 1, RuleIndex: 0, Effect: Allow}}
	if d.Outcome != Denied || !reflect.DeepEqual(d.Reasons, want) {
		t.Errorf("got %+v, want denied with %+v", d, want)
	}
}

func TestDecide_TenantFloorBeforeAnyRule(t *testing.T) {
	a := mustACL(t, `{"name":"a","rules":[{"effect":"allow","actions":["read"],"resources":["run:*"]}]}`)
	e := compile(t, Subject{TenantID: "acme", ID: "u"}, []Binding{{ACL: "a", SubjectKind: SubjectTenant}}, a)
	d := e.Decide(Request{Subject: e.Subject, Action: "read", Resource: Resource{Type: "run", Name: "tenant", TenantID: "other"}})
	if d.Outcome != Denied || !d.TenantFloor || len(d.Reasons) != 0 {
		t.Errorf("cross-tenant: %+v, want the tenant floor with no rule read", d)
	}
	// An operator-global resource (tenant "") is reachable from any tenant.
	if ask(e, "read", "run", "tenant") != Allowed {
		t.Error("own tenant")
	}
	d = e.Decide(Request{Subject: e.Subject, Action: "read", Resource: Resource{Type: "run", Name: "tenant"}})
	if d.Outcome != Allowed {
		t.Errorf("global resource: %v", d.Outcome)
	}
}

func TestCompile_FiltersBindingsBySubjectAndExpiry(t *testing.T) {
	a := mustACL(t, `{"name":"a","rules":[{"effect":"allow","actions":["invoke"],"resources":["tool:*"]}]}`)
	b := mustACL(t, `{"name":"b","rules":[{"effect":"deny","actions":["invoke"],"resources":["tool:*"]}]}`)
	subj := Subject{TenantID: "t", ID: "alice", Groups: []string{"g1"}, TokenDefID: "tok"}
	e := compile(t, subj, []Binding{
		{ACL: "a", SubjectKind: SubjectGroup, SubjectID: "g1"},
		{ACL: "b", SubjectKind: SubjectUser, SubjectID: "bob"},                                      // not her
		{ACL: "b", SubjectKind: SubjectGroup, SubjectID: "g2"},                                      // not her group
		{ACL: "b", SubjectKind: SubjectToken, SubjectID: "tok", ExpiresAt: testNow.Add(-time.Hour)}, // expired
	}, a, b)
	if got := ask(e, "invoke", "tool", "Bash"); got != Allowed {
		t.Errorf("= %v, want allowed (the deny bindings do not apply)", got)
	}
	if len(e.Applied()) != 1 {
		t.Errorf("applied = %+v", e.Applied())
	}
	e = compile(t, subj, []Binding{{ACL: "a", SubjectKind: SubjectGroup, SubjectID: "g1"}, {ACL: "b", SubjectKind: SubjectToken, SubjectID: "tok", ExpiresAt: testNow.Add(time.Hour)}}, a, b)
	if got := ask(e, "invoke", "tool", "Bash"); got != Denied {
		t.Errorf("= %v, want denied (the token binding is live)", got)
	}
}

func TestCompile_MissingACLMatchesNothingAndIsReported(t *testing.T) {
	e := compile(t, Subject{TenantID: "t", ID: "u"}, []Binding{{ACL: "retired", SubjectKind: SubjectTenant}})
	if ask(e, "invoke", "tool", "Bash") != NoMatch {
		t.Error("a missing list must match nothing")
	}
	if !reflect.DeepEqual(e.Missing(), []string{"retired"}) {
		t.Errorf("missing = %v", e.Missing())
	}
}

func TestCompile_RefusesAListThatNoLongerValidates(t *testing.T) {
	bad := ACL{Name: "x", Rules: []Rule{{Effect: Allow, Actions: []string{"read"}, Resources: []string{"tool:*"}}}}
	_, err := Compile(Input{Subject: Subject{TenantID: "t"}, Bindings: []Binding{{ACL: "x", SubjectKind: SubjectTenant}}, ACLs: map[string]ACLVersion{"x": {ACL: bad}}, Now: testNow})
	if err == nil || !strings.Contains(err.Error(), "not a verb") {
		t.Errorf("want a validation error, got %v", err)
	}
}

// --- the three properties of RFC EY §3.4 ---

func randomRule(r *rand.Rand) Rule {
	types := []struct{ typ, verb string }{{"agent", "run"}, {"tool", "invoke"}, {"volume", "read"}, {"memory", "read"}}
	names := map[string][]string{
		"agent":  {"agent:*", "agent:chat/*", "agent:chat/**", "agent:admin/**", "agent:ccq-*", "agent:x"},
		"tool":   {"tool:*", "tool:Bash", "tool:Ba*", "tool:Read"},
		"volume": {"volume:*", "volume:shared-*", "volume:scratch"},
		"memory": {"memory:*", "memory:tenant", "memory:user", "memory:user:a*"},
	}
	tv := types[r.Intn(len(types))]
	eff := Allow
	if r.Intn(3) == 0 {
		eff = Deny
	}
	rule := Rule{Effect: eff, Actions: []string{tv.verb}, Resources: []string{names[tv.typ][r.Intn(len(names[tv.typ]))]}}
	if r.Intn(4) == 0 {
		rule.When = &Condition{Transport: []string{[]string{"http", "grpc"}[r.Intn(2)]}}
	}
	return rule
}

func randomWorld(r *rand.Rand) ([]ACL, []Binding) {
	var acls []ACL
	var bindings []Binding
	n := 1 + r.Intn(4)
	for i := 0; i < n; i++ {
		a := ACL{Name: "acl" + string(rune('a'+i))}
		m := 1 + r.Intn(6)
		for j := 0; j < m; j++ {
			a.Rules = append(a.Rules, randomRule(r))
		}
		acls = append(acls, a)
		kinds := []Binding{
			{ACL: a.Name, SubjectKind: SubjectTenant},
			{ACL: a.Name, SubjectKind: SubjectUser, SubjectID: "alice"},
			{ACL: a.Name, SubjectKind: SubjectGroup, SubjectID: "g1"},
			{ACL: a.Name, SubjectKind: SubjectGroup, SubjectID: "g2"},
		}
		bindings = append(bindings, kinds[r.Intn(len(kinds))])
	}
	return acls, bindings
}

var propertyRequests = []Request{
	{Action: "run", Resource: Resource{Type: "agent", Name: "chat/support"}},
	{Action: "run", Resource: Resource{Type: "agent", Name: "admin/x/y"}},
	{Action: "run", Resource: Resource{Type: "agent", Name: "ccq-score"}},
	{Action: "invoke", Resource: Resource{Type: "tool", Name: "Bash"}},
	{Action: "invoke", Resource: Resource{Type: "tool", Name: "Read"}},
	{Action: "read", Resource: Resource{Type: "volume", Name: "shared-ro"}},
	{Action: "read", Resource: Resource{Type: "memory", Name: "tenant"}},
	{Action: "read", Resource: Resource{Type: "memory", Name: "user:alice"}},
}

func outcomes(e *Effective, transport string) []Outcome {
	out := make([]Outcome, len(propertyRequests))
	for i, req := range propertyRequests {
		req.Subject = e.Subject
		req.Context = Context{Transport: transport, Now: testNow}
		out[i] = e.Decide(req).Outcome
	}
	return out
}

func TestProperty_OrderIndependence(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	subj := Subject{TenantID: "t", ID: "alice", Groups: []string{"g1"}}
	for iter := 0; iter < 300; iter++ {
		acls, bindings := randomWorld(r)
		base := compile(t, subj, bindings, acls...)
		want := outcomes(base, "http")
		// Shuffle rules inside each list and the bindings; same answer.
		shuffled := make([]ACL, len(acls))
		for i, a := range acls {
			rules := append([]Rule(nil), a.Rules...)
			r.Shuffle(len(rules), func(x, y int) { rules[x], rules[y] = rules[y], rules[x] })
			shuffled[i] = ACL{Name: a.Name, Rules: rules}
		}
		sb := append([]Binding(nil), bindings...)
		r.Shuffle(len(sb), func(x, y int) { sb[x], sb[y] = sb[y], sb[x] })
		r.Shuffle(len(shuffled), func(x, y int) { shuffled[x], shuffled[y] = shuffled[y], shuffled[x] })
		got := outcomes(compile(t, subj, sb, shuffled...), "http")
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: order changed the outcome: %v vs %v\nacls=%+v", iter, got, want, acls)
		}
	}
}

func TestProperty_MonotoneNarrowing(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	subj := Subject{TenantID: "t", ID: "alice", Groups: []string{"g1"}}
	for iter := 0; iter < 300; iter++ {
		acls, bindings := randomWorld(r)
		before := outcomes(compile(t, subj, bindings, acls...), "http")
		// Add a deny rule to a random list, or remove an allow rule.
		i := r.Intn(len(acls))
		mod := append([]ACL(nil), acls...)
		rules := append([]Rule(nil), acls[i].Rules...)
		if r.Intn(2) == 0 {
			rule := randomRule(r)
			rule.Effect = Deny
			rules = append(rules, rule)
		} else {
			var kept []Rule
			removed := false
			for _, rl := range rules {
				if !removed && rl.Effect == Allow {
					removed = true
					continue
				}
				kept = append(kept, rl)
			}
			if len(kept) == 0 {
				continue // a list needs a rule; skip this draw
			}
			rules = kept
		}
		mod[i] = ACL{Name: acls[i].Name, Rules: rules}
		after := outcomes(compile(t, subj, bindings, mod...), "http")
		for k := range before {
			if after[k] == Allowed && before[k] != Allowed {
				t.Fatalf("iteration %d: narrowing widened request %d: %v → %v", iter, k, before[k], after[k])
			}
		}
	}
}

func TestProperty_TenantClosure(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	for iter := 0; iter < 200; iter++ {
		acls, bindings := randomWorld(r)
		// The same subject id and groups in tenant A and tenant B, with the
		// rules bound in A only: B's answers are all NoMatch.
		a := compile(t, Subject{TenantID: "A", ID: "alice", Groups: []string{"g1", "g2"}}, bindings, acls...)
		b := compile(t, Subject{TenantID: "B", ID: "alice", Groups: []string{"g1", "g2"}}, nil)
		for _, o := range outcomes(b, "http") {
			if o != NoMatch {
				t.Fatalf("iteration %d: tenant B got %v with no bindings", iter, o)
			}
		}
		// And A's set refuses B's resources at the floor.
		for _, req := range propertyRequests {
			req.Subject = a.Subject
			req.Resource.TenantID = "B"
			if d := a.Decide(req); !d.TenantFloor || d.Outcome != Denied {
				t.Fatalf("iteration %d: cross-tenant request was not floored: %+v", iter, d)
			}
		}
	}
}

func TestQuick_AgreesWithDecide(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	subj := Subject{TenantID: "t", ID: "alice", Groups: []string{"g1"}}
	for iter := 0; iter < 300; iter++ {
		acls, bindings := randomWorld(r)
		e := compile(t, subj, bindings, acls...)
		for _, req := range propertyRequests {
			req.Subject = subj
			// Quick is for condition-free questions: compare against a
			// Decide with an empty context, which is what Quick builds.
			want := e.Decide(req).Outcome
			if got := e.Quick(req.Resource.Type, req.Action, req.Resource.Name); got != want {
				t.Fatalf("iteration %d: Quick(%s %s:%s) = %v, Decide = %v", iter, req.Action, req.Resource.Type, req.Resource.Name, got, want)
			}
		}
	}
}

func TestDecideAll_Semantics(t *testing.T) {
	a := mustACL(t, `{"name":"a","rules":[
		{"effect":"allow","actions":["invoke"],"resources":["tool:Read"]},
		{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]}`)
	e := compile(t, Subject{TenantID: "t", ID: "u"}, []Binding{{ACL: "a", SubjectKind: SubjectTenant}}, a)
	reqs := []Request{
		{Subject: e.Subject, Action: "invoke", Resource: Resource{Type: "tool", Name: "Read"}},
		{Subject: e.Subject, Action: "invoke", Resource: Resource{Type: "tool", Name: "Bash"}},
		{Subject: e.Subject, Action: "invoke", Resource: Resource{Type: "tool", Name: "Read"}},
	}
	all := e.DecideAll(reqs, ExecuteAll)
	if all[0].Outcome != Allowed || all[1].Outcome != Denied || all[2].Outcome != Allowed {
		t.Errorf("execute_all: %+v", all)
	}
	stop := e.DecideAll(reqs, DenyOnFirstDeny)
	if stop[1].Outcome != Denied || stop[2].Outcome != NoMatch || len(stop[2].Reasons) != 0 {
		t.Errorf("deny_on_first_deny did not stop: %+v", stop)
	}
	first := e.DecideAll(reqs, PermitOnFirstAllow)
	if first[0].Outcome != Allowed || first[1].Outcome != NoMatch {
		t.Errorf("permit_on_first_permit did not stop: %+v", first)
	}
}

func TestFilter_IsTheSearchForm(t *testing.T) {
	a := mustACL(t, `{"name":"a","rules":[
		{"effect":"allow","actions":["run"],"resources":["agent:chat/*"]},
		{"effect":"deny","actions":["run"],"resources":["agent:chat/internal"]}]}`)
	e := compile(t, Subject{TenantID: "t", ID: "u"}, []Binding{{ACL: "a", SubjectKind: SubjectTenant}}, a)
	cands := []Resource{{Type: "agent", Name: "chat/support"}, {Type: "agent", Name: "chat/internal"}, {Type: "agent", Name: "ops/x"}}
	got := e.Filter("run", cands, Context{}, nil)
	if len(got) != 1 || got[0].Name != "chat/support" {
		t.Errorf("enforce-style filter = %+v", got)
	}
	got = e.Filter("run", cands, Context{}, func(Resource) bool { return true })
	if len(got) != 2 || got[1].Name != "ops/x" {
		t.Errorf("compat-style filter (legacy allows) = %+v", got)
	}
}

func TestExplain_TracesEveryRuleOfTheType(t *testing.T) {
	a := mustACL(t, `{"name":"a","rules":[
		{"effect":"allow","actions":["invoke"],"resources":["tool:*"],"when":{"transport":["grpc"]}},
		{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]},
		{"effect":"allow","actions":["read"],"resources":["volume:*"]},
		{"effect":"deny","actions":["read"],"resources":["agent:*"]}]}`)
	e := compile(t, Subject{TenantID: "t", ID: "u"}, []Binding{{ACL: "a", SubjectKind: SubjectTenant}, {ACL: "gone", SubjectKind: SubjectTenant}}, a)
	ex := e.Explain(Request{Subject: e.Subject, Action: "invoke", Resource: Resource{Type: "tool", Name: "Read"}, Context: Context{Transport: "http", Now: testNow}})
	if ex.Decision.Outcome != NoMatch {
		t.Errorf("decision = %v", ex.Decision.Outcome)
	}
	if len(ex.Traces) != 2 {
		t.Fatalf("traces = %+v (want the two tool rules only)", ex.Traces)
	}
	byIndex := map[int]Trace{}
	for _, tr := range ex.Traces {
		byIndex[tr.RuleIndex] = tr
	}
	if byIndex[0].Matched || byIndex[0].Why != "condition does not hold" {
		t.Errorf("rule 0: %+v", byIndex[0])
	}
	if byIndex[1].Matched || byIndex[1].Why != "no selector matches the resource" {
		t.Errorf("rule 1: %+v", byIndex[1])
	}
	if !reflect.DeepEqual(ex.Missing, []string{"gone"}) || len(ex.Applied) != 2 {
		t.Errorf("missing=%v applied=%d", ex.Missing, len(ex.Applied))
	}
}

func TestResolveAndDiverges_PerMode(t *testing.T) {
	deny := Decision{Outcome: Denied}
	allow := Decision{Outcome: Allowed}
	none := Decision{Outcome: NoMatch}
	cases := []struct {
		mode   Mode
		d      Decision
		legacy bool
		want   bool
	}{
		{ModeCompat, deny, true, false}, {ModeCompat, allow, false, false}, {ModeCompat, none, true, true}, {ModeCompat, none, false, false},
		{ModeShadow, deny, true, true}, {ModeShadow, allow, false, false},
		{ModeEnforce, deny, true, false}, {ModeEnforce, allow, false, true}, {ModeEnforce, none, true, false},
	}
	for _, c := range cases {
		if got := Resolve(c.mode, c.d, c.legacy); got != c.want {
			t.Errorf("Resolve(%s, %v, legacy=%v) = %v, want %v", c.mode, c.d.Outcome, c.legacy, got, c.want)
		}
	}
	if !Diverges(deny, true) || !Diverges(allow, false) || Diverges(none, true) || Diverges(none, false) || Diverges(deny, false) {
		t.Error("Diverges")
	}
	for in, want := range map[string]Mode{"": ModeCompat, "compat": ModeCompat, "SHADOW": ModeShadow, " enforce ": ModeEnforce} {
		if m, err := ParseMode(in); err != nil || m != want {
			t.Errorf("ParseMode(%q) = %v, %v", in, m, err)
		}
	}
	if _, err := ParseMode("strict"); err == nil {
		t.Error("ParseMode(strict) should fail")
	}
}

func BenchmarkQuick_ToolInvoke(b *testing.B) {
	a, _ := ParseACL([]byte(supportACL))
	noWindow := ACL{Name: "plain", Rules: []Rule{
		{Effect: Allow, Actions: []string{"invoke"}, Resources: []string{"tool:Read"}},
		{Effect: Allow, Actions: []string{"invoke"}, Resources: []string{"tool:Write"}},
		{Effect: Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}},
	}}
	_ = a
	e, _ := Compile(Input{Subject: Subject{TenantID: "t", ID: "u"}, Bindings: []Binding{{ACL: "plain", SubjectKind: SubjectTenant}}, ACLs: map[string]ACLVersion{"plain": {ACL: noWindow}}, Now: testNow})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if e.Quick("tool", "invoke", "Bash") != Denied {
			b.Fatal("wrong")
		}
	}
}

func BenchmarkDecide_GlobsAndConditions(b *testing.B) {
	a, _ := ParseACL([]byte(supportACL))
	e, _ := Compile(Input{Subject: Subject{TenantID: "t", ID: "u"}, Bindings: []Binding{{ACL: a.Name, SubjectKind: SubjectTenant}}, ACLs: map[string]ACLVersion{a.Name: {ACL: a}}, Now: testNow})
	req := Request{Subject: e.Subject, Action: "invoke", Resource: Resource{Type: "tool", Name: "Read"}, Context: Context{Transport: "http", Now: testNow}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if e.Decide(req).Outcome != Allowed {
			b.Fatal("wrong")
		}
	}
}

func TestDenyPatterns_UnconditionalDeniesOnly(t *testing.T) {
	a := mustACL(t, `{"name":"a","rules":[
		{"effect":"deny","actions":["invoke"],"resources":["skill:support/**","skill:seo","tool:Bash"]},
		{"effect":"deny","actions":["invoke"],"resources":["skill:marketing/*"],"when":{"transport":["grpc"]}},
		{"effect":"deny","actions":["read"],"resources":["skill:*"]},
		{"effect":"allow","actions":["invoke"],"resources":["skill:*"]}]}`)
	e := compile(t, Subject{TenantID: "t", ID: "u"}, []Binding{{ACL: "a", SubjectKind: SubjectTenant}}, a)
	if got := e.DenyPatterns("skill", "invoke"); !reflect.DeepEqual(got, []string{"support/**", "seo"}) {
		t.Errorf("DenyPatterns(skill, invoke) = %v", got)
	}
	if got := e.DenyPatterns("tool", "invoke"); !reflect.DeepEqual(got, []string{"Bash"}) {
		t.Errorf("DenyPatterns(tool, invoke) = %v", got)
	}
	if got := e.DenyPatterns("volume", "read"); got != nil {
		t.Errorf("DenyPatterns(volume) = %v", got)
	}
}
