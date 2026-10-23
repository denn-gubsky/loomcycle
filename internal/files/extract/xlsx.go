package extract

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
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
		rows, total, wide, err := sheetRows(ctx, data, sst, maxRows)
		if err != nil {
			rep.finish(w)
			return rep, err
		}
		out := "\n\n" + heading(2, name) + "\n\n"
		if len(rows) == 0 {
			out += "_(no cells)_"
		} else {
			rep.Tables++
			out += table(rows)
			if total > len(rows) {
				out += fmt.Sprintf("\n\n[+%d rows omitted]", total-len(rows))
				rep.note("sheet %q: %d of %d rows shown", name, len(rows), total)
			}
		}
		if wide {
			rep.note("sheet %q: columns past the %dth are not shown", name, maxSheetColumns)
		}
		if err := w.text(out); err != nil {
			rep.finish(w)
			return rep, err
		}
	}
	rep.finish(w)
	return rep, nil
}

// sharedStrings reads xl/sharedStrings.xml: each <si>'s text, its rich-text
// runs joined, its phonetic hints (<rPh>) left out.
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
				cur.Write(t)
			}
		}
	}
}

// sheetRows streams a worksheet: the first maxRows non-empty rows as cell
// strings, the count of all non-empty rows, and whether any cell lay past
// maxSheetColumns.
func sheetRows(ctx context.Context, data []byte, sst []string, maxRows int) ([][]string, int, bool, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	var rows [][]string
	total := 0
	wide := false
	var row []string
	rowHasValue := false
	var typ, val, inline string
	col := 0
	inV, inIs, inT, inPh := false, false, false, 0
	for n := 0; ; n++ {
		if n%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, false, err
			}
		}
		tok, err := d.Token()
		if err == io.EOF {
			return rows, total, wide, nil
		}
		if err != nil {
			return nil, 0, false, fmt.Errorf("extract: malformed worksheet: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row, rowHasValue, col = row[:0], false, 0
			case "c":
				typ, val, inline = "", "", ""
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
				v := cellValue(typ, val, inline, sst)
				if v != "" {
					rowHasValue = true
				}
				if col >= maxSheetColumns {
					if v != "" {
						wide = true
					}
				} else if total < maxRows {
					for len(row) <= col {
						row = append(row, "")
					}
					row[col] = cell(v)
				}
				col++
			case "row":
				if !rowHasValue {
					continue
				}
				total++
				if total <= maxRows {
					end := len(row)
					for end > 0 && row[end-1] == "" {
						end--
					}
					rows = append(rows, append([]string(nil), row[:end]...))
				}
			}
		case xml.CharData:
			switch {
			case inV:
				val += string(t)
			case inT:
				inline += string(t)
			}
		}
	}
}

// cellValue is a cell's cached value as text: a shared string, an inline
// string, a boolean, or the number, date or error exactly as stored.
func cellValue(typ, val, inline string, sst []string) string {
	switch typ {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil || i < 0 || i >= len(sst) {
			return ""
		}
		return sst[i]
	case "inlineStr":
		return inline
	case "b":
		switch strings.TrimSpace(val) {
		case "1":
			return "TRUE"
		case "0":
			return "FALSE"
		}
		return val
	}
	return val
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
