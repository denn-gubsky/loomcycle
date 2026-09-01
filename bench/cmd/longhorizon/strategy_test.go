package main

import (
	"strings"
	"testing"
)

func TestA0_HistoryGrows(t *testing.T) {
	s := NewA0()
	insn := Instruction{Op: OpSet, Key: "counter_00", Value: 3, Text: "SET counter_00 = 3."}
	// Before any step: system + current user = 2 messages.
	if got := len(s.StepMessages(insn)); got != 2 {
		t.Fatalf("initial step messages = %d, want 2", got)
	}
	for i := 0; i < 5; i++ {
		s.Observe(insn, "ok")
	}
	// After 5 steps: system + 5*(user,assistant) + current user = 1 + 10 + 1 = 12.
	if got := len(s.StepMessages(insn)); got != 12 {
		t.Errorf("step messages after 5 obs = %d, want 12 (history grows O(T))", got)
	}
	if s.PendingRecap() != nil {
		t.Error("A0 must never request a recap")
	}
}

func TestA1_WindowEvictsAndRecaps(t *testing.T) {
	s := NewA1(2) // window cap = 2 pairs = 4 messages
	insn := Instruction{Op: OpAdd, Key: "counter_01", Value: 1, Text: "ADD 1 to counter_01."}

	s.Observe(insn, "ok") // 1 pair
	s.Observe(insn, "ok") // 2 pairs (at cap)
	if s.PendingRecap() != nil {
		t.Fatal("recap fired before the window overflowed")
	}
	s.Observe(insn, "ok") // 3rd pair -> evict oldest -> recap pending
	rm := s.PendingRecap()
	if rm == nil {
		t.Fatal("no recap requested after window overflow")
	}
	if len(s.window) != 4 {
		t.Errorf("window not trimmed to 4 messages: got %d", len(s.window))
	}
	// PendingRecap is consumed (one-shot).
	if s.PendingRecap() != nil {
		t.Error("PendingRecap should return nil on the second read")
	}
	// Installing the recap surfaces it in the preamble.
	s.SetRecap("counter_01 = 2")
	msgs := s.StepMessages(insn)
	if !strings.Contains(joinContents(msgs), "counter_01 = 2") {
		t.Error("recap not injected into the step preamble")
	}
	if msgs[0].Role != "system" {
		t.Error("preamble must start with the system task rules")
	}
}

func TestA2_PatchMergeAndNullDeletion(t *testing.T) {
	s := NewA2([]string{"counter_00", "counter_01"})
	if got := s.stateJSON(); got != `{"counter_00":0,"counter_01":0}` {
		t.Fatalf("initial stateJSON = %s", got)
	}
	// A prose-wrapped patch is still parsed; the merge is runtime-owned.
	s.Observe(Instruction{}, "Here is the patch: {\"counter_00\": 7}")
	if s.state["counter_00"] != 7 {
		t.Errorf("patch not applied: %v", s.state)
	}
	// null deletes a key.
	s.Observe(Instruction{}, `{"counter_01": null}`)
	if _, ok := s.state["counter_01"]; ok {
		t.Errorf("null did not delete the key: %v", s.state)
	}
	// An unparseable reply is a no-op (dropped, not corrupting state).
	before := s.stateJSON()
	s.Observe(Instruction{}, "I could not decide.")
	if s.stateJSON() != before {
		t.Errorf("unparseable patch mutated state: %s -> %s", before, s.stateJSON())
	}
	// stateJSON is deterministically ordered (stable prompt/hash).
	if s.stateJSON() != `{"counter_00":7}` {
		t.Errorf("stateJSON after ops = %s", s.stateJSON())
	}
	// The step prompt carries the STATE, not a history.
	msgs := s.StepMessages(Instruction{Text: "ADD 1 to counter_00."})
	if !strings.Contains(joinContents(msgs), `STATE: {"counter_00":7}`) {
		t.Error("A2 step prompt missing the state object")
	}
}

func TestParsePatch(t *testing.T) {
	m, ok := parsePatch(`prefix {"a": 1, "b": null} suffix`)
	if !ok || len(m) != 2 {
		t.Fatalf("parsePatch failed: %v %v", m, ok)
	}
	if n, ok := asInt(m["a"]); !ok || n != 1 {
		t.Errorf("asInt(a) = %d,%v", n, ok)
	}
	if m["b"] != nil {
		t.Errorf("b should be nil, got %v", m["b"])
	}
	if _, ok := parsePatch("no json here"); ok {
		t.Error("parsePatch should fail on non-JSON")
	}
}

func joinContents(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}
