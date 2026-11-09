package authz

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Every catalog pair is classified — asked somewhere, waiting for a site, or
// retired — and every Condition field is evaluated somewhere. A new verb, type
// or field fails here until it is classified, so it cannot land stored but
// inert (RFC FY Part E). The table names only catalog pairs.
func TestACLCatalog_EveryPairIsEnforcedSomewhere(t *testing.T) {
	seen := map[pairKey]bool{}
	for _, spec := range catalog {
		for _, v := range spec.Verbs {
			k := pairKey{spec.Name, v}
			seen[k] = true
			_, enforced := enforcedPairs[k]
			_, waiting := unenforcedPairs[k]
			retired := retiredPairs[k]
			n := 0
			for _, b := range []bool{enforced, waiting, retired} {
				if b {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%s %s: classified %d times (enforced=%v unenforced=%v retired=%v), want exactly once", v, spec.Name, n, enforced, waiting, retired)
			}
			if enforced && len(enforcedPairs[k]) == 0 {
				t.Errorf("%s %s: enforced at no site", v, spec.Name)
			}
		}
	}
	for _, m := range []map[pairKey]bool{keysOf(enforcedPairs), keysOf(unenforcedPairs), retiredPairs} {
		for k := range m {
			if !seen[k] {
				t.Errorf("the table names %s %s, which is not a catalog pair", k.verb, k.typ)
			}
		}
	}
	fields := map[string]bool{}
	ct := reflect.TypeOf(Condition{})
	for i := 0; i < ct.NumField(); i++ {
		name := conditionJSONName(ct.Field(i))
		fields[name] = true
		_, waiting := unenforcedConditions[name]
		if IsConditionEnforced(name) == waiting {
			t.Errorf("condition field %q: enforced=%v unenforced=%v, want exactly one", name, !waiting, waiting)
		}
	}
	for name := range unenforcedConditions {
		if !fields[name] {
			t.Errorf("the table names condition %q, which Condition does not have", name)
		}
	}
	for name := range enforcedConditions {
		if !fields[name] {
			t.Errorf("the table names condition %q, which Condition does not have", name)
		}
	}
}

func keysOf[V any](m map[pairKey]V) map[pairKey]bool {
	out := make(map[pairKey]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// A pair or condition the table says is asked inside the code names the test
// that drives that code; the test must exist, or the table names a site that
// may not ask it.
func TestACLEnforcement_EveryProofTestExists(t *testing.T) {
	want := map[string]bool{}
	for _, sites := range enforcedPairs {
		for _, s := range sites {
			if name, ok := s.ProofTest(); ok {
				want[name] = true
			}
		}
	}
	for _, s := range enforcedConditions {
		if name, ok := s.ProofTest(); ok {
			want[name] = true
		}
	}
	if len(want) == 0 {
		t.Fatal("no proof tests named: the scan proves nothing")
	}
	root := filepath.Join("..", "..")
	found := map[string]bool{}
	decl := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "web") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range decl.FindAllSubmatch(b, -1) {
			found[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range want {
		if !found[name] {
			t.Errorf("the enforcement table names proof test %s, which does not exist", name)
		}
	}
}

func TestRuleNotEnforceable_NamesThePairOrTheField(t *testing.T) {
	cases := []struct {
		rule Rule
		want string
	}{
		{Rule{Effect: Allow, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}}, ""},
		{Rule{Effect: Deny, Actions: []string{"provision"}, Resources: []string{"volume:data"}}, "provision volume is not evaluated by any surface (a volume is created as def:volume/)"},
		{Rule{Effect: Deny, Actions: []string{"mount"}, Resources: []string{"volume:data"}}, ""},
		{Rule{Effect: Deny, Actions: []string{"use"}, Resources: []string{"search:*"}}, "use search is not evaluated by any surface (removed from the catalog)"},
		{Rule{Effect: Deny, Actions: []string{"read"}, Resources: []string{"run:*"}, When: &Condition{Time: &TimeWindow{Days: []string{"mon"}}}}, ""},
		{Rule{Effect: Deny, Actions: []string{"read"}, Resources: []string{"run:*"}, When: &Condition{Transport: []string{"mcp", "webui"}}}, ""},
		{Rule{Effect: Deny, Actions: []string{"read"}, Resources: []string{"run:*"}, When: &Condition{Transport: []string{"http", "bridge"}}}, `condition "transport" is not evaluated by any surface (no surface stamps the transport "bridge")`},
		{Rule{Effect: Deny, Actions: []string{"read"}, Resources: []string{"run:*"}, When: &Condition{Transport: []string{"bridge:slack"}}}, `condition "transport" is not evaluated by any surface (no surface stamps the transport "bridge:slack")`},
	}
	for _, c := range cases {
		if err := c.rule.Validate(); err != nil {
			t.Fatalf("%v: %v", c.rule, err)
		}
		err := RuleNotEnforceable(c.rule)
		got := ""
		if err != nil {
			var ne *NotEnforceableError
			if !errors.As(err, &ne) {
				t.Errorf("%v: %T is not a *NotEnforceableError", c.rule, err)
			}
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%v: %q, want %q", c.rule, got, c.want)
		}
	}
}

// No built-in baseline grants a verb removed from the catalog.
func TestBaselines_NameNoRetiredVerb(t *testing.T) {
	for name, a := range Baselines() {
		for i, r := range a.Rules {
			for _, sel := range r.Resources {
				typ, _, err := SplitSelector(sel)
				if err != nil {
					continue
				}
				for _, v := range r.Actions {
					if IsRetired(typ, v) {
						t.Errorf("%s rule %d grants the removed %s %s", name, i, v, typ)
					}
				}
			}
		}
	}
}

// Every transport value a condition may name is either stamped by a surface
// or refused at write: the stamped set is the condition's set less bridge.
func TestTransports_EveryStampedValueIsAConditionValue(t *testing.T) {
	for v := range stampedTransports {
		if !contains(Transports, v) {
			t.Errorf("%s is stamped but not a condition value", v)
		}
	}
	for _, v := range Transports {
		if !IsTransportStamped(v) && v != "bridge" {
			t.Errorf("%s is a condition value no surface stamps; refuse it or stamp it", v)
		}
	}
}

// A selector naming some instances on a pair no surface asks per instance
// decides nothing: at the type-level question a deny on some instances is no
// deny, and an allow on some instances admits every one. Every write refuses
// it (RFC GD D2), naming the selector; the same rule on every instance, or on
// some instances of a pair asked per instance, is accepted.
func TestRuleNotEnforceable_RefusesAnInstanceSelectorOnATypeLevelOnlyPair(t *testing.T) {
	refused := []Rule{
		{Effect: Deny, Actions: []string{"read"}, Resources: []string{"run:tenant"}},
		{Effect: Allow, Actions: []string{"read", "cancel"}, Resources: []string{"run:own"}},
		{Effect: Deny, Actions: []string{"read"}, Resources: []string{"agent:x"}},
		{Effect: Deny, Actions: []string{"read"}, Resources: []string{"skill:x-*"}},
		{Effect: Deny, Actions: []string{"read"}, Resources: []string{"evaluation:self"}},
	}
	for _, r := range refused {
		if err := r.Validate(); err != nil {
			t.Fatalf("%v: %v", r, err)
		}
		err := RuleNotEnforceable(r)
		var ne *NotEnforceableError
		if !errors.As(err, &ne) {
			t.Errorf("%v: %v, want a *NotEnforceableError", r, err)
			continue
		}
		if !strings.Contains(err.Error(), r.Resources[0]) {
			t.Errorf("%v: %q does not name the selector", r, err)
		}
	}
	accepted := []Rule{
		{Effect: Deny, Actions: []string{"read", "cancel"}, Resources: []string{"run:*"}},
		{Effect: Deny, Actions: []string{"run"}, Resources: []string{"agent:x"}},
		{Effect: Deny, Actions: []string{"invoke"}, Resources: []string{"skill:x-*"}},
		{Effect: Deny, Actions: []string{"read"}, Resources: []string{"agent:**"}},
		{Effect: Deny, Actions: []string{"publish"}, Resources: []string{"channel:ops"}},
	}
	for _, r := range accepted {
		if err := RuleNotEnforceable(r); err != nil {
			t.Errorf("%v: refused: %v", r, err)
		}
	}
}

// A type-level-only pair is enforced, and only at the transport gates: a code
// site (a tool, a narrowing, a resolver) or the connector asks an instance.
func TestTypeLevelOnlyPairs_AreAskedOnlyAtTransports(t *testing.T) {
	for k := range typeLevelOnlyPairs {
		sites, ok := enforcedPairs[k]
		if !ok {
			t.Errorf("%s %s: type-level-only but not enforced", k.verb, k.typ)
		}
		for _, s := range sites {
			if s != SiteHTTPRoute && s != SiteGRPCRPC && s != SiteMCPTool {
				t.Errorf("%s %s: type-level-only but asked at %s, which asks an instance", k.verb, k.typ, s)
			}
		}
	}
}

// No built-in baseline carries a selector that decides nothing: a baseline is
// the shape operators copy into their own lists.
func TestBaselines_NameNoInstanceOfATypeLevelOnlyPair(t *testing.T) {
	for name, a := range Baselines() {
		for i, r := range a.Rules {
			for _, sel := range r.Resources {
				for _, v := range r.Actions {
					if SelectorDecidesNothing(v, sel) {
						t.Errorf("%s rule %d: %s %s names instances no surface asks", name, i, v, sel)
					}
				}
			}
		}
	}
}

// An evaluation of a named instance of a type-level-only pair is refused, as
// the rule naming it is; the type-level question and an instance of a pair
// asked per instance are answered.
func TestQuestionNotEnforceable_RefusesANamedInstanceOfATypeLevelOnlyPair(t *testing.T) {
	if err := QuestionNotEnforceable("run", "read", "own"); err == nil {
		t.Error("read run:own was answered")
	}
	for _, q := range [][3]string{{"run", "read", ""}, {"run", "read", AnyInstance}, {"agent", "run", "chat"}} {
		if err := QuestionNotEnforceable(q[0], q[1], q[2]); err != nil {
			t.Errorf("%v: %v", q, err)
		}
	}
	if err := QuestionNotEnforceable("volume", "provision", ""); err == nil {
		t.Error("an unenforced pair was answered")
	}
}
