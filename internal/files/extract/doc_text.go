package extract

import (
	"encoding/binary"
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// The text of a Word document: its characters decoded piece by piece in CP
// order, the special characters turned into paragraphs, cells, rows, line
// breaks and note references, and each block written as it ends.

// errDOCInferFull ends the heading-inference pass when what it would hold
// passes its bounds; the document is then read without inferred headings.
var errDOCInferFull = errors.New("extract: heading inference bound reached")

// docCompressed is [MS-DOC] §2.4.1's mapping of an 8-bit piece's bytes:
// Latin-1, but for these, which take windows-1252's characters.
var docCompressed = map[byte]rune{
	0x82: 0x201A, 0x83: 0x0192, 0x84: 0x201E, 0x85: 0x2026, 0x86: 0x2020, 0x87: 0x2021,
	0x88: 0x02C6, 0x89: 0x2030, 0x8A: 0x0160, 0x8B: 0x2039, 0x8C: 0x0152,
	0x91: 0x2018, 0x92: 0x2019, 0x93: 0x201C, 0x94: 0x201D, 0x95: 0x2022, 0x96: 0x2013,
	0x97: 0x2014, 0x98: 0x02DC, 0x99: 0x2122, 0x9A: 0x0161, 0x9B: 0x203A, 0x9C: 0x0153,
	0x9F: 0x0178,
}

// chars decodes CPs [a, b) in order, calling fn with each character and the
// byte it is stored at. Each piece is read in chunks, so a piece past a
// cut is never read.
func (d *docReader) chars(a, b int, fn func(cp int, ch rune, fc int64) error) error {
	if a >= b {
		return nil
	}
	i := sort.Search(len(d.pieces), func(i int) bool { return d.pieces[i].cp1 > a })
	buf := make([]byte, 32<<10)
	var high uint16 // a UTF-16 high surrogate waiting for its low half
	for ; i < len(d.pieces) && d.pieces[i].cp0 < b; i++ {
		p := d.pieces[i]
		from, to := max(a, p.cp0), min(b, p.cp1)
		if from >= to {
			continue
		}
		w := 1
		if p.wide {
			w = 2
		}
		for cp := from; cp < to; {
			if err := d.ctx.Err(); err != nil {
				return err
			}
			n := min(to-cp, len(buf)/w)
			fc := p.fc + int64(cp-p.cp0)*int64(w)
			d.decoded += int64(n * w)
			if d.decoded > maxDOCText {
				return refusedf("the document's text is over %d bytes", maxDOCText)
			}
			chunk := buf[:n*w]
			if _, err := d.wd.ReadAt(chunk, fc); err != nil {
				return refusedf("the document's text lies past its stream")
			}
			for k := 0; k < n; k++ {
				var ch rune
				switch {
				case p.wide:
					u := binary.LittleEndian.Uint16(chunk[2*k:])
					switch {
					case utf16.IsSurrogate(rune(u)) && u < 0xDC00:
						high = u
						continue
					case utf16.IsSurrogate(rune(u)):
						if high == 0 {
							continue
						}
						ch = utf16.DecodeRune(rune(high), rune(u))
						high = 0
					default:
						high = 0
						ch = rune(u)
					}
				case p.cm != nil:
					ch = p.cm.DecodeByte(chunk[k])
				default:
					ch = rune(chunk[k])
					if r, ok := docCompressed[chunk[k]]; ok {
						ch = r
					}
				}
				if err := fn(cp+k, ch, fc+int64(k*w)); err != nil {
					return err
				}
			}
			cp += n
		}
	}
	return nil
}

// read writes the main document, then the notes its text referred to.
func (d *docReader) read() error {
	if err := d.chars(0, d.ccpText, d.char); err != nil {
		return err
	}
	return d.finish()
}

// inferHeadings reads the main document once without writing, to decide
// which paragraphs look like headings (Options.InferHeadings): only the
// first maxDOCInferRun characters of a paragraph and its formatting are
// kept. Past its bounds, or on any error, no heading is inferred; an error
// the document has comes back from the writing pass.
func (d *docReader) inferHeadings() {
	d.inferring = true
	err := d.chars(0, d.ccpText, d.char)
	if err == nil && d.para.Len() > 0 {
		err = d.endPara(-1, false)
	}
	d.inferring = false
	paras := d.inferParas
	d.inferParas, d.runs, d.inferText = nil, nil, 0
	d.para.Reset()
	d.paraCut, d.fields, d.instr, d.paraN, d.decoded = false, d.fields[:0], 0, 0, 0
	if errors.Is(err, errDOCInferFull) {
		d.rep.note("headings were not inferred: the document has over %d paragraphs or %d bytes of text to weigh", maxDOCInferParas, maxDOCInferText)
		return
	}
	if err != nil {
		return
	}
	res := inferLevels(paras)
	d.inferred = res.levels
	d.rep.InferredHeadings = res.headings
	res.note(d.rep)
}

// char takes one character of the main document.
func (d *docReader) char(cp int, ch rune, fc int64) error {
	switch ch {
	case 0x13: // a field begins: its instruction until 0x14
		if len(d.fields) == maxDOCFieldDepth {
			d.fieldOver++
			if !d.inferring && d.fieldOver == 1 {
				d.deepFields++
			}
			return nil
		}
		d.fields = append(d.fields, true)
		d.instr++
		return nil
	case 0x14: // the field's result follows
		if d.fieldOver == 0 && len(d.fields) > 0 && d.fields[len(d.fields)-1] {
			d.fields[len(d.fields)-1] = false
			d.instr--
		}
		return nil
	case 0x15: // the field ends
		switch {
		case d.fieldOver > 0:
			d.fieldOver--
		case len(d.fields) > 0:
			if d.fields[len(d.fields)-1] {
				d.instr--
			}
			d.fields = d.fields[:len(d.fields)-1]
		}
		return nil
	}
	if d.instr > 0 {
		return nil
	}
	if ref, ok := d.refs[cp]; ok {
		return d.noteRef(ref, fc)
	}
	switch {
	case ch == 0x0D, ch == 0x0C:
		return d.endPara(fc, false)
	case ch == 0x07:
		return d.endPara(fc, true)
	case ch == 0x0B:
		return d.add("\n", fc)
	case ch == 0x09:
		return d.add(" ", fc)
	case ch == 0x01, ch == 0x08:
		if !d.inferring {
			d.pictures++
		}
		return nil
	case ch == 0x1E: // a non-breaking hyphen
		return d.add("-", fc)
	case ch < 0x20 || (ch >= 0x7F && ch < 0xA0) || ch == 0xFFFD:
		return nil
	}
	if !d.inferring && !d.paraCut && d.para.Len()+utf8.RuneLen(ch) <= maxDOCPara {
		d.para.WriteRune(ch) // the common case, without a string per character
		return nil
	}
	return d.add(string(ch), fc)
}

// noteRef writes a reference to a note as [n], numbering the notes in the
// order they are referred to.
func (d *docReader) noteRef(ref docNoteRef, fc int64) error {
	if d.inferring {
		return d.add("[n]", fc)
	}
	d.notes = append(d.notes, ref)
	return d.add("["+strconv.Itoa(len(d.notes))+"]", fc)
}

// add appends text to the paragraph. A body paragraph that reaches
// maxDOCPara is written in pieces; a heading's or a cell's text is cut.
func (d *docReader) add(s string, fc int64) error {
	if d.paraCut {
		return nil
	}
	if d.inferring {
		return d.addInfer(s, fc)
	}
	if d.para.Len()+len(s) > maxDOCPara {
		p := d.papAt(fc)
		if p.inTable || d.headingLevel(p) > 0 {
			d.paraCut = true
			return nil
		}
		text := d.para.String()
		d.para.Reset()
		if err := d.emit(d.paragraphMD(text)); err != nil {
			return err
		}
	}
	d.para.WriteString(s)
	return nil
}

// addInfer keeps a paragraph's first characters and their formatting for
// the inference pass.
func (d *docReader) addInfer(s string, fc int64) error {
	if d.para.Len() >= maxDOCInferRun {
		return nil
	}
	f := d.fmtAt(fc)
	if len(d.runs) == 0 || d.runs[len(d.runs)-1].fmt != f {
		d.runs = append(d.runs, fmtRun{fmt: f})
	}
	d.runs[len(d.runs)-1].text += s
	d.para.WriteString(s)
	d.inferText += len(s)
	if d.inferText > maxDOCInferText {
		return errDOCInferFull
	}
	return nil
}

// fmtAt is the bold and size of the character at fc: its paragraph style's,
// changed by its own character properties.
func (d *docReader) fmtAt(fc int64) runFmt {
	istd := d.papAt(fc).istd
	if d.styleCache == nil {
		d.styleCache = map[int]runFmt{}
	}
	sf, ok := d.styleCache[istd]
	if !ok {
		bold, hps := d.styleFmt(istd)
		sf = runFmt{bold: bold, size: float64(hps) / 2}
		if len(d.styleCache) < maxDOCStyles {
			d.styleCache[istd] = sf
		}
	}
	f := sf
	docSprms(d.propsAt(&d.chpBTE, &d.chp, fc, false), func(sprm uint16, op []byte) {
		switch sprm {
		case sprmCFBold:
			f.bold = docToggle(op[0], sf.bold)
		case sprmCHps:
			f.size = float64(binary.LittleEndian.Uint16(op)) / 2
		}
	})
	return f
}

// endPara ends the paragraph whose mark is at fc: a cell's paragraph, a
// cell (cellMark), a row (a cell mark with the row's properties), a heading
// or a body paragraph. fc -1 is a last paragraph with no mark.
func (d *docReader) endPara(fc int64, cellMark bool) error {
	p := docPara{outline: -1}
	if fc >= 0 {
		p = d.papAt(fc)
	}
	text := d.para.String()
	d.para.Reset()
	d.paraCut = false
	idx := d.paraN
	d.paraN++
	if d.inferring {
		runs := d.runs
		d.runs = nil
		if d.paraN > maxDOCInferParas {
			return errDOCInferFull
		}
		d.inferParas = append(d.inferParas, inferPara{runs: runs, eligible: !p.inTable && !cellMark, real: d.headingLevel(p)})
		return nil
	}
	if d.paraN > maxDOCParas {
		return refusedf("the document has over %d paragraphs", maxDOCParas)
	}
	switch {
	case cellMark && p.ttp:
		return d.endRow()
	case cellMark && p.inTable:
		d.cellPara(text)
		d.endCell()
		return nil
	case p.inTable:
		d.cellPara(text)
		return nil
	}
	lvl := d.headingLevel(p)
	if lvl == 0 && idx < len(d.inferred) {
		lvl = d.inferred[idx]
	}
	if lvl > 0 {
		return d.emitHeading(lvl, text)
	}
	return d.emit(d.paragraphMD(text))
}

// paragraphMD renders a body paragraph: each line escaped, a line break a
// hard break, empty lines dropped.
func (d *docReader) paragraphMD(text string) string {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimRight(escapeLine(l), " \t"); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\\\n")
}

// emitHeading writes a heading one level below the title. A Heading 1
// that is the document's first block becomes the title when the
// properties name none.
func (d *docReader) emitHeading(lvl int, text string) error {
	t := oneLine(text)
	if len(t) > maxDOCLine {
		t, _ = cutRunesPlain(t, maxDOCLine/4)
		d.cutHead++
	}
	if t == "" {
		return nil
	}
	if !d.started && lvl == 1 && metaString(d.meta.Title, MaxMetaString) == "" {
		d.title = t
		return d.begin()
	}
	return d.emit(heading(lvl+1, t))
}

// cellPara keeps the paragraph just ended for the cell, under the cell's
// cap.
func (d *docReader) cellPara(raw string) {
	if room := maxCellBytes - d.cellBytes; len(raw) > room {
		room = max(room, 0)
		for room > 0 && !utf8.RuneStart(raw[room]) {
			room--
		}
		raw = raw[:room]
		d.w.tableCut = true
	}
	text := cell(raw)
	if text == "" {
		return
	}
	d.cellBytes += len(text)
	d.cell = append(d.cell, text)
}

// endCell closes the cell: its paragraphs joined, added to the row.
func (d *docReader) endCell() {
	if len(d.row) < maxSheetColumns {
		d.row = append(d.row, cellParagraphs(d.cell))
	} else {
		d.w.tableWide = true
	}
	d.cell, d.cellBytes = d.cell[:0], 0
}

// endRow writes the row as a line of the table, the first row as its
// header, under the table caps: a row past them is read and not written.
func (d *docReader) endRow() error {
	if len(d.cell) > 0 {
		d.endCell()
	}
	row := d.row
	d.row = d.row[:0]
	if len(row) == 0 {
		return nil
	}
	t := d.tbl
	first := t == nil
	if first {
		t = &rtfTable{width: len(row)}
	} else if t.full {
		return nil
	}
	line := rtfRowLine(row, t.width)
	if first {
		line += "\n|" + strings.Repeat(" --- |", t.width)
	}
	room, docBound := d.w.tableRoom()
	if (t.rows+1)*max(t.width, len(row)) > maxTableCells || t.size+len(line)+1 > room {
		t.full = true
		d.w.tableCut = true
		d.w.docTablesCut = d.w.docTablesCut || docBound
		return nil
	}
	if first {
		if err := d.begin(); err != nil {
			return err
		}
		d.tbl = t
		d.rep.Tables++
		line = "\n\n" + line
	} else {
		line = "\n" + line
	}
	t.rows++
	t.size += len(line)
	return d.w.text(line)
}

// endTable charges a table that ended to the document's table budget.
func (d *docReader) endTable() {
	if d.tbl == nil {
		return
	}
	d.w.tableBytes += d.tbl.size
	d.tbl = nil
}

// begin writes the title, once.
func (d *docReader) begin() error {
	if d.started {
		return nil
	}
	d.started = true
	if t := metaString(d.meta.Title, MaxMetaString); t != "" {
		d.title = t
	}
	return d.w.text(heading(1, d.title))
}

// emit writes one block after the title; any paragraph outside a table
// ends the table before it.
func (d *docReader) emit(md string) error {
	d.endTable()
	if md == "" {
		return nil
	}
	if err := d.begin(); err != nil {
		return err
	}
	return d.w.text("\n\n" + md)
}

// finish writes what is open, then the notes the text referred to.
func (d *docReader) finish() error {
	if d.para.Len() > 0 {
		if err := d.endPara(-1, false); err != nil {
			return err
		}
	}
	if len(d.cell) > 0 || len(d.row) > 0 {
		if err := d.endRow(); err != nil {
			return err
		}
	}
	if err := d.begin(); err != nil {
		return err
	}
	d.endTable()
	if len(d.notes) == 0 {
		return nil
	}
	if err := d.emit(heading(2, "Notes")); err != nil {
		return err
	}
	kept := 0
	for k, ref := range d.notes {
		text, err := d.noteText(ref)
		if err != nil {
			return err
		}
		if text == "" {
			continue
		}
		if kept+len(text) > maxDOCNoteBytes {
			d.droppedNotes++
			continue
		}
		kept += len(text)
		if err := d.emit(escapeLine("[" + strconv.Itoa(k+1) + "] " + text)); err != nil {
			return err
		}
	}
	return nil
}

// noteText decodes one note's text from its sub-document: on one line,
// field instructions dropped, at most maxDOCNote bytes.
func (d *docReader) noteText(ref docNoteRef) (string, error) {
	base, end, txt := d.ccpText, d.ccpText+d.ccpFtn, d.ftnTxt
	if ref.edn {
		base = d.ccpText + d.ccpFtn + d.ccpHdd + d.ccpAtn
		end, txt = base+d.ccpEdn, d.ednTxt
	}
	if ref.i+1 >= len(txt) {
		return "", nil
	}
	a, b := base+int(txt[ref.i]), base+int(txt[ref.i+1])
	if a < base || b > end || a >= b {
		return "", nil
	}
	var sb strings.Builder
	instr, depth := 0, []bool{}
	err := d.chars(a, b, func(_ int, ch rune, _ int64) error {
		switch ch {
		case 0x13:
			if len(depth) < maxDOCFieldDepth {
				depth = append(depth, true)
				instr++
			}
			return nil
		case 0x14:
			if len(depth) > 0 && depth[len(depth)-1] {
				depth[len(depth)-1] = false
				instr--
			}
			return nil
		case 0x15:
			if len(depth) > 0 {
				if depth[len(depth)-1] {
					instr--
				}
				depth = depth[:len(depth)-1]
			}
			return nil
		}
		if instr > 0 || sb.Len() >= maxDOCNote {
			return nil
		}
		switch {
		case ch == 0x0D || ch == 0x0B || ch == 0x07 || ch == 0x09 || ch == 0x0C:
			sb.WriteByte(' ')
		case ch == 0x1E:
			sb.WriteByte('-')
		case ch < 0x20 || (ch >= 0x7F && ch < 0xA0) || ch == 0xFFFD:
		default:
			sb.WriteRune(ch)
		}
		return nil
	})
	return oneLine(sb.String()), err
}
