package extract

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// XLSX reads a workbook: each sheet, in workbook order, becomes a section
// with one GFM table of its cells' cached values (what Excel last computed,
// not the formulas), the first row as the table's header.
type XLSX struct{}

const xlsxType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

// maxSheetColumns caps the columns a sheet's table shows: a sheet with one
// cell in column XFD would otherwise render sixteen thousand empty cells.
const maxSheetColumns = 200

func (XLSX) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{xlsxType, "application/vnd.ms-excel.sheet.macroenabled.12"}, []string{".xlsx", ".xlsm"})
}

func (XLSX) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "xlsx"}
	a, err := openZip(ctx, r, size, opts.limits)
	if err != nil {
		return rep, err
	}
	pkg, err := a.rels("")
	if err != nil {
		return rep, err
	}
	main := "xl/workbook.xml"
	for _, rl := range pkg {
		if relType(rl.Type, "officeDocument") && !rl.External {
			main = rl.Target
		}
	}
	wb, err := a.readXML(main)
	if err != nil {
		return rep, err
	}
	rels, err := a.rels(main)
	if err != nil {
		return rep, err
	}
	var sst []string
	for _, rl := range rels {
		if relType(rl.Type, "sharedStrings") && !rl.External {
			b, err := a.read(rl.Target)
			if err != nil {
				return rep, err
			}
			if sst, err = sharedStrings(ctx, b); err != nil {
				return rep, err
			}
		}
	}
	w := newWriter(ctx, sink, opts)
	docTitle, _ := packageProps(a, pkg)
	if docTitle == "" {
		docTitle = title(opts.Name, "Workbook")
	}
	if err := w.text(heading(1, docTitle)); err != nil {
		return rep, err
	}
	maxRows := opts.maxRows()
	for _, sh := range wb.path("workbook", "sheets").all("sheet") {
		name := sh.attr("name")
		if name == "" {
			name = fmt.Sprintf("Sheet %d", rep.Sheets+1)
		}
		rep.Sheets++
		rl, ok := rels[sh.attrRel("id")]
		if !ok || rl.External || !a.has(rl.Target) {
			// A chart sheet or a dangling reference: named, not read.
			if err := w.text("\n\n" + heading(2, name) + "\n\n_(no cells)_"); err != nil {
				return rep, err
			}
			continue
		}
		data, err := a.read(rl.Target)
		if err != nil {
			rep.finish(w)
			return rep, err
		}
		if err := writeSheet(ctx, w, &rep, name, data, sst, maxRows); err != nil {
			rep.finish(w)
			return rep, err
		}
	}
	rep.finish(w)
	return rep, nil
}

// errSheetFull stops a sheet's scan once its table has reached a cap.
var errSheetFull = errors.New("extract: sheet table full")

// writeSheet writes one sheet's section: its heading, then its table a row
// at a time, so no more than one rendered row is held and the sink's own
// limit applies as the rows come. GFM sizes a table by its header row, so the
// sheet is read twice: once to learn how wide the rows it will show are, and
// once to write them. A cell's text is cut at maxCellBytes, and the table
// stops at maxTableCells and maxTableBytes: a shared string is stored once
// but may be named by every cell, so the sheet's own size says nothing about
// what its table renders to.
func writeSheet(ctx context.Context, w *writer, rep *Report, name string, data []byte, sst []string, maxRows int) error {
	// widths[k] is the width the first k+1 rows need.
	var widths []int
	total, wide, err := scanSheet(ctx, data, sst, maxRows, func(row []xlsxCell) error {
		wd := len(row)
		if n := len(widths); n > 0 {
			wd = max(wd, widths[n-1])
		}
		widths = append(widths, wd)
		return nil
	})
	if err != nil {
		return err
	}
	if err := w.text("\n\n" + heading(2, name) + "\n\n"); err != nil {
		return err
	}
	if len(widths) == 0 {
		if wide {
			rep.note("sheet %q: columns past the %dth are not shown", name, maxSheetColumns)
		}
		return w.text("_(no cells)_")
	}
	show := len(widths)
	for show > 1 && show*widths[show-1] > maxTableCells {
		show--
	}
	width := widths[show-1]
	// What this sheet's table may render to: its own cap, or what the
	// document's other tables have left.
	room, docBound := w.tableRoom()

	// A shared string long enough to matter is rendered once, however many
	// cells name it.
	rendered := map[int]string{}
	long := false
	render := func(c xlsxCell) string {
		if c.shared >= 0 {
			if r, ok := rendered[c.shared]; ok {
				return r
			}
		}
		v, cut := cutCell(c.text)
		long = long || cut
		r := cell(v)
		if c.shared >= 0 && len(c.text) > 1<<10 {
			rendered[c.shared] = r
		}
		return r
	}
	var line strings.Builder
	written, size, full := 0, 0, false
	_, _, err = scanSheet(ctx, data, sst, show, func(row []xlsxCell) error {
		line.Reset()
		if written > 0 {
			line.WriteString("\n")
		}
		line.WriteString("|")
		for i := range width {
			c := ""
			if i < len(row) {
				c = render(row[i])
			}
			if c == "" {
				c = " "
			}
			line.WriteString(" ")
			line.WriteString(c)
			line.WriteString(" |")
		}
		if written == 0 {
			line.WriteString("\n|")
			for range width {
				line.WriteString(" --- |")
			}
		}
		// The header row is always written when the table has its own cap
		// to fill: maxSheetColumns cells of at most maxCellBytes, escaped,
		// stay under maxTableBytes.
		if size+line.Len() > room && (written > 0 || docBound) {
			full = true
			return errSheetFull
		}
		if written == 0 {
			rep.Tables++
		}
		size += line.Len()
		written++
		return w.text(line.String())
	})
	w.tableBytes += size
	if err != nil && !errors.Is(err, errSheetFull) {
		return err
	}
	if total > written {
		rep.note("sheet %q: %d of %d rows shown", name, written, total)
		if err := w.text(fmt.Sprintf("\n\n[+%d rows omitted]", total-written)); err != nil {
			return err
		}
	}
	if show < len(widths) {
		rep.note("sheet %q: rows past %d cells are not shown", name, maxTableCells)
	}
	switch {
	case full && docBound:
		w.docTablesCut = true
	case full:
		rep.note("sheet %q: the table was cut at %d bytes of text", name, maxTableBytes)
	}
	if long {
		rep.note("sheet %q: cells longer than %d bytes were cut", name, maxCellBytes)
	}
	if wide {
		rep.note("sheet %q: columns past the %dth are not shown", name, maxSheetColumns)
	}
	return nil
}

// cutCell cuts a cell's text to maxCellBytes, on a character boundary.
func cutCell(s string) (string, bool) {
	if len(s) <= maxCellBytes {
		return s, false
	}
	i := maxCellBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "…", true
}

// appendCapped appends b to dst while dst holds no more than maxCellBytes:
// what is past that is cut when the cell is rendered, so it is not kept, and
// a value split into many pieces (comments between them) costs no more than
// one.
func appendCapped(dst, b []byte) []byte {
	if room := maxCellBytes + 1 - len(dst); room > 0 {
		dst = append(dst, b[:min(len(b), room)]...)
	}
	return dst
}

// sharedStrings reads xl/sharedStrings.xml: each <si>'s text, its rich-text
// runs joined, its phonetic hints (<rPh>) left out, kept to just past
// maxCellBytes (no cell shows more).
func sharedStrings(ctx context.Context, data []byte) ([]string, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	var out []string
	var cur strings.Builder
	inSI, inT, inPh := false, false, 0
	for n := 0; ; n++ {
		if n%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		tok, err := d.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("extract: malformed shared strings: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "rPh":
				inPh++
			case "t":
				inT = inSI && inPh == 0
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				inSI = false
				out = append(out, cur.String())
			case "rPh":
				inPh--
			case "t":
				inT = false
			}
		case xml.CharData:
			if inT {
				if room := maxCellBytes + 1 - cur.Len(); room > 0 {
					cur.Write(t[:min(len(t), room)])
				}
			}
		}
	}
}

// xlsxCell is one cell's value as text, not yet rendered; shared is the
// shared string it names, or -1.
type xlsxCell struct {
	text   string
	shared int
}

// scanSheet streams a worksheet: each of the first maxRows non-empty rows is
// passed to fn as its cells' values up to maxSheetColumns, trailing empty
// cells dropped (fn must not keep the slice). It returns the count of all
// non-empty rows and whether any cell lay past maxSheetColumns. An error
// from fn stops the scan and is returned.
func scanSheet(ctx context.Context, data []byte, sst []string, maxRows int, fn func(row []xlsxCell) error) (int, bool, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	total := 0
	wide := false
	var row []xlsxCell
	rowHasValue := false
	var typ string
	var val, inline []byte
	col := 0
	inV, inIs, inT, inPh := false, false, false, 0
	for n := 0; ; n++ {
		if n%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, false, err
			}
		}
		tok, err := d.Token()
		if err == io.EOF {
			return total, wide, nil
		}
		if err != nil {
			return 0, false, fmt.Errorf("extract: malformed worksheet: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row, rowHasValue, col = row[:0], false, 0
			case "c":
				typ, val, inline = "", val[:0], inline[:0]
				for _, at := range t.Attr {
					switch at.Name.Local {
					case "t":
						typ = at.Value
					case "r":
						if c, ok := colIndex(at.Value); ok {
							col = c
						}
					}
				}
			case "v":
				inV = true
			case "is":
				inIs = true
			case "rPh":
				inPh++
			case "t":
				inT = inIs && inPh == 0
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inV = false
			case "is":
				inIs = false
			case "rPh":
				inPh--
			case "t":
				inT = false
			case "c":
				c := cellValue(typ, string(val), string(inline), sst)
				if c.text != "" {
					rowHasValue = true
				}
				if col >= maxSheetColumns {
					if c.text != "" {
						wide = true
					}
				} else if total < maxRows {
					for len(row) <= col {
						row = append(row, xlsxCell{shared: -1})
					}
					row[col] = c
				}
				col++
			case "row":
				if err := ctx.Err(); err != nil {
					return 0, false, err
				}
				if !rowHasValue {
					continue
				}
				total++
				if total <= maxRows {
					// A cell that renders empty (whitespace only) does not
					// widen the row.
					end := len(row)
					for end > 0 && strings.TrimSpace(row[end-1].text) == "" {
						end--
					}
					if err := fn(row[:end]); err != nil {
						return total, wide, err
					}
				}
			}
		case xml.CharData:
			switch {
			case inV:
				val = appendCapped(val, t)
			case inT:
				inline = appendCapped(inline, t)
			}
		}
	}
}

// cellValue is a cell's cached value as text: a shared string, an inline
// string, a boolean, or the number, date or error exactly as stored.
func cellValue(typ, val, inline string, sst []string) xlsxCell {
	switch typ {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil || i < 0 || i >= len(sst) {
			return xlsxCell{shared: -1}
		}
		return xlsxCell{text: sst[i], shared: i}
	case "inlineStr":
		return xlsxCell{text: inline, shared: -1}
	case "b":
		switch strings.TrimSpace(val) {
		case "1":
			return xlsxCell{text: "TRUE", shared: -1}
		case "0":
			return xlsxCell{text: "FALSE", shared: -1}
		}
	}
	return xlsxCell{text: val, shared: -1}
}

// colIndex reads the column of a cell reference ("B7" → 1).
func colIndex(ref string) (int, bool) {
	n := 0
	i := 0
	for ; i < len(ref); i++ {
		c := ref[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c < 'A' || c > 'Z' {
			break
		}
		n = n*26 + int(c-'A'+1)
		if n > 1<<20 {
			return 0, false
		}
	}
	if i == 0 {
		return 0, false
	}
	return n - 1, true
}
