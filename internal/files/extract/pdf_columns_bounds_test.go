package extract

import (
	"testing"
	"time"
)

// The thresholds that tell a column gap from a word space, and the numbers
// they come from (Chrome's print of the passport table, 10 pt Arial, with
// and without borders; see testdata/README.md):
//
//   - gaps between cells: 13.8 to 139 pt, so 1.38 to 13.9 font sizes;
//   - word spaces: Chrome draws a space glyph, so the gap between runs
//     at a word break is about 0; a justified line adds 2.6 pt;
//   - a hanging list indent: the number's end to its text, 6 to 8 pt.
//
// A column gap is at least one font size (and three median word spaces);
// three lines must share two column starts, which line up within 2 pt.
func TestColumnThresholds_AreTheMeasuredOnes(t *testing.T) {
	if colGapSizes != 1.0 || colGapMedians != 3.0 || colMinLines != 3 || colTol != 2.0 {
		t.Fatalf("thresholds changed: gap %g sizes, %g medians, %d lines, %g pt", colGapSizes, colGapMedians, colMinLines, colTol)
	}
}

// Lines whose wide gaps never line up are each tried as a table's start;
// the work is charged to the page's budget, so a page of thousands of them
// stops early instead of taking time quadratic in its lines.
func TestAlignedTables_ManyMisalignedLinesStayInBudget(t *testing.T) {
	var rows []pdfLine
	for i := range 20000 {
		y := 10000 - float64(i)*12
		off := float64(i%997) * 0.37
		rows = append(rows, pdfLine{y: y, size: 10, runs: []pdfRun{
			{x: 40, endX: 70, y: y, size: 10, text: "аа", upright: true},
			{x: 100 + off, endX: 130 + off, y: y, size: 10, text: "бб", upright: true},
			{x: 200 + 2*off, endX: 230 + 2*off, y: y, size: 10, text: "вв", upright: true},
		}})
	}
	start := time.Now()
	_, ok := alignedTables(rows, &gridBudget{left: maxPDFGridWork})
	if ok {
		t.Error("the budget held for 20000 misaligned lines")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s", d)
	}
}
