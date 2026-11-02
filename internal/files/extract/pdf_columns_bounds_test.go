package extract

import (
	"testing"
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
// three lines must share two column starts, which line up within 2 pt; and
// a table has three columns or more, since two columns of prose (a
// newsletter, a paper) line up just as a two-column table does.
func TestColumnThresholds_AreTheMeasuredOnes(t *testing.T) {
	if colGapSizes != 1.0 || colGapMedians != 3.0 || colMinLines != 3 || colTol != 2.0 || colMinColumns != 3 {
		t.Fatalf("thresholds changed: gap %g sizes, %g medians, %d lines, %g pt, %d columns", colGapSizes, colGapMedians, colMinLines, colTol, colMinColumns)
	}
}

// Lines whose wide gaps never line up are each tried as a table's start;
// the work is charged to the page's budget, so a page of thousands of them
// stops early instead of taking time quadratic in its lines. Judged by the
// budget, not a wall clock, which a race build on a busy machine would make
// flaky.
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
	budget := &gridBudget{left: maxPDFGridWork}
	_, ok := alignedTables(rows, budget)
	if ok || budget.left >= 0 {
		t.Errorf("ok %v, budget left %d: the search did not stop at the budget for 20000 misaligned lines", ok, budget.left)
	}
}

// alignedRows is n lines of cols cells each, every cell starting at the same
// x on every line: one table as far as alignment goes.
func alignedRows(n, cols int) []pdfLine {
	rows := make([]pdfLine, n)
	for i := range n {
		y := 1e6 - float64(i)*12
		runs := make([]pdfRun, cols)
		for c := range cols {
			x := 40 + float64(c)*40
			runs[c] = pdfRun{x: x, endX: x + 12, y: y, size: 10, text: "яя", upright: true, seq: c}
		}
		rows[i] = pdfLine{y: y, size: 10, runs: runs}
	}
	return rows
}

// However many lines share its columns, a table is at most
// maxPDFTableRows lines; the lines past it start another table.
func TestAlignedTables_ARegionIsAtMostMaxRows(t *testing.T) {
	regions, ok := alignedTables(alignedRows(maxPDFTableRows+500, colMinColumns), &gridBudget{left: maxPDFGridWork})
	if !ok || len(regions) != 2 {
		t.Fatalf("ok %v, regions %d, want 2", ok, len(regions))
	}
	for _, g := range regions {
		if g.to-g.from > maxPDFTableRows {
			t.Errorf("region of %d lines, over %d", g.to-g.from, maxPDFTableRows)
		}
	}
}

// More columns than a table may have is not a table.
func TestAlignedTables_TooManyColumnsIsNotATable(t *testing.T) {
	regions, ok := alignedTables(alignedRows(10, maxPDFTableCols+1), &gridBudget{left: maxPDFGridWork})
	if !ok || len(regions) != 0 {
		t.Errorf("ok %v, regions %d", ok, len(regions))
	}
}

// Writing a table's cells is charged to the page's budget, as finding it
// is: a budget that covers the search but not the cells reads the text as
// text, and says so.
func TestFlowBlocks_ReadingCellsIsChargedToTheBudget(t *testing.T) {
	rows := alignedRows(1000, maxPDFTableCols)
	var runs []pdfRun
	for _, r := range rows {
		runs = append(runs, r.runs...)
	}
	// What the search alone costs.
	search := &gridBudget{left: maxPDFGridWork}
	search.spend(len(runs))
	if _, ok := alignedTables(pdfLines(runs), search); !ok {
		t.Fatal("the search ran out of budget")
	}
	spent := maxPDFGridWork - search.left
	// Enough for the search, not for writing 1000 rows of 64 cells.
	blocks, ok := flowBlocks(runs, &gridBudget{left: spent + 1000})
	if ok || len(blocks) != 1 || blocks[0].table != nil {
		t.Errorf("ok %v, %d blocks (table %v): the cells were written past the budget", ok, len(blocks), len(blocks) > 0 && blocks[0].table != nil)
	}
	// With the whole budget the table is read, within the caps.
	blocks, ok = flowBlocks(runs, &gridBudget{left: maxPDFGridWork})
	if !ok || len(blocks) != 1 || len(blocks[0].table) != 1000 || len(blocks[0].table[0]) != maxPDFTableCols {
		t.Errorf("ok %v, blocks %d", ok, len(blocks))
	}
}
