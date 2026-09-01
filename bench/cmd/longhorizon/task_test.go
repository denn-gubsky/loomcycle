package main

import (
	"fmt"
	"reflect"
	"testing"
)

// applyInstructions re-derives the end state from the rendered instruction stream,
// independently of GenerateTask's inline computation — so a mismatch means the
// instructions an agent SEES disagree with the oracle it is graded against.
func applyInstructions(keys []string, insns []Instruction) map[string]int {
	st := map[string]int{}
	for _, k := range keys {
		st[k] = 0
	}
	for _, in := range insns {
		switch in.Op {
		case OpSet, OpCorrection:
			st[in.Key] = in.Value
		case OpAdd:
			st[in.Key] += in.Value
		case OpSub:
			st[in.Key] -= in.Value
		case OpReset:
			st[in.Key] = 0
		case OpNote:
			// no-op
		}
	}
	return st
}

func TestGenerateTask_OracleMatchesRenderedStream(t *testing.T) {
	task := GenerateTask(7, 120, 5, 0.15, true)
	got := applyInstructions(task.Keys, task.Instructions)
	if !reflect.DeepEqual(got, task.FinalState) {
		t.Fatalf("oracle FinalState %v != replayed stream %v", task.FinalState, got)
	}
	if len(task.Instructions) != 120 {
		t.Errorf("horizon = %d, want 120", len(task.Instructions))
	}
	// Queries cover every key + SUM + MAX, and their answers match the oracle.
	if len(task.Queries) != len(task.Keys)+2 {
		t.Fatalf("queries = %d, want keys+2", len(task.Queries))
	}
	for _, q := range task.Queries {
		if q.Kind == "get" {
			if want := fmt.Sprintf("%d", task.FinalState[q.Key]); q.Answer != want {
				t.Errorf("query %s answer %q, want %q", q.Key, q.Answer, want)
			}
		}
	}
}

func TestGenerateTask_Deterministic(t *testing.T) {
	a := GenerateTask(42, 60, 4, 0.1, true)
	b := GenerateTask(42, 60, 4, 0.1, true)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed produced different tasks — not reproducible")
	}
	c := GenerateTask(43, 60, 4, 0.1, true)
	if reflect.DeepEqual(a, c) {
		t.Fatal("different seeds produced identical tasks")
	}
}

func TestGenerateTask_DriftHonoured(t *testing.T) {
	task := GenerateTask(3, 40, 3, 0, true)
	var corr *Instruction
	for i := range task.Instructions {
		if task.Instructions[i].Op == OpCorrection {
			corr = &task.Instructions[i]
		}
	}
	if corr == nil {
		t.Fatal("drift=true produced no CORRECTION")
	}
	// The correction is the LAST word on its key (nothing after it re-touches it
	// unless a later step does, in which case the oracle reflects that later step).
	got := applyInstructions(task.Keys, task.Instructions)
	if got[corr.Key] != task.FinalState[corr.Key] {
		t.Errorf("drift key %s not consistent: replay %d vs oracle %d", corr.Key, got[corr.Key], task.FinalState[corr.Key])
	}
}

func TestQuery_GradeTolerantParsing(t *testing.T) {
	get := Query{Kind: "get", Key: "counter_01", Answer: "42"}
	for _, ans := range []string{"42", " 42 ", "The value is 42.", "counter_01 = 42"} {
		if !get.Grade(ans) {
			t.Errorf("get.Grade(%q) = false, want true", ans)
		}
	}
	if get.Grade("41") {
		t.Error("get.Grade(41) = true, want false")
	}
	mx := Query{Kind: "max", Answer: "counter_03"}
	if !mx.Grade("The highest is counter_03 right now.") {
		t.Error("max.Grade with surrounding text failed")
	}
	if mx.Grade("counter_04") {
		t.Error("max.Grade(counter_04) = true, want false")
	}
}
