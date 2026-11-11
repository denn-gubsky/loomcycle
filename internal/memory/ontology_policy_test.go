package memory

import (
	"reflect"
	"strings"
	"testing"
)

// filePlan is one scheme: records (10y, acl compliance) > risk (acl risk) > outsourcing
// (5y, acl vendors), and a sibling top concept with nothing.
func filePlan() []OntologyScheme {
	mk := func(path []string, body string) OntologyConcept {
		c := ParseConceptBody("file-plan", path[len(path)-1], path, body)
		c.ChunkID = "id:" + c.Tag
		return c
	}
	return []OntologyScheme{{Name: "file-plan", Concepts: []OntologyConcept{
		mk([]string{"Records"}, "@retention 10y\n@acl compliance"),
		mk([]string{"Records", "Risk"}, "@acl risk"),
		mk([]string{"Records", "Risk", "Outsourcing"}, "@retention 5y\n@acl vendors"),
		mk([]string{"Notes"}, ""),
	}}}
}

func TestEffectiveConceptPolicies_RetentionIsNearestAndGroupsAccumulate(t *testing.T) {
	got, issues := EffectiveConceptPolicies(filePlan(), nil)
	if len(issues) != 0 {
		t.Errorf("issues = %v", issues)
	}
	risk := got["id:file-plan/records/risk"]
	if risk.Retention != "10y" || risk.RetentionFrom != "file-plan/records" || !risk.InheritedRetention("file-plan/records/risk") {
		t.Errorf("risk retention = %+v, want 10y inherited from records", risk)
	}
	if want := []string{"compliance", "risk"}; !reflect.DeepEqual(risk.Groups, want) {
		t.Errorf("risk groups = %v, want %v", risk.Groups, want)
	}
	out := got["id:file-plan/records/risk/outsourcing"]
	if out.Retention != "5y" || out.InheritedRetention("file-plan/records/risk/outsourcing") {
		t.Errorf("outsourcing retention = %+v, want its own 5y", out)
	}
	if want := []string{"compliance", "risk", "vendors"}; !reflect.DeepEqual(out.Groups, want) {
		t.Errorf("outsourcing groups = %v, want %v", out.Groups, want)
	}
	if n := got["id:file-plan/notes"]; n.Retention != "" || len(n.Groups) != 0 {
		t.Errorf("notes = %+v, want no policy", n)
	}
}

func TestEffectiveConceptPolicies_UnknownGroupAndMalformedRetentionContributeNothing(t *testing.T) {
	schemes := []OntologyScheme{{Name: "fp", Concepts: []OntologyConcept{
		ParseConceptBody("fp", "Top", []string{"Top"}, "@retention 7y\n@acl ghosts"),
		ParseConceptBody("fp", "Leaf", []string{"Top", "Leaf"}, "@retention forever\n@acl\n"),
	}}}
	schemes[0].Concepts[0].ChunkID, schemes[0].Concepts[1].ChunkID = "top", "leaf"
	got, issues := EffectiveConceptPolicies(schemes, func(g string) string {
		if g == "ghosts" {
			return "is not a group of this tenant"
		}
		return ""
	})
	if leaf := got["leaf"]; leaf.Retention != "7y" || len(leaf.Groups) != 0 {
		t.Errorf("leaf = %+v, want 7y inherited and no groups", leaf)
	}
	all := strings.Join(issues, "\n")
	for _, want := range []string{`fp/top: @acl "ghosts" is not a group`, "fp/top/leaf: @retention", "fp/top/leaf: @acl needs a group name"} {
		if !strings.Contains(all, want) {
			t.Errorf("issues lack %q:\n%s", want, all)
		}
	}
	if strings.Count(all, "ghosts") != 1 {
		t.Errorf("an unknown group is reported once, on the concept that names it:\n%s", all)
	}
}

func TestCombinePolicies_LongestRetentionAndEveryGroup(t *testing.T) {
	ret, from, groups := CombinePolicies([]GoverningConcept{
		{Tag: "b", Policy: ConceptPolicy{Retention: "18mo", Groups: []string{"legal"}}},
		{Tag: "a", Policy: ConceptPolicy{Retention: "1y", Groups: []string{"compliance", "legal"}}},
		{Tag: "c", Policy: ConceptPolicy{Retention: "90d"}},
	})
	if ret != "18mo" || from != "b" {
		t.Errorf("retention = %q from %q, want 18mo from b", ret, from)
	}
	if want := []string{"compliance", "legal"}; !reflect.DeepEqual(groups, want) {
		t.Errorf("groups = %v, want %v", groups, want)
	}
	if r, f, g := CombinePolicies(nil); r != "" || f != "" || g != nil {
		t.Errorf("no concepts = %q %q %v, want nothing", r, f, g)
	}
}

func TestParseConceptBody_ReportsARepeatedACL(t *testing.T) {
	c := ParseConceptBody("fp", "X", []string{"X"}, "@acl a\n@acl b")
	if c.ACL != "b" || !strings.Contains(strings.Join(c.Issues, " "), "@acl given more than once") {
		t.Errorf("acl = %q issues = %v", c.ACL, c.Issues)
	}
}
