package authz

import (
	"reflect"
	"strings"
	"testing"
)

func TestClosure_TransitiveAndDeduplicated(t *testing.T) {
	g := GroupGraph{
		"all":      {"staff", "contractors"},
		"staff":    {"support", "eng"},
		"support":  {"support-l2"},
		"eng":      nil,
		"orphan":   {"nobody"},
		"isolated": nil,
	}
	got := Closure(g, []string{"support-l2", "eng"})
	want := []string{"all", "eng", "staff", "support", "support-l2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Closure = %v, want %v", got, want)
	}
	if got := Closure(g, nil); len(got) != 0 {
		t.Errorf("no direct groups → %v", got)
	}
	if got := Closure(nil, []string{"x"}); !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("no graph → %v", got)
	}
}

func TestClosure_TerminatesOnACycleAtTheDepthCap(t *testing.T) {
	g := GroupGraph{"a": {"b"}, "b": {"c"}, "c": {"a"}}
	got := Closure(g, []string{"a"})
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("cycle closure = %v", got)
	}
	// A chain deeper than the cap is cut, not followed forever.
	deep := GroupGraph{}
	for i := 0; i < 20; i++ {
		deep["g"+string(rune('a'+i+1))] = []string{"g" + string(rune('a'+i))}
	}
	got = Closure(deep, []string{"ga"})
	if len(got) != MaxGroupNestingDepth+1 {
		t.Errorf("deep chain closure has %d groups, want %d", len(got), MaxGroupNestingDepth+1)
	}
}

func TestClosureVia_NamesTheGroupOneStepCloserToTheSubject(t *testing.T) {
	g := GroupGraph{
		"all":     {"staff", "contractors"},
		"staff":   {"support", "eng"},
		"support": {"support-l2"},
		"eng":     nil,
	}
	got, via := ClosureVia(g, []string{"support-l2", "eng"})
	if want := Closure(g, []string{"support-l2", "eng"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("ClosureVia groups = %v, Closure = %v", got, want)
	}
	want := map[string]string{
		"support-l2": ViaDirect,
		"eng":        ViaDirect,
		"support":    "support-l2",
		// staff is reached through eng and through support at different
		// depths; eng is one hop, so it wins.
		"staff": "eng",
		"all":   "staff",
	}
	if !reflect.DeepEqual(via, want) {
		t.Errorf("via = %v, want %v", via, want)
	}
	// A group that is both direct and reachable through another reads as
	// direct.
	_, via = ClosureVia(g, []string{"support", "staff"})
	if via["staff"] != ViaDirect {
		t.Errorf("a direct group read as via %q", via["staff"])
	}
}

func TestValidateGroupGraph(t *testing.T) {
	if err := ValidateGroupGraph(GroupGraph{"a": {"b"}, "b": {"c"}, "c": nil}); err != nil {
		t.Errorf("acyclic: %v", err)
	}
	for name, g := range map[string]GroupGraph{
		"self":    {"a": {"a"}},
		"cycle":   {"a": {"b"}, "b": {"a"}},
		"badname": {"bad name!": nil},
	} {
		if err := ValidateGroupGraph(g); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
	deep := GroupGraph{}
	for i := 0; i < MaxGroupNestingDepth+2; i++ {
		deep["g"+string(rune('a'+i))] = []string{"g" + string(rune('a'+i+1))}
	}
	if err := ValidateGroupGraph(deep); err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Errorf("deep: %v", err)
	}
}
