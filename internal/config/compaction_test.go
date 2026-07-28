package config

import (
	"reflect"
	"testing"
)

func ptrB(b bool) *bool     { return &b }
func ptrI2(i int) *int      { return &i }
func ptrS(s string) *string { return &s }

// MergeCompaction overlays `over` onto `base` per field — and is the building
// block for the spawn precedence blend (per-spawn > parent-inherited > child def).
func TestMergeCompaction_PerFieldAndPrecedence(t *testing.T) {
	childDef := &Compaction{KeepLastN: ptrI2(8), TargetPercentage: ptrI2(20), KeepFirst: ptrB(false)}
	parent := &Compaction{Enabled: ptrB(true), KeepLastN: ptrI2(4)} // parent sets enabled + keep_last_n
	override := &Compaction{TargetPercentage: ptrI2(30)}            // per-spawn changes target

	// "child def as fallback": parent-set wins, child fills the gaps the parent
	// left unset, the per-spawn override wins over both.
	eff := MergeCompaction(MergeCompaction(childDef, parent), override)
	if eff.Enabled == nil || !*eff.Enabled {
		t.Errorf("enabled: parent-set should win, got %v", eff.Enabled)
	}
	if eff.KeepLastN == nil || *eff.KeepLastN != 4 {
		t.Errorf("keep_last_n: parent-set (4) should win over child (8), got %v", eff.KeepLastN)
	}
	if eff.KeepFirst == nil || *eff.KeepFirst != false {
		t.Errorf("keep_first: parent unset → child def (false) fills the gap, got %v", eff.KeepFirst)
	}
	if eff.TargetPercentage == nil || *eff.TargetPercentage != 30 {
		t.Errorf("target_percentage: per-spawn override (30) should win, got %v", eff.TargetPercentage)
	}
}

func TestMergeCompaction_NilInputs(t *testing.T) {
	if MergeCompaction(nil, nil) != nil {
		t.Error("merge(nil,nil) should be nil")
	}
	out := MergeCompaction(nil, &Compaction{KeepLastN: ptrI2(3)})
	if out == nil || out.KeepLastN == nil || *out.KeepLastN != 3 {
		t.Errorf("merge(nil, x) should be x: %+v", out)
	}
}

func TestCompaction_Validate(t *testing.T) {
	bad := []*Compaction{
		{TargetPercentage: ptrI2(5)},  // < 10
		{TargetPercentage: ptrI2(60)}, // > 50
		{AutoCompactAtPct: ptrI2(40)}, // < 50
		{AutoCompactAtPct: ptrI2(99)}, // > 95
		{KeepLastN: ptrI2(-1)},        // < 0
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d (%+v): expected a validation error", i, c)
		}
	}
	ok := &Compaction{Enabled: ptrB(true), TargetPercentage: ptrI2(10), KeepLastN: ptrI2(4), AutoCompactAtPct: ptrI2(80), Model: ptrS("haiku")}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid compaction rejected: %v", err)
	}
	if (*Compaction)(nil).Validate() != nil {
		t.Error("nil compaction should validate")
	}
}

// nonZeroCompaction sets EVERY field to a non-zero value via reflection, so a
// newly added field is populated without anyone remembering to add it here. That
// is the whole point: a hand-written literal is exactly what goes stale.
func nonZeroCompaction(t *testing.T) *Compaction {
	t.Helper()
	c := &Compaction{}
	v := reflect.ValueOf(c).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() != reflect.Ptr {
			t.Fatalf("Compaction.%s is not a pointer; this test's non-zero strategy assumes the all-pointer shape that makes unset distinguishable from zero",
				v.Type().Field(i).Name)
		}
		elem := reflect.New(f.Type().Elem())
		switch elem.Elem().Kind() {
		case reflect.Bool:
			elem.Elem().SetBool(true)
		case reflect.Int:
			// In-range for every int field (target_percentage 10..50,
			// autocompact_at_pct 50..95) so Validate() passes too.
			elem.Elem().SetInt(50)
		case reflect.String:
			elem.Elem().SetString("x")
		default:
			t.Fatalf("Compaction.%s has unhandled kind %s — extend this helper", v.Type().Field(i).Name, elem.Elem().Kind())
		}
		f.Set(elem)
	}
	return c
}

// TestCompaction_CloneAndMergeCoverEveryField guards the sub-struct drift the
// AgentDef-level coverage test cannot see. mergeAgentDef and
// lookup.SubstrateAgentDef both carry *Compaction WHOLESALE, so a field added to
// the struct but forgotten in Clone or MergeCompaction is silently dropped on
// every fork and every spawn-inherited override — with no compile error and no
// warning, which is the failure mode that cost twelve AgentDef fields once.
//
// Reflective on purpose: it fails for a field nobody remembered to add.
func TestCompaction_CloneAndMergeCoverEveryField(t *testing.T) {
	full := nonZeroCompaction(t)
	ft := reflect.TypeOf(Compaction{})

	t.Run("Clone", func(t *testing.T) {
		got := reflect.ValueOf(full.Clone()).Elem()
		want := reflect.ValueOf(full).Elem()
		for i := 0; i < ft.NumField(); i++ {
			name := ft.Field(i).Name
			g, w := got.Field(i), want.Field(i)
			if g.IsNil() {
				t.Errorf("Clone() dropped Compaction.%s — a fork of an agent setting it loses the setting", name)
				continue
			}
			if !reflect.DeepEqual(g.Elem().Interface(), w.Elem().Interface()) {
				t.Errorf("Clone() Compaction.%s = %v, want %v", name, g.Elem(), w.Elem())
			}
			// Deep copy, not aliased: mutating the clone must not reach the source.
			if g.Pointer() == w.Pointer() {
				t.Errorf("Clone() aliased Compaction.%s instead of copying it", name)
			}
		}
	})

	t.Run("MergeCompaction", func(t *testing.T) {
		got := reflect.ValueOf(MergeCompaction(&Compaction{}, full)).Elem()
		want := reflect.ValueOf(full).Elem()
		for i := 0; i < ft.NumField(); i++ {
			name := ft.Field(i).Name
			g := got.Field(i)
			if g.IsNil() {
				t.Errorf("MergeCompaction() never overlays Compaction.%s — an override of it is SILENTLY DISCARDED", name)
				continue
			}
			if !reflect.DeepEqual(g.Elem().Interface(), want.Field(i).Elem().Interface()) {
				t.Errorf("MergeCompaction() Compaction.%s = %v, want %v", name, g.Elem(), want.Field(i).Elem())
			}
		}
	})

	t.Run("IsZero", func(t *testing.T) {
		// Each field ALONE must defeat IsZero. A field missing from IsZero collapses
		// a def that sets only that field to nil, so it hashes as a no-compaction
		// def and a create changing only it deduplicates away.
		fv := reflect.ValueOf(full).Elem()
		for i := 0; i < ft.NumField(); i++ {
			only := &Compaction{}
			reflect.ValueOf(only).Elem().Field(i).Set(fv.Field(i))
			if only.IsZero() {
				t.Errorf("IsZero() ignores Compaction.%s — a def setting only that field collapses to nil", ft.Field(i).Name)
			}
		}
		if !(&Compaction{}).IsZero() || !(*Compaction)(nil).IsZero() {
			t.Error("an all-unset / nil Compaction must be zero, or every pre-feature agent re-hashes")
		}
	})

	t.Run("Validate accepts the all-set form", func(t *testing.T) {
		if err := full.Validate(); err != nil {
			t.Errorf("the all-fields-set Compaction failed Validate: %v", err)
		}
	})
}
