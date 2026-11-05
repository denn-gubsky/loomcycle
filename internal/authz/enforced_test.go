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
		{Rule{Effect: Deny, Actions: []string{"browse"}, Resources: []string{"document:user"}}, "browse document is not evaluated by any surface"},
		{Rule{Effect: Deny, Actions: []string{"read"}, Resources: []string{"run:own"}, When: &Condition{Time: &TimeWindow{Days: []string{"mon"}}}}, ""},
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
