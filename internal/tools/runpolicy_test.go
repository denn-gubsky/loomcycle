package tools

import (
	"reflect"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// Every RunPolicy field has a written inheritance rule, and every rule names a
// field. A field added without one fails here — so whether a child keeps it is
// decided on purpose, in Inherit, and not by whatever the spawn path happened
// to restamp.
func TestRunPolicy_EveryFieldHasAnInheritanceRule(t *testing.T) {
	typ := reflect.TypeOf(RunPolicy{})
	fields := map[string]bool{}
	var missing []string
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		fields[name] = true
		if _, ok := inheritRules[name]; !ok {
			missing = append(missing, name)
		}
	}
	var stale []string
	for name := range inheritRules {
		if !fields[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("RunPolicy fields with no inheritance rule: %v — add each to inheritRules and decide it in Inherit", missing)
	}
	if len(stale) > 0 {
		t.Errorf("inheritRules names fields RunPolicy does not have: %v", stale)
	}
}

// Inherit does what inheritRules says for every field, checked generically:
// each field in turn is given a non-zero value on the parent only, then on the
// child's own grant only, and the child's policy is compared with what the rule
// allows. A rule and the code drifting apart fails here.
func TestRunPolicy_InheritFollowsItsRules(t *testing.T) {
	typ := reflect.TypeOf(RunPolicy{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		rule := inheritRules[f.Name]
		t.Run(f.Name, func(t *testing.T) {
			val := nonZero(t, f.Type)

			var parent RunPolicy
			reflect.ValueOf(&parent).Elem().Field(i).Set(val)
			fromParent := reflect.ValueOf(parent.Inherit(ChildGrant{})).Field(i)

			var own RunPolicy
			reflect.ValueOf(&own).Elem().Field(i).Set(val)
			fromOwn := reflect.ValueOf(RunPolicy{}.Inherit(ChildGrant{Own: own})).Field(i)

			zero := reflect.Zero(f.Type).Interface()
			switch rule {
			case ruleOwn:
				if !reflect.DeepEqual(fromParent.Interface(), zero) {
					t.Errorf("own: the parent's %v reached the child as %v", val, fromParent)
				}
				if !reflect.DeepEqual(fromOwn.Interface(), val.Interface()) {
					t.Errorf("own: the child's own %v became %v", val, fromOwn)
				}
			case ruleInherit:
				if !reflect.DeepEqual(fromParent.Interface(), val.Interface()) {
					t.Errorf("inherit: the parent's %v became %v", val, fromParent)
				}
			case ruleMerge:
				if !reflect.DeepEqual(fromParent.Interface(), val.Interface()) {
					t.Errorf("merge: the parent's %v (child unset) became %v", val, fromParent)
				}
				if !reflect.DeepEqual(fromOwn.Interface(), val.Interface()) {
					t.Errorf("merge: the child's own %v (parent unset) became %v", val, fromOwn)
				}
			case ruleClear:
				if !reflect.DeepEqual(fromParent.Interface(), zero) {
					t.Errorf("clear: the parent's spawn input %v reached the child as %v", val, fromParent)
				}
				if !reflect.DeepEqual(fromOwn.Interface(), zero) {
					t.Errorf("clear: a spawn input %v on the child's own grant survived as %v", val, fromOwn)
				}
			case ruleNarrow:
				// Narrowing is field-specific (TestRunPolicy_InheritNarrows);
				// generically, a child with nothing of its own never ends up
				// with more than its parent had.
				if reflect.DeepEqual(val.Interface(), zero) {
					t.Fatal("fixture: nonZero returned zero")
				}
			default:
				t.Fatalf("field %s has rule %q, which this test does not know", f.Name, rule)
			}
		})
	}
}

// The narrowing rules, case by case: a child never holds more than its parent
// and never more than its own definition grants.
func TestRunPolicy_InheritNarrows(t *testing.T) {
	t.Run("FanoutCap", func(t *testing.T) {
		for _, c := range []struct{ parent, own, want int }{
			{8, 2, 2}, // the child's own smaller cap wins (H4)
			{2, 8, 2}, // never wider than the parent's
			{4, 0, 4}, // a child with no cap of its own runs under the parent's
			{0, 3, 3}, // no run cap above: the child's own definition's
			{0, 0, 0},
		} {
			got := RunPolicy{FanoutCap: c.parent}.Inherit(ChildGrant{Own: RunPolicy{FanoutCap: c.own}}).FanoutCap
			if got != c.want {
				t.Errorf("parent %d, own %d: child cap %d, want %d", c.parent, c.own, got, c.want)
			}
		}
	})
	t.Run("EphemeralWritable", func(t *testing.T) {
		writable := RunPolicy{EphemeralWritable: true}
		if !writable.Inherit(ChildGrant{Own: writable}).EphemeralWritable {
			t.Error("a writable child of a writable parent lost write access")
		}
		if (RunPolicy{}).Inherit(ChildGrant{Own: writable}).EphemeralWritable {
			t.Error("a child of a read-only parent got write access back")
		}
		if writable.Inherit(ChildGrant{}).EphemeralWritable {
			t.Error("a child whose own definition is read-only was given write access")
		}
	})
	t.Run("Crypto", func(t *testing.T) {
		got := RunPolicy{Crypto: CryptoPolicyValue{Scopes: []string{"user", "run"}}}.
			Inherit(ChildGrant{Own: RunPolicy{Crypto: CryptoPolicyValue{Scopes: []string{"run", "tenant"}}}}).Crypto.Scopes
		if !reflect.DeepEqual(got, []string{"run"}) {
			t.Errorf("child key scopes %v, want the intersection [run]", got)
		}
	})
	t.Run("Volumes", func(t *testing.T) {
		parent := VolumePolicyValue{Active: true, Bindings: []VolumeBinding{{Name: "a", Root: "/a"}, {Name: "b", Root: "/b", ReadOnly: true}}}
		own := VolumePolicyValue{Active: true, Bindings: []VolumeBinding{{Name: "b", Root: "/elsewhere"}, {Name: "c", Root: "/c"}}}
		got := RunPolicy{Volumes: parent}.Inherit(ChildGrant{Own: RunPolicy{Volumes: own}, DeclaresVolumes: true}).Volumes
		want := VolumePolicyValue{Active: true, Bindings: []VolumeBinding{{Name: "b", Root: "/b", ReadOnly: true}}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("declared child volumes %+v, want %+v", got, want)
		}
		if got := (RunPolicy{Volumes: parent}).Inherit(ChildGrant{Own: RunPolicy{Volumes: own}}).Volumes; !reflect.DeepEqual(got, parent) {
			t.Errorf("a child declaring no volumes got %+v, want its parent's %+v", got, parent)
		}
		if got := (RunPolicy{}).Inherit(ChildGrant{Own: RunPolicy{Volumes: own}, DeclaresVolumes: true}).Volumes; !reflect.DeepEqual(got, own) {
			t.Errorf("under an unconfined parent the child got %+v, want its own %+v", got, own)
		}
	})
	t.Run("Hosts", func(t *testing.T) {
		parent := HostPolicyValue{AllowedHosts: []string{"api.example.com"}, HasList: true}
		got := RunPolicy{Hosts: parent}.Inherit(ChildGrant{HostsNarrowOnly: true}).Hosts
		if !got.NarrowOnly || !reflect.DeepEqual(got.AllowedHosts, parent.AllowedHosts) {
			t.Errorf("a locked child's hosts %+v, want the parent's list made narrow-only", got)
		}
		sticky := RunPolicy{Hosts: got}.Inherit(ChildGrant{}).Hosts
		if !sticky.NarrowOnly {
			t.Error("a narrow-only list became a replacing one for a grandchild")
		}
		if (RunPolicy{}).Inherit(ChildGrant{HostsNarrowOnly: true}).Hosts.NarrowOnly {
			t.Error("narrow-only was set on a child whose parent had no list to narrow")
		}
	})
	t.Run("Compaction", func(t *testing.T) {
		three, five, seven := 3, 5, 70
		parent := RunPolicy{
			Compaction:         &config.Compaction{KeepLastN: &three},
			CompactionOverride: &config.Compaction{AutoCompactAtPct: &seven},
		}
		own := RunPolicy{Compaction: &config.Compaction{KeepLastN: &five, TargetPercentage: &five}}
		got := parent.Inherit(ChildGrant{Own: own}).Compaction
		if got == nil || *got.KeepLastN != 3 || *got.TargetPercentage != 5 || got.AutoCompactAtPct == nil || *got.AutoCompactAtPct != 70 {
			t.Errorf("child compaction %+v, want the parent's keep_last_n, its own target and the spawn's autocompact_at_pct", got)
		}
		locked := parent.Inherit(ChildGrant{Own: own, LocksOverrides: true}).Compaction
		if locked == nil || *locked.KeepLastN != 5 {
			t.Errorf("a locked child took its parent's per-run compaction: %+v", locked)
		}
	})
}

// nonZero builds a non-zero value of typ, so the rule test exercises every
// field without a hand-written fixture per type.
func nonZero(t *testing.T, typ reflect.Type) reflect.Value {
	t.Helper()
	v := reflect.New(typ).Elem()
	fill(t, v)
	if v.IsZero() {
		t.Fatalf("could not build a non-zero %v", typ)
	}
	return v
}

func fill(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64, reflect.Int32:
		v.SetInt(3)
	case reflect.String:
		v.SetString("x")
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(t, s.Index(0))
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fill(t, k)
		e := reflect.New(v.Type().Elem()).Elem()
		fill(t, e)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(t, p.Elem())
		v.Set(p)
	case reflect.Struct:
		// The first exported field that can hold a value is enough: the value
		// only has to be non-zero.
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				fill(t, v.Field(i))
				return
			}
		}
	}
}
