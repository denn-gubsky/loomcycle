package authz

import (
	"reflect"
	"testing"
)

func TestResourceForTool_MapsEveryRegistryPrefix(t *testing.T) {
	cases := map[string]Resource{
		"Bash":                         {Type: "tool", Name: "Bash"},
		"mcp__jobs__getAgentContext":   {Type: "mcp", Name: "jobs/getAgentContext"},
		"mcp__jobs__with__sep":         {Type: "mcp", Name: "jobs/with__sep"},
		"a2a__peer__skill":             {Type: "a2a", Name: "peer/skill"},
		"client:browser_click":         {Type: "client", Name: "browser_click"},
		"localapi__petstore__listPets": {Type: "localapi", Name: "petstore/listPets"},
		"mcp__broken":                  {Type: "mcp", Name: "broken"},
	}
	for name, want := range cases {
		if got := ResourceForTool(name); !reflect.DeepEqual(got, want) {
			t.Errorf("ResourceForTool(%q) = %+v, want %+v", name, got, want)
		}
	}
	// Every mapped type exists in the catalog with `invoke`.
	for _, r := range cases {
		if !ValidVerb(r.Type, "invoke") {
			t.Errorf("%s has no invoke verb", r.Type)
		}
	}
	if ActionForVolumeMode("ro") != "read" || ActionForVolumeMode("rw") != "write" || ActionForVolumeMode("") != "write" {
		t.Error("ActionForVolumeMode")
	}
}

func TestResourceForTool_RuleMatchesTheMappedName(t *testing.T) {
	a := ACL{Name: "x", Rules: []Rule{
		{Effect: Deny, Actions: []string{"invoke"}, Resources: []string{"mcp:jobs/patch*"}},
		{Effect: Allow, Actions: []string{"invoke"}, Resources: []string{"mcp:jobs/*", "a2a:*", "tool:*"}},
	}}
	e, err := Compile(Input{Subject: Subject{TenantID: "t", ID: "u"}, Bindings: []Binding{{ACL: "x", SubjectKind: SubjectTenant}}, ACLs: map[string]ACLVersion{"x": {ACL: a}}, Now: testNow})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]Outcome{
		"mcp__jobs__getAgentContext":  Allowed,
		"mcp__jobs__patchApplication": Denied,
		"a2a__peer__skill":            Allowed,
		"Bash":                        Allowed,
		"mcp__other__x":               NoMatch,
	} {
		r := ResourceForTool(name)
		if got := e.Quick(r.Type, "invoke", r.Name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}
