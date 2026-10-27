package authz

import (
	"reflect"
	"strings"
	"testing"
)

// AllowPatterns mirrors DenyPatterns; a conditional rule is in neither and
// HasConditionalRules says one exists.
func TestAllowPatterns_ListUnconditionalAllowsAndFlagConditionalOnes(t *testing.T) {
	a := mustACL(t, `{"name":"l","rules":[
		{"effect":"allow","actions":["run"],"resources":["agent:chat/*","agent:support/**"]},
		{"effect":"deny","actions":["run"],"resources":["agent:admin/**"]},
		{"effect":"allow","actions":["run"],"resources":["agent:night/*"],"when":{"transport":["grpc"]}},
		{"effect":"allow","actions":["read"],"resources":["agent:docs/*"]}]}`)
	e := compile(t, Subject{TenantID: "acme", ID: "alice"}, []Binding{{ACL: "l", SubjectKind: SubjectTenant}}, a)
	if got := e.AllowPatterns("agent", "run"); !reflect.DeepEqual(got, []string{"chat/*", "support/**"}) {
		t.Errorf("AllowPatterns = %v", got)
	}
	if got := e.DenyPatterns("agent", "run"); !reflect.DeepEqual(got, []string{"admin/**"}) {
		t.Errorf("DenyPatterns = %v", got)
	}
	if !e.HasConditionalRules("agent", "run") || e.HasConditionalRules("agent", "read") || e.HasConditionalRules("tool", "invoke") {
		t.Error("HasConditionalRules misreports")
	}
	if e.AllowPatterns("tool", "invoke") != nil {
		t.Error("a type with no rules has allow patterns")
	}
}

// Compile copies the reporting fields of its input and records each applied
// binding with the version its list resolved to.
func TestCompile_RecordsAppliedListsBaselineAndVia(t *testing.T) {
	a := mustACL(t, `{"name":"l","rules":[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]}`)
	in := Input{
		Subject:  Subject{TenantID: "acme", ID: "alice", Groups: []string{"support"}},
		Bindings: []Binding{{ACL: "l", SubjectKind: SubjectGroup, SubjectID: "support"}, {ACL: "absent", SubjectKind: SubjectTenant}, {ACL: "l", SubjectKind: SubjectUser, SubjectID: "bob"}},
		ACLs:     map[string]ACLVersion{"l": {ACL: a, Version: 4}},
		Now:      testNow,
		Baseline: BaselineMember,
		Via:      map[string]string{"support": ViaDirect},
	}
	e, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []AppliedList{{Name: "l", Version: 4, Binding: in.Bindings[0]}}
	if got := e.Lists(); !reflect.DeepEqual(got, want) {
		t.Errorf("Lists = %+v, want %+v", got, want)
	}
	if e.Baseline() != BaselineMember {
		t.Errorf("Baseline = %q", e.Baseline())
	}
	in.Via["support"] = "mutated"
	if e.GroupVia()["support"] != ViaDirect {
		t.Error("the set shares its input's via map")
	}
	if (&Effective{}).GroupVia() != nil {
		t.Error("an unrecorded via is not nil")
	}
}

func TestValidateResourceName_RefusesWhatNoSelectorCouldName(t *testing.T) {
	for _, c := range []struct{ typ, name string }{
		{"agent", "chat/support"}, {"agent", AnyInstance}, {"memory", "tenant"}, {"document", "tenant:/kb/x"}, {"run", "own"}, {"principal", "user:alice"},
	} {
		if err := ValidateResourceName(c.typ, c.name); err != nil {
			t.Errorf("%s:%s refused: %v", c.typ, c.name, err)
		}
	}
	for _, c := range []struct{ typ, name string }{
		{"agent", ""}, {"agent", "a/../b"}, {"agent", "a b"}, {"nosuch", "x"}, {"run", "everyone"}, {"memory", "planet"}, {"agent", strings.Repeat("a", MaxSelectorBytes)},
	} {
		if err := ValidateResourceName(c.typ, c.name); err == nil {
			t.Errorf("%s:%q accepted", c.typ, c.name)
		}
	}
}
