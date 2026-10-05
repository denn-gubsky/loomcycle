package builtin

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// applyTeamOverlay names each Definition field it merges, so a new field with
// no case is silently dropped by every fork (Layout, then Hooks). This walks the
// struct itself: each field set alone in an overlay must reach the result.
func TestApplyTeamOverlay_CarriesEveryDefinitionField(t *testing.T) {
	typ := reflect.TypeOf(teamgraph.Definition{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		var ov teamgraph.Definition
		v := reflect.ValueOf(&ov).Elem().Field(i)
		switch v.Kind() {
		case reflect.String:
			v.SetString("x")
		case reflect.Int:
			v.SetInt(1)
		case reflect.Slice:
			v.Set(reflect.MakeSlice(f.Type, 1, 1))
		case reflect.Map:
			v.Set(reflect.MakeMap(f.Type))
		case reflect.Pointer:
			v.Set(reflect.New(f.Type.Elem()))
		default:
			t.Fatalf("field %s: kind %s has no test value; extend this test", f.Name, v.Kind())
		}
		var base teamgraph.Definition
		applyTeamOverlay(&base, ov)
		if !reflect.DeepEqual(reflect.ValueOf(base).Field(i).Interface(), v.Interface()) {
			t.Errorf("a fork drops Definition.%s: applyTeamOverlay has no case for it", f.Name)
		}
	}
}

// `local` merges per kind: sending the agents replaces them all, sending the
// block without them keeps the parent's, and `{}` declares none.
func TestApplyTeamOverlay_LocalAgentsReplaceWholesalePerKind(t *testing.T) {
	parse := func(s string) teamgraph.Definition {
		t.Helper()
		d, err := teamgraph.Parse([]byte(s))
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		return d
	}
	const parent = `{"entry":"s","local":{"agents":{"a":{"tier":"low"},"b":{"tier":"low"}}}}`
	for name, tc := range map[string]struct {
		overlay string
		want    []string
	}{
		"no local block keeps the parent's":    {`{"entry":"s2"}`, []string{"a", "b"}},
		"a block without agents keeps them":    {`{"local":{}}`, []string{"a", "b"}},
		"null agents keeps them":               {`{"local":{"agents":null}}`, []string{"a", "b"}},
		"agents replaces the whole list":       {`{"local":{"agents":{"c":{"tier":"low"}}}}`, []string{"c"}},
		"an empty agents object declares none": {`{"local":{"agents":{}}}`, nil},
	} {
		base := parse(parent)
		applyTeamOverlay(&base, parse(tc.overlay))
		if got := base.LocalAgentNames(); !reflect.DeepEqual(append([]string(nil), got...), tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
			t.Errorf("%s: local agents = %v, want %v", name, got, tc.want)
		}
	}
}

// Skills are their own kind: sending them replaces the team's skills and
// leaves its agents alone, and the other way round.
func TestApplyTeamOverlay_LocalSkillsReplaceIndependentlyOfAgents(t *testing.T) {
	parse := func(s string) teamgraph.Definition {
		t.Helper()
		d, err := teamgraph.Parse([]byte(s))
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		return d
	}
	const parent = `{"entry":"s","local":{"agents":{"a":{"tier":"low"}},"skills":{"x":{"body":"X"},"y":{"body":"Y"}}}}`
	for name, tc := range map[string]struct {
		overlay              string
		wantAgents, wantSkls []string
	}{
		"no local block keeps both":            {`{"entry":"s2"}`, []string{"a"}, []string{"x", "y"}},
		"agents only keeps the skills":         {`{"local":{"agents":{"b":{"tier":"low"}}}}`, []string{"b"}, []string{"x", "y"}},
		"skills only keeps the agents":         {`{"local":{"skills":{"z":{"body":"Z"}}}}`, []string{"a"}, []string{"z"}},
		"null skills keeps them":               {`{"local":{"skills":null}}`, []string{"a"}, []string{"x", "y"}},
		"an empty skills object declares none": {`{"local":{"skills":{}}}`, []string{"a"}, nil},
	} {
		base := parse(parent)
		applyTeamOverlay(&base, parse(tc.overlay))
		same := func(got, want []string) bool {
			return reflect.DeepEqual(got, want) || (len(got) == 0 && len(want) == 0)
		}
		if got := base.LocalAgentNames(); !same(got, tc.wantAgents) {
			t.Errorf("%s: local agents = %v, want %v", name, got, tc.wantAgents)
		}
		if got := base.LocalSkillNames(); !same(got, tc.wantSkls) {
			t.Errorf("%s: local skills = %v, want %v", name, got, tc.wantSkls)
		}
	}
}
