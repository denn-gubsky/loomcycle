package config

import (
	"reflect"
	"testing"
)

// MergeContext overlays `over` onto `base` per field — the building block for the
// spawn precedence blend (per-spawn > parent-inherited > child def), same as
// MergeCompaction.
func TestMergeContext_PerFieldAndPrecedence(t *testing.T) {
	childDef := &Context{KeepLastN: ptrI2(8), RecapMaxChars: ptrI2(256), Reasoning: ptrS("drop")}
	parent := &Context{Mode: ptrS(ContextModeRecap), KeepLastN: ptrI2(4)} // parent sets mode + keep_last_n
	override := &Context{RecapMaxChars: ptrI2(512)}                       // per-spawn changes the budget

	eff := MergeContext(MergeContext(childDef, parent), override)
	if eff.Mode == nil || *eff.Mode != ContextModeRecap {
		t.Errorf("mode: parent-set should win, got %v", eff.Mode)
	}
	if eff.KeepLastN == nil || *eff.KeepLastN != 4 {
		t.Errorf("keep_last_n: parent-set (4) should win over child (8), got %v", eff.KeepLastN)
	}
	if eff.Reasoning == nil || *eff.Reasoning != "drop" {
		t.Errorf("reasoning: parent unset → child def (drop) fills the gap, got %v", eff.Reasoning)
	}
	if eff.RecapMaxChars == nil || *eff.RecapMaxChars != 512 {
		t.Errorf("recap_max_chars: per-spawn override (512) should win, got %v", eff.RecapMaxChars)
	}
}

func TestMergeContext_NilInputs(t *testing.T) {
	if MergeContext(nil, nil) != nil {
		t.Error("merge(nil,nil) should be nil")
	}
	out := MergeContext(nil, &Context{KeepLastN: ptrI2(3)})
	if out == nil || out.KeepLastN == nil || *out.KeepLastN != 3 {
		t.Errorf("merge(nil, x) should be x: %+v", out)
	}
}

func TestContext_Validate(t *testing.T) {
	bad := []*Context{
		{Mode: ptrS("stateful")},       // reserved, not yet available
		{Mode: ptrS("bogus")},          // unknown
		{Reasoning: ptrS("summarize")}, // unknown R-policy
		{KeepLastN: ptrI2(-1)},         // < 0
		{RecapMaxChars: ptrI2(-1)},     // < 0
		{AutoRecapAtPct: ptrI2(40)},    // < 50
		{AutoRecapAtPct: ptrI2(99)},    // > 95
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d (%+v): expected a validation error", i, c)
		}
	}
	for _, m := range []string{ContextModeAppend, ContextModeRecap} {
		if err := (&Context{Mode: ptrS(m)}).Validate(); err != nil {
			t.Errorf("mode %q should be valid: %v", m, err)
		}
	}
	for _, r := range []string{"recap", "drop", "keep"} {
		if err := (&Context{Reasoning: ptrS(r)}).Validate(); err != nil {
			t.Errorf("reasoning %q should be valid: %v", r, err)
		}
	}
	if (*Context)(nil).Validate() != nil {
		t.Error("nil context should validate")
	}
}

// nonZeroContext sets EVERY field to a non-zero, VALIDATION-PASSING value via
// reflection, so a newly added field is populated without anyone remembering to
// update this test. String enum fields get a valid member by name.
func nonZeroContext(t *testing.T) *Context {
	t.Helper()
	c := &Context{}
	v := reflect.ValueOf(c).Elem()
	vt := v.Type()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		name := vt.Field(i).Name
		if f.Kind() != reflect.Ptr {
			t.Fatalf("Context.%s is not a pointer; this test assumes the all-pointer shape that makes unset distinguishable from zero", name)
		}
		elem := reflect.New(f.Type().Elem())
		switch elem.Elem().Kind() {
		case reflect.Int:
			// In-range for autorecap_at_pct (50..95); fine for the others.
			elem.Elem().SetInt(50)
		case reflect.String:
			switch name {
			case "Mode":
				elem.Elem().SetString(ContextModeRecap)
			case "Reasoning":
				elem.Elem().SetString("recap")
			default:
				elem.Elem().SetString("x")
			}
		default:
			t.Fatalf("Context.%s has unhandled kind %s — extend this helper", name, elem.Elem().Kind())
		}
		f.Set(elem)
	}
	return c
}

// TestContext_CloneAndMergeCoverEveryField guards sub-struct drift: mergeAgentDef
// and lookup.SubstrateAgentDef both carry *Context WHOLESALE, so a field added to
// the struct but forgotten in Clone / MergeContext / IsZero is silently dropped on
// every fork and spawn-inherited override — with no compile error. Reflective on
// purpose: it fails for a field nobody remembered to add.
func TestContext_CloneAndMergeCoverEveryField(t *testing.T) {
	full := nonZeroContext(t)
	ft := reflect.TypeOf(Context{})

	t.Run("Clone", func(t *testing.T) {
		got := reflect.ValueOf(full.Clone()).Elem()
		want := reflect.ValueOf(full).Elem()
		for i := 0; i < ft.NumField(); i++ {
			name := ft.Field(i).Name
			g, w := got.Field(i), want.Field(i)
			if g.IsNil() {
				t.Errorf("Clone() dropped Context.%s — a fork setting it loses the setting", name)
				continue
			}
			if !reflect.DeepEqual(g.Elem().Interface(), w.Elem().Interface()) {
				t.Errorf("Clone() Context.%s = %v, want %v", name, g.Elem(), w.Elem())
			}
			if g.Pointer() == w.Pointer() {
				t.Errorf("Clone() aliased Context.%s instead of copying it", name)
			}
		}
	})

	t.Run("MergeContext", func(t *testing.T) {
		got := reflect.ValueOf(MergeContext(&Context{}, full)).Elem()
		want := reflect.ValueOf(full).Elem()
		for i := 0; i < ft.NumField(); i++ {
			name := ft.Field(i).Name
			g := got.Field(i)
			if g.IsNil() {
				t.Errorf("MergeContext() never overlays Context.%s — an override of it is SILENTLY DISCARDED", name)
				continue
			}
			if !reflect.DeepEqual(g.Elem().Interface(), want.Field(i).Elem().Interface()) {
				t.Errorf("MergeContext() Context.%s = %v, want %v", name, g.Elem(), want.Field(i).Elem())
			}
		}
	})

	t.Run("IsZero", func(t *testing.T) {
		fv := reflect.ValueOf(full).Elem()
		for i := 0; i < ft.NumField(); i++ {
			only := &Context{}
			reflect.ValueOf(only).Elem().Field(i).Set(fv.Field(i))
			if only.IsZero() {
				t.Errorf("IsZero() ignores Context.%s — a def setting only that field collapses to nil", ft.Field(i).Name)
			}
		}
		if !(&Context{}).IsZero() || !(*Context)(nil).IsZero() {
			t.Error("an all-unset / nil Context must be zero, or every pre-feature agent re-hashes")
		}
	})

	t.Run("Validate accepts the all-set form", func(t *testing.T) {
		if err := full.Validate(); err != nil {
			t.Errorf("the all-fields-set Context failed Validate: %v", err)
		}
	})
}
