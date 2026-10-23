package extract

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// runsPart is a document part of n runs, each after a line break and
// indentation: text between sibling elements.
func runsPart(n int) []byte {
	var b strings.Builder
	b.WriteString(`<w:document ` + nsW + `><w:body><w:p>`)
	for range n {
		b.WriteString("\n  <w:r><w:t>а</w:t></w:r>")
	}
	b.WriteString("\n</w:p></w:body></w:document>")
	return []byte(b.String())
}

// A part of many small elements separated by whitespace once grew its
// parent's text by a string copy per child: quadratic in the part's size
// (200k runs took 9.5 s). Judged by the bytes the parse allocates, which do
// not depend on the machine or a race build as a wall-clock bound would:
// eight times the runs must allocate well under the sixty-four times a
// quadratic parse does.
func TestParseXML_ManyRunsIsLinear(t *testing.T) {
	allocated := func(n int) uint64 {
		data := runsPart(n)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		root, err := parseXML(context.Background(), data)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(root.all("t")); got != n {
			t.Fatalf("runs = %d, want %d", got, n)
		}
		return after.TotalAlloc - before.TotalAlloc
	}
	small, large := allocated(10_000), allocated(80_000)
	ratio := float64(large) / float64(small)
	t.Logf("8x the runs allocated %.1fx the bytes (%d vs %d)", ratio, large, small)
	if ratio > 20 {
		t.Errorf("8x the runs allocated %.1fx the bytes: the parse is not linear", ratio)
	}
}
