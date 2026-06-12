package config

import "testing"

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
