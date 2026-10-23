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
				// Never more than the parent held: not with an empty own
				// grant, and not with an own grant wider than the parent's.
				// What "more" means is the field's (narrowAtMost); the cases
				// for each narrowing are TestRunPolicy_InheritNarrows.
				nc, ok := narrowAtMost[f.Name]
				if !ok {
					t.Fatalf("narrow field %s has no comparator in narrowAtMost", f.Name)
				}
				if got := parent.Inherit(ChildGrant{}); !nc.atMost(got, parent) {
					t.Errorf("narrow: with an empty own grant the child holds more than the parent's %v: %v", val, fromParent)
				}
				wide := ChildGrant{Own: nc.wideOwn, DeclaresVolumes: true}
				if got := parent.Inherit(wide); !nc.atMost(got, parent) {
					t.Errorf("narrow: with a wide own grant the child holds more than the parent's %v: %v",
						val, reflect.ValueOf(got).Field(i))
				}
				// A parent holding none of it passes none on, whatever the
				// child's own grant — except where zero means "not confined".
				if !nc.zeroParentUnbounded {
					if got := (RunPolicy{}).Inherit(wide); !nc.atMost(got, RunPolicy{}) {
						t.Errorf("narrow: a parent holding none gave a child with a wide own grant %v",
							reflect.ValueOf(got).Field(i))
					}
				}
			default:
				t.Fatalf("field %s has rule %q, which this test does not know", f.Name, rule)
			}
		})
	}
}

// narrowAtMost says, per narrow field, what "no more than the parent" means,
// with an own grant wider than any fixture parent and a pair the comparator
// must reject (so it is not vacuous). Every narrow field needs an entry.
var narrowAtMost = map[string]struct {
	atMost        func(child, parent RunPolicy) bool
	wideOwn       RunPolicy
	widerChild    RunPolicy
	widerOfParent RunPolicy
	// zeroParentUnbounded: the field's zero on a parent means \"nothing to
	// narrow against\", not \"none\" — the child takes its own (an unconfined
	// parent's child is resolved as if top-level, RFC AH §4).
	zeroParentUnbounded bool
}{
	"Volumes": {
		atMost: func(c, p RunPolicy) bool {
			if !c.Volumes.Active {
				return true // bound to no volume: nothing at all
			}
			if !p.Volumes.Active {
				return false
			}
			for _, cb := range c.Volumes.Bindings {
				ok := false
				for _, pb := range p.Volumes.Bindings {
					if cb.Name == pb.Name && cb.Root == pb.Root && (cb.ReadOnly || !pb.ReadOnly) {
						ok = true
					}
				}
				if !ok {
					return false
				}
			}
			return true
		},
		wideOwn:             RunPolicy{Volumes: VolumePolicyValue{Active: true, Bindings: []VolumeBinding{{Name: "x"}, {Name: "root", Root: "/"}}}},
		widerChild:          RunPolicy{Volumes: VolumePolicyValue{Active: true, Bindings: []VolumeBinding{{Name: "root", Root: "/"}}}},
		widerOfParent:       RunPolicy{Volumes: VolumePolicyValue{Active: true}},
		zeroParentUnbounded: true,
	},
	"EphemeralWritable": {
		atMost:        func(c, p RunPolicy) bool { return !c.EphemeralWritable || p.EphemeralWritable },
		wideOwn:       RunPolicy{EphemeralWritable: true},
		widerChild:    RunPolicy{EphemeralWritable: true},
		widerOfParent: RunPolicy{},
	},
	"Crypto": {
		atMost: func(c, p RunPolicy) bool {
			for _, s := range c.Crypto.Scopes {
				if !containsScope(p.Crypto.Scopes, s) {
					return false
				}
			}
			return true
		},
		wideOwn:       RunPolicy{Crypto: CryptoPolicyValue{Scopes: []string{"agent", "user", "tenant", "run"}}},
		widerChild:    RunPolicy{Crypto: CryptoPolicyValue{Scopes: []string{"tenant"}}},
		widerOfParent: RunPolicy{Crypto: CryptoPolicyValue{Scopes: []string{"user"}}},
	},
	"ShellCreds": {
		// A narrowed parent's child is narrowed to names the parent allows;
		// an unnarrowed parent bounds nothing (RFC EW: absent = every
		// credential the operator lists).
		atMost: func(c, p RunPolicy) bool {
			if !p.ShellCreds.Set {
				return true
			}
			if !c.ShellCreds.Set {
				return false
			}
			for _, n := range c.ShellCreds.Names {
				if !p.ShellCreds.Allows(n) {
					return false
				}
			}
			return true
		},
		wideOwn:             RunPolicy{ShellCreds: ShellCredsPolicyValue{Set: true, Names: []string{"A", "B", "C", "D"}}},
		widerChild:          RunPolicy{ShellCreds: ShellCredsPolicyValue{Set: true, Names: []string{"B"}}},
		widerOfParent:       RunPolicy{ShellCreds: ShellCredsPolicyValue{Set: true, Names: []string{"A"}}},
		zeroParentUnbounded: true,
	},
	"GitRemote": {
		// Zero is "no network", not "unbounded": a parent that may not fetch
		// passes no fetch on, so this comparator does not set
		// zeroParentUnbounded (unlike ShellCreds, whose zero is every
		// credential).
		atMost: func(c, p RunPolicy) bool {
			return (!c.GitRemote.Fetch || p.GitRemote.Fetch) && (!c.GitRemote.Push || p.GitRemote.Push)
		},
		wideOwn:       RunPolicy{GitRemote: GitRemotePolicyValue{Fetch: true, Push: true}},
		widerChild:    RunPolicy{GitRemote: GitRemotePolicyValue{Fetch: true, Push: true}},
		widerOfParent: RunPolicy{GitRemote: GitRemotePolicyValue{Fetch: true}},
	},
	"FanoutCap": {
		// A capped parent's child is capped no wider (0 would hand the
		// width to a by-name lookup); an uncapped parent bounds nothing.
		atMost:        func(c, p RunPolicy) bool { return p.FanoutCap <= 0 || (c.FanoutCap > 0 && c.FanoutCap <= p.FanoutCap) },
		wideOwn:       RunPolicy{FanoutCap: 1 << 20},
		widerChild:    RunPolicy{FanoutCap: 8},
		widerOfParent: RunPolicy{FanoutCap: 2},
	},
}

// Each comparator rejects a wider child, so the generic narrow check above can
// fail.
func TestRunPolicy_NarrowComparatorsRejectAWiderChild(t *testing.T) {
	for name, nc := range narrowAtMost {
		if nc.atMost(nc.widerChild, nc.widerOfParent) {
			t.Errorf("%s: the comparator accepts a child wider than its parent", name)
		}
		if inheritRules[name] != ruleNarrow {
			t.Errorf("%s has a comparator but rule %q", name, inheritRules[name])
		}
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

// RFC EW: a child's shell credentials are the names both its parent run and
// its own definition allow; an unset side narrows nothing; an empty list is
// none, and stays none through a spawn.
func TestRunPolicy_ShellCredsNarrowsForAChild(t *testing.T) {
	set := func(names ...string) ShellCredsPolicyValue {
		return ShellCredsPolicyValue{Set: true, Names: append([]string{}, names...)}
	}
	for _, c := range []struct {
		name        string
		parent, own ShellCredsPolicyValue
		want        ShellCredsPolicyValue
	}{
		{"neither narrows", ShellCredsPolicyValue{}, ShellCredsPolicyValue{}, ShellCredsPolicyValue{}},
		{"only the child narrows", ShellCredsPolicyValue{}, set("A"), set("A")},
		{"only the parent narrows", set("A"), ShellCredsPolicyValue{}, set("A")},
		{"both: the intersection", set("A", "B"), set("B", "C"), set("B")},
		{"a parent that took none passes none on", set(), set("A"), set()},
		{"a child that takes none takes none", set("A"), set(), set()},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := RunPolicy{ShellCreds: c.parent}.Inherit(ChildGrant{Own: RunPolicy{ShellCreds: c.own}}).ShellCreds
			if got.Set != c.want.Set || len(got.Names) != len(c.want.Names) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			for i := range got.Names {
				if got.Names[i] != c.want.Names[i] {
					t.Fatalf("got %+v, want %+v", got, c.want)
				}
			}
		})
	}
}

// Filter keeps the operator's names the narrowing allows, in the operator's
// order, and never adds one the operator did not list.
func TestShellCredsPolicy_FilterNeverWidens(t *testing.T) {
	operator := []string{"A", "B"}
	for _, c := range []struct {
		name string
		v    ShellCredsPolicyValue
		want []string
	}{
		{"unset keeps the operator's list", ShellCredsPolicyValue{}, []string{"A", "B"}},
		{"a name keeps only it", ShellCredsPolicyValue{Set: true, Names: []string{"B"}}, []string{"B"}},
		{"empty keeps none", ShellCredsPolicyValue{Set: true}, nil},
		{"a name the operator did not list adds nothing", ShellCredsPolicyValue{Set: true, Names: []string{"C"}}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.v.Filter(operator); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// RFC FB: a child's Git tool may perform a network operation only when both
// its parent run and its own definition allow it; a parent with no network
// passes none on, whatever the child's definition lists.
func TestRunPolicy_GitRemoteNarrowsForAChild(t *testing.T) {
	none := GitRemotePolicyValue{}
	fetch := GitRemotePolicyValue{Fetch: true}
	push := GitRemotePolicyValue{Fetch: true, Push: true}
	for _, c := range []struct {
		name        string
		parent, own GitRemotePolicyValue
		want        GitRemotePolicyValue
	}{
		{"neither allows", none, none, none},
		{"a parent with no network passes none on", none, push, none},
		{"a child that lists none takes none", push, none, none},
		{"a fetch-only parent withholds push", fetch, push, fetch},
		{"a fetch-only child does not gain push", push, fetch, fetch},
		{"both allow push", push, push, push},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := RunPolicy{GitRemote: c.parent}.Inherit(ChildGrant{Own: RunPolicy{GitRemote: c.own}}).GitRemote
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// GitRemoteFor reads a definition's list: push counts only with fetch (a
// push-only list, refused at validation, grants nothing to push), unknown
// values grant nothing, and Ops reports the result as a non-nil list.
func TestGitRemoteFor_PushRequiresFetchAndOpsIsNeverNil(t *testing.T) {
	for _, c := range []struct {
		name    string
		in      []string
		want    GitRemotePolicyValue
		wantOps []string
	}{
		{"absent", nil, GitRemotePolicyValue{}, []string{}},
		{"empty", []string{}, GitRemotePolicyValue{}, []string{}},
		{"fetch", []string{"fetch"}, GitRemotePolicyValue{Fetch: true}, []string{"fetch"}},
		{"fetch and push", []string{"push", "fetch"}, GitRemotePolicyValue{Fetch: true, Push: true}, []string{"fetch", "push"}},
		{"push alone", []string{"push"}, GitRemotePolicyValue{}, []string{}},
		{"unknown", []string{"clone"}, GitRemotePolicyValue{}, []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := GitRemoteFor(c.in)
			if got != c.want {
				t.Fatalf("GitRemoteFor(%v) = %+v, want %+v", c.in, got, c.want)
			}
			ops := got.Ops()
			if ops == nil || !reflect.DeepEqual(ops, c.wantOps) {
				t.Errorf("Ops() = %#v, want %#v", ops, c.wantOps)
			}
			if got.Allows("fetch") != c.want.Fetch || got.Allows("push") != c.want.Push || got.Allows("clone") {
				t.Errorf("Allows disagrees with %+v", got)
			}
		})
	}
}
