package main

import (
	"fmt"
	"testing"
)

// The contaminated run this guard exists for, and the healthy run it must not
// block. Both are real numbers from the same corpus on the same day: the first
// scored 0.0216 and looked like a result about consolidation, the second is what
// consolidation actually does.
func TestDiversionGuard_CatchesTheContaminatedRunAndPassesTheHealthyOne(t *testing.T) {
	keys := func(facts, chunks int) []string {
		var out []string
		for i := 0; i < facts; i++ {
			out = append(out, fmt.Sprintf("memory/fact/f-%d", i))
		}
		for i := 0; i < chunks; i++ {
			// doc.chunk rows are chunk BODIES, not recallable facts — counting them
			// as facts is what let the partition look populated when it was not.
			out = append(out, fmt.Sprintf("doc.chunk:%032x", i))
		}
		return out
	}
	for _, tc := range []struct {
		name       string
		written    int
		facts      int
		chunks     int
		wantRefuse bool
	}{
		{"the contaminated run: placement diverted them to tenant scope", 76, 7, 9, true},
		{"the healthy run: 59 written, 58 reachable", 59, 58, 55, false},
		{"the healthy run, second conversation", 41, 41, 38, false},
		{"a few retired is fine, not a diversion", 40, 36, 0, false},
		{"total diversion", 40, 0, 12, true},
		{"the turns arm writes no facts — guard must stay silent", 0, 0, 419, false},
		{"exactly half is a shortfall we tolerate", 40, 20, 0, false},
		{"just under half refuses", 40, 19, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reachable, refuse := factsDiverted(tc.written, keys(tc.facts, tc.chunks))
			if reachable != tc.facts {
				t.Errorf("counted %d reachable, want %d — doc.chunk rows must not count as facts", reachable, tc.facts)
			}
			if refuse != tc.wantRefuse {
				t.Errorf("refuse = %v, want %v (written=%d reachable=%d)", refuse, tc.wantRefuse, tc.written, reachable)
			}
		})
	}
}
