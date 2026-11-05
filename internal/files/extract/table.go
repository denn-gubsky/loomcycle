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
