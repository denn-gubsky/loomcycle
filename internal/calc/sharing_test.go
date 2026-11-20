package calc

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// doubling builds a list whose expanded size doubles at each of depth
// levels while costing O(depth) steps: every level holds two references
// to the level below.
func doubling(depth int, tail string) string {
	var b strings.Builder
	b.WriteString("l0 = [1, 1]\n")
	for i := 1; i <= depth; i++ {
		b.WriteString("l" + strconv.Itoa(i) + " = [l" + strconv.Itoa(i-1) + ", l" + strconv.Itoa(i-1) + "]\n")
	}
	b.WriteString(tail)
	return b.String()
}

// TestSharing_DoublingListsAreRefusedAtConstruction: a list's size counts
// every nested element, shared references expanded, so no later walk
// (exactness, shape, rendering, the Assigned map) can be exponential.
func TestSharing_DoublingListsAreRefusedAtConstruction(t *testing.T) {
	for name, expr := range map[string]string{
		"literal result":     doubling(60, "l60"),
		"assigned only":      doubling(60, "0"),
		"literal shallow":    doubling(24, "l24"),
		"assigned shallow":   doubling(24, "0"),
		"reduce":             "reduce(1:60, (acc, i) -> [acc, acc], [1])",
		"map":                "reduce(1:60, (acc, i) -> map([1, 2], j -> acc), 1)",
		"zip":                "reduce(1:60, (acc, i) -> zip([acc, acc], [acc, acc]), 1)",
		"index gather":       "reduce(1:60, (acc, i) -> [acc][[1, 1]], 1)",
		"user function":      "d(v) = [v, v]; d(d(d(d(d(d(d(d(d(d(d(d(d(d(d(d(d(d(d(d(d(d(d(d(1))))))))))))))))))))))))",
		"shared then joined": "a = 1:200000; [a, a, a, a, a, a]",
	} {
		start := time.Now()
		_, err := Eval(context.Background(), Request{Expr: expr})
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Errorf("%s: took %v", name, d)
		}
		if err == nil || err.Code != string(codeListTooLong) {
			t.Errorf("%s: err = %v, want list_too_long", name, err)
		}
	}
	// Under the bound, sharing is fine.
	r := mustEval(t, doubling(10, "len(l10)"))
	if r.Text != "2" {
		t.Errorf("len(l10) = %q", r.Text)
	}
}
