package extract

// The caps every table renderer shares. A table's rendered size is not the
// size of the text the file holds: a cell referenced many times (an XLSX
// shared string), every row padded to the widest one, a merged cell's span,
// each multiply it, so a part of a few kilobytes can render to gigabytes
// before the output limit sees any of it. ODT keeps its own caps
// (maxODTTableBytes, the same byte bound).
const (
	// maxRowsCeiling is the most rows Options.MaxRows may ask for.
	maxRowsCeiling = 10_000
	// maxTableBytes caps the Markdown one table renders to.
	maxTableBytes = 16 << 20
	// maxTableCells caps rows × columns of one table: the default row cap
	// over the widest sheet, so only a raised max_rows meets it.
	maxTableCells = DefaultMaxRows * maxSheetColumns
	// maxCellBytes caps the text one spreadsheet cell shows; Excel's own
	// limit is 32,767 characters.
	maxCellBytes = 32 << 10
)

// tableRows collects a table that a tree format (DOCX, PPTX) builds in
// memory before rendering it with table. A row is cut to maxSheetColumns, and
// the table ends at the first row that would take it past maxTableCells or
// what table would render past maxTableBytes, the padding of every row to
// the widest one counted.
type tableRows struct {
	rows  [][]string
	width int
	text  int // bytes of cell text held
	// wide is set when a row was cut to maxSheetColumns, and full when a row
	// was refused because the table was at a cap.
	wide, full bool
}

// add appends a row of rendered cells; false when the table is full and the
// row was not added.
func (t *tableRows) add(row []string) bool {
	if t.full {
		return false
	}
	if len(row) > maxSheetColumns {
		row, t.wide = row[:maxSheetColumns], true
	}
	width := max(t.width, len(row))
	text := t.text
	for _, c := range row {
		text += len(c)
	}
	n := len(t.rows) + 1
	// table writes "| c |" per cell (an empty one as a space) and a "| --- |"
	// separator: at most six bytes a column a line besides the text.
	if n*width > maxTableCells || text+(n+1)*(2+6*width) > maxTableBytes {
		t.full = true
		return false
	}
	t.rows, t.width, t.text = append(t.rows, row), width, text
	return true
}

// outermost returns every descendant element called local that has no
// ancestor of the same name below n, in document order. A table's cells and
// paragraphs read the text beneath them, so a table nested in a table (or a
// paragraph in a paragraph) is read through its outermost one, once; reading
// each level again would multiply the text by the depth.
func (n *node) outermost(local string) []*node {
	var out []*node
	var walk func(*node, int)
	walk = func(m *node, depth int) {
		if depth > maxXMLDepth {
			return
		}
		for _, k := range m.kids {
			if k.name == local {
				out = append(out, k)
				continue
			}
			walk(k, depth+1)
		}
	}
	if n != nil {
		walk(n, 0)
	}
	return out
}
