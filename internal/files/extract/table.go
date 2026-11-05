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
	// maxDocTableBytes caps the Markdown all of one document's tables render
	// to together: the most any caller keeps of an extraction (the File
	// tool's output limit). The per-table caps alone would let a document of
	// many tables each just under them grow without bound.
	maxDocTableBytes = 32 << 20
	// maxTableCells caps rows × columns of one table: the default row cap
	// over the widest sheet, so only a raised max_rows meets it.
	maxTableCells = DefaultMaxRows * maxSheetColumns
	// maxCellBytes caps the text one spreadsheet cell shows; Excel's own
	// limit is 32,767 characters.
	maxCellBytes = 32 << 10
)

// tableRows collects a table that a tree format (DOCX, PPTX, HTML) builds in
// memory before rendering it with table. A row is cut to maxSheetColumns, and
// the table ends at the first row that would take it past maxTableCells or
// what table would render past maxTableBytes, or past what is left of the
// document's maxDocTableBytes, the padding of every row to the widest one
// counted. Make one with newTable and end it with done.
type tableRows struct {
	w     *writer
	rows  [][]string
	width int
	text  int // bytes of cell text held
	size  int // what table will render the rows to, at most
	// wide is set when a row was cut to maxSheetColumns, and full when a row
	// was refused because the table was at a cap.
	wide, full bool
}

// newTable starts a table charged to the writer's document-wide budget.
func (w *writer) newTable() *tableRows { return &tableRows{w: w} }

// done charges the table to the document's budget and records its cuts for
// the report.
func (t *tableRows) done() {
	t.w.tableBytes += t.size
	t.w.tableCut = t.w.tableCut || t.full
	t.w.tableWide = t.w.tableWide || t.wide
}

// tableRoom is what a table may render to now: its own cap, or what is left
// of the document's, whichever is less. docBound reports that the document's
// is the one that binds.
func (w *writer) tableRoom() (room int, docBound bool) {
	left := maxDocTableBytes - w.tableBytes
	if left < maxTableBytes {
		return max(left, 0), true
	}
	return maxTableBytes, false
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
	size := text + (n+1)*(2+6*width)
	room, docBound := t.w.tableRoom()
	if n*width > maxTableCells || size > room {
		t.full = true
		t.w.docTablesCut = t.w.docTablesCut || (docBound && n*width <= maxTableCells)
		return false
	}
	t.rows, t.width, t.text, t.size = append(t.rows, row), width, text, size
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
