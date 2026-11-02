package extract

import (
	"math"
	"sort"
)

// Tables read from text aligned in columns (RFC FU P2).
//
// A table drawn without borders is still laid out in columns: on each of
// its lines the cells stand apart by a gap far wider than a word space, and
// the cells of one column start at the same x line after line. Where several
// lines share such column starts, the lines are read as the rows of a table,
// each run going to the column it starts in. A cell that wraps has no border
// to say where it ends, so each line is a row of its own: a wrapped cell's
// second line is a row whose other cells are empty. Text that a page's
// borders already put in a table (P1) is not looked at again.

const (
	// colGapSizes is how wide, in font sizes, a gap on a line must be to
	// part two cells. Chrome's borderless passport table has gaps of 13.8 to
	// 139 pt between its cells at 10 pt; a justified line's widest word gap
	// is 2.6 pt, and a hanging list indent about 8 pt.
	colGapSizes = 1.0
	// colGapMedians is how many times wider than the line's median word
	// space it must also be: a justified line's spaces, however wide, are
	// all about the same. Word spaces are the gaps narrower than a column
	// gap and wider than a tenth of the font size; the median is not taken
	// over every gap, since a line written one run per cell has no gaps but
	// column gaps, and one written a glyph per run has mostly none at all.
	colGapMedians = 3.0
	// colMinLines is how many lines of a table must each start cells at two
	// or more of its columns.
	colMinLines = 3
	// colTol is how close, in points, two cells must start to be in one
	// column.
	colTol = 2.0
)

// colSegment is a stretch of a line between column gaps.
type colSegment struct {
	start, end float64
}

// colLine is a line read for columns.
type colLine struct {
	segs    []colSegment
	upright bool
	empty   bool
}

// columnLine splits a line's runs where the gap between them is a column
// gap.
func columnLine(l pdfLine, budget *gridBudget) (colLine, bool) {
	cl := colLine{upright: true, empty: true}
	if !budget.spend(2 * len(l.runs)) {
		return cl, false
	}
	gaps := make([]float64, 0, len(l.runs))
	end := math.Inf(-1)
	for i, r := range l.runs {
		if !r.upright {
			cl.upright = false
		}
		if nonSpace(r.text) > 0 {
			cl.empty = false
		}
		if i > 0 {
			size := math.Max(math.Max(r.size, l.runs[i-1].size), 1)
			if g := r.x - end; g > 0.1*size && g < colGapSizes*size {
				gaps = append(gaps, g)
			}
		}
		end = math.Max(end, math.Max(r.endX, r.x))
	}
	median := 0.0
	if len(gaps) > 0 {
		s := append([]float64(nil), gaps...)
		sort.Float64s(s)
		median = s[len(s)/2]
	}
	end = math.Inf(-1)
	for i, r := range l.runs {
		size := math.Max(r.size, 1)
		if i > 0 {
			size = math.Max(size, l.runs[i-1].size)
		}
		if i == 0 || r.x-end >= math.Max(colGapSizes*size, colGapMedians*median) {
			cl.segs = append(cl.segs, colSegment{start: r.x})
		}
		end = math.Max(end, math.Max(r.endX, r.x))
		cl.segs[len(cl.segs)-1].end = end
	}
	return cl, true
}

// columnOf is the column a stretch starting at x is in: the last whose start
// is at or left of it, -1 when it is left of them all.
func columnOf(cols []float64, x float64) int {
	return sort.Search(len(cols), func(k int) bool { return cols[k] > x+colTol }) - 1
}

// fits reports whether a line belongs to a table of these columns: each of
// its stretches stays within one column, and a line of one stretch (a cell's
// wrapped line) starts where a column does.
func (cl colLine) fits(cols []float64) bool {
	if cl.empty {
		return true
	}
	if !cl.upright {
		return false
	}
	for _, s := range cl.segs {
		k := columnOf(cols, s.start)
		if k < 0 {
			return false
		}
		if k+1 < len(cols) && s.end > cols[k+1]+colTol {
			return false
		}
		if len(cl.segs) == 1 && math.Abs(s.start-cols[k]) > colTol {
			return false
		}
	}
	return true
}

// columnStarts clusters where the lines' cells start and keeps the starts
// two or more lines share.
func columnStarts(lines []colLine) []float64 {
	type start struct {
		x    float64
		line int
	}
	var all []start
	for i, l := range lines {
		if len(l.segs) < 2 || !l.upright || l.empty {
			continue
		}
		for _, s := range l.segs {
			all = append(all, start{s.start, i})
		}
	}
	sort.Slice(all, func(a, b int) bool { return all[a].x < all[b].x })
	var cols []float64
	for i := 0; i < len(all); {
		j := i
		seen := map[int]bool{}
		for j < len(all) && all[j].x-all[i].x <= colTol {
			seen[all[j].line] = true
			j++
		}
		if len(seen) >= 2 {
			cols = append(cols, all[i].x)
		}
		i = j
	}
	return cols
}

// sharing counts the lines that start cells at two or more of the columns.
func sharing(lines []colLine, cols []float64) int {
	n := 0
	for _, l := range lines {
		if l.empty {
			continue
		}
		hit := map[int]bool{}
		for _, s := range l.segs {
			if k := columnOf(cols, s.start); k >= 0 && math.Abs(s.start-cols[k]) <= colTol {
				hit[k] = true
			}
		}
		if len(hit) >= 2 {
			n++
		}
	}
	return n
}

// colRegion is a table found in a block of lines: lines [from, to) and its
// column starts.
type colRegion struct {
	from, to int
	cols     []float64
}

// alignedTables finds the tables in a page's lines. A table does not run on
// past a paragraph break (a gap of more than about two lines) into a line of
// one stretch, which is prose or a heading below it; it does run on into a
// line of cells that fit its columns, as a table with tall rows has such
// gaps between them. ok is false when the budget ran out; what was found
// before then is not returned.
func alignedTables(rows []pdfLine, budget *gridBudget) ([]colRegion, bool) {
	lines := make([]colLine, len(rows))
	for i, r := range rows {
		var ok bool
		if lines[i], ok = columnLine(r, budget); !ok {
			return nil, false
		}
	}
	// segsBefore[i] counts the stretches of lines[:i], so a pass over a
	// window is charged for what it reads.
	segsBefore := make([]int, len(lines)+1)
	for i, l := range lines {
		segsBefore[i+1] = segsBefore[i] + len(l.segs)
	}
	// reach[i] is where the lines a table starting at i may take end: the
	// first paragraph break before a line of one stretch.
	reach := make([]int, len(lines)+1)
	reach[len(lines)] = len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		reach[i] = reach[i+1]
		if i+1 < len(lines) && paragraphBreak(rows, i+1) && len(lines[i+1].segs) < 2 {
			reach[i] = i + 1
		}
	}
	var out []colRegion
	for i := 0; i < len(lines); {
		if len(lines[i].segs) < 2 || !lines[i].upright || lines[i].empty {
			i++
			continue
		}
		// Columns from the lines it may take, then the run of lines that
		// fit them; once more from that run alone, so lines past the table
		// (another table, prose with a wide gap) do not shape its columns.
		to := reach[i]
		var cols []float64
		for range 3 {
			if !budget.spend(4 * (to - i + segsBefore[to] - segsBefore[i])) {
				return nil, false
			}
			cols = columnStarts(lines[i:to])
			if len(cols) < 2 || len(cols) > maxPDFTableCols {
				cols = nil
				break
			}
			end := i
			for end < to && lines[end].fits(cols) {
				end++
			}
			if end == to {
				break
			}
			to = end
		}
		if cols != nil && to-i >= colMinLines && sharing(lines[i:to], cols) >= colMinLines {
			// Empty lines at the end are not the table's.
			for to > i && lines[to-1].empty {
				to--
			}
			out = append(out, colRegion{from: i, to: to, cols: cols})
			i = to
			continue
		}
		i++
	}
	return out, true
}

// read writes a region's lines as table rows, each run in its column.
func (g colRegion) read(rows []pdfLine) ([][]string, int) {
	var out [][]string
	nchars := 0
	for _, l := range rows[g.from:g.to] {
		parts := make([][]pdfRun, len(g.cols))
		for _, r := range l.runs {
			k := max(columnOf(g.cols, r.x), 0)
			parts[k] = append(parts[k], r)
		}
		row := make([]string, len(g.cols))
		empty := true
		for k, p := range parts {
			t := lineText(p)
			if t != "" {
				empty = false
			}
			nchars += nonSpace(t)
			row[k] = cell(t)
		}
		if !empty {
			out = append(out, row)
		}
	}
	return out, nchars
}

// flowBlocks reads text no border put in a table: as text, with the tables
// its aligned columns make in place. budget is the page's: ok is false when
// it ran out, and then the text is read as text.
func flowBlocks(runs []pdfRun, budget *gridBudget) ([]pdfBlock, bool) {
	if len(runs) == 0 {
		return nil, true
	}
	if !budget.spend(len(runs)) {
		return []pdfBlock{textBlock(assemble(runs))}, false
	}
	rows := pdfLines(runs)
	regions, ok := alignedTables(rows, budget)
	if !ok {
		return []pdfBlock{textBlock(assembleLines(rows))}, false
	}
	if len(regions) == 0 {
		return []pdfBlock{textBlock(assembleLines(rows))}, true
	}
	var out []pdfBlock
	text := func(rs []pdfLine) {
		if t := assembleLines(rs); t != "" {
			out = append(out, textBlock(t))
		}
	}
	last := 0
	for _, g := range regions {
		cells, nchars := g.read(rows)
		if len(cells) == 0 {
			continue // no text to put in a table: its lines stay text
		}
		text(rows[last:g.from])
		out = append(out, pdfBlock{table: cells, nchars: nchars, aligned: true})
		last = g.to
	}
	text(rows[last:])
	return out, true
}
