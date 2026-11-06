package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// amplifiedXLSX is a workbook whose one sheet has rows × cols cells that all
// reference one shared string of strBytes: a few kilobytes of file that name
// the same string thousands of times.
func amplifiedXLSX(t *testing.T, strBytes, rows, cols int) []byte {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for range rows {
		sb.WriteString("<row>")
		for range cols {
			sb.WriteString(`<c t="s"><v>0</v></c>`)
		}
		sb.WriteString("</row>")
	}
	sb.WriteString(`</sheetData></worksheet>`)
	return storedSheetXLSX(t, sb.String(), `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>`+strings.Repeat("ab ", strBytes/3)+`</t></si></sst>`)
}

// storedSheetXLSX is a workbook of one sheet, sheetXML, and the shared
// strings sstXML. The sheet part is stored, not deflated, so the archive's
// ratio guard has nothing to refuse however repetitive it is.
func storedSheetXLSX(t *testing.T, sheetXML, sstXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string, method uint16) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("_rels/.rels", relsXML([3]string{"rId1", "officeDocument", "xl/workbook.xml"}), zip.Deflate)
	add("xl/workbook.xml", `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" `+nsR+`><sheets><sheet name="S" r:id="rId1"/></sheets></workbook>`, zip.Deflate)
	add("xl/_rels/workbook.xml.rels", relsXML([3]string{"rId1", "worksheet", "worksheets/sheet1.xml"}, [3]string{"rId2", "sharedStrings", "sharedStrings.xml"}), zip.Deflate)
	add("xl/sharedStrings.xml", sstXML, zip.Deflate)
	add("xl/worksheets/sheet1.xml", sheetXML, zip.Store)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExtractXLSX_SharedStringAmplificationIsBounded is the spreadsheet that
// took a shared process down: 2,000 cells naming one 64 KiB shared string,
// about 44 KB of file, allocated 1.9 GiB and ran 21 s before the output limit
// saw any of it. It must now finish in bounded time and memory, the table cut
// at its byte cap.
func TestExtractXLSX_SharedStringAmplificationIsBounded(t *testing.T) {
	data := amplifiedXLSX(t, 64<<10, 10, 200)
	if len(data) > 64<<10 {
		t.Fatalf("fixture is %d bytes; the point is a small file", len(data))
	}
	// The production sink: 32 MiB of Markdown (fileExtractMaxMarkdown).
	s := &memSink{limit: 32 << 20}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	rep, err := XLSX{}.Extract(ctx, bytes.NewReader(data), int64(len(data)), s, Options{Name: "a.xlsx"})
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	// TotalAlloc counts every byte allocated in the window, whatever the GC
	// did with it since: the measure of the work, and not a racy peak. No
	// test in this package runs in parallel, so the window holds this call.
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("elapsed %s, allocated %.1f MiB, wrote %d bytes, err %v", elapsed, float64(alloc)/(1<<20), s.md.Len(), err)

	if elapsed > 5*time.Second {
		t.Errorf("took %s; the bound is 5s", elapsed)
	}
	if alloc > 256<<20 {
		t.Errorf("allocated %d MiB; the bound is 256 MiB", alloc>>20)
	}
	if err != nil {
		t.Errorf("Extract: %v", err)
	}
	if s.md.Len() > maxTableBytes+(1<<20) {
		t.Errorf("wrote %d bytes; one table is capped at %d", s.md.Len(), maxTableBytes)
	}
	if !hasNote(rep, "was cut at") {
		t.Errorf("no note says the table was cut: %+v", rep.Notes)
	}
	if !hasNote(rep, "cells longer than") {
		t.Errorf("no note says long cells were cut: %+v", rep.Notes)
	}
}

// TestExtract_MaxRowsIsClamped: a caller's max_rows is honoured up to the
// ceiling and no further, and a wide sheet stops at the cell ceiling first.
func TestExtract_MaxRowsIsClamped(t *testing.T) {
	t.Run("csv", func(t *testing.T) {
		var b strings.Builder
		for i := range maxRowsCeiling + 50 {
			fmt.Fprintf(&b, "%d,x\n", i)
		}
		s, rep := run(t, CSV{}, []byte(b.String()), Options{Name: "t.csv", MaxRows: 1_000_000})
		// Every table line starts a line: the header, the separator, and the
		// rows after the header.
		if got := strings.Count(s.md.String(), "\n|"); got != maxRowsCeiling+1 {
			t.Errorf("wrote %d table lines, want %d", got, maxRowsCeiling+1)
		}
		if !hasNote(rep, fmt.Sprintf("%d of %d rows shown", maxRowsCeiling, maxRowsCeiling+50)) {
			t.Errorf("notes = %v", rep.Notes)
		}
	})
	t.Run("xlsx cell ceiling", func(t *testing.T) {
		// 200 columns: the cell ceiling allows maxTableCells/200 rows, fewer
		// than the 2,000 asked for.
		rows := maxTableCells/maxSheetColumns + 10
		var sb strings.Builder
		sb.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
		for range rows {
			sb.WriteString("<row>")
			for range maxSheetColumns {
				sb.WriteString(`<c><v>1</v></c>`)
			}
			sb.WriteString("</row>")
		}
		sb.WriteString(`</sheetData></worksheet>`)
		data := storedSheetXLSX(t, sb.String(), `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`)
		s, rep := run(t, XLSX{}, data, Options{Name: "w.xlsx", MaxRows: rows})
		want := maxTableCells / maxSheetColumns
		if got := strings.Count(s.md.String(), "\n| 1 |"); got != want { // the header row included
			t.Errorf("wrote %d rows, want %d", got, want)
		}
		if !hasNote(rep, fmt.Sprintf("%d of %d rows shown", want, rows)) {
			t.Errorf("notes = %v", rep.Notes)
		}
	})
}

// TestExtractDOCX_TablePaddingIsBounded: one row of 1,000 cells each spanning
// 64 columns, then empty rows. Every row is padded to the widest, so a few
// kilobytes of rows rendered to tens of megabytes before the output limit.
func TestExtractDOCX_TablePaddingIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<w:document ` + nsW + `><w:body><w:tbl><w:tr>`)
	for range 1000 {
		b.WriteString(`<w:tc><w:tcPr><w:gridSpan w:val="65"/></w:tcPr><w:p><w:r><w:t>x</w:t></w:r></w:p></w:tc>`)
	}
	b.WriteString(`</w:tr>` + strings.Repeat(`<w:tr/>`, 100) + `</w:tbl></w:body></w:document>`)
	s, rep, err := extractDOCX(t, docxWith(t, b.String()), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := s.md.Len(); n > 1<<20 {
		t.Errorf("wrote %d bytes for a table of one cell of text", n)
	}
	if !hasNote(rep, fmt.Sprintf("wider than %d columns", maxSheetColumns)) {
		t.Errorf("notes = %v", rep.Notes)
	}
}

// TestExtractDOCX_NestedParagraphsCountOnce: a cell's paragraph nested in its
// own paragraph two hundred deep. Each nesting level read the text below it
// again, so 64 KiB of text became 12 MiB.
func TestExtractDOCX_NestedParagraphsCountOnce(t *testing.T) {
	text := strings.Repeat("ab ", (64<<10)/3)
	body := `<w:document ` + nsW + `><w:body><w:tbl><w:tr><w:tc>` +
		strings.Repeat(`<w:p>`, 200) + `<w:r><w:t>` + text + `</w:t></w:r>` + strings.Repeat(`</w:p>`, 200) +
		`</w:tc></w:tr></w:tbl></w:body></w:document>`
	s, _, err := extractDOCX(t, docxWith(t, body), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := s.md.Len(); n > 2*len(text) {
		t.Errorf("wrote %d bytes for %d bytes of text", n, len(text))
	}
}

// TestExtractPPTX_NestedTablesCountOnce: a table nested in its own cell
// forty-five deep (the XML depth cap allows no more). Each was rendered as a table of its own, holding the text of
// every table inside it.
func TestExtractPPTX_NestedTablesCountOnce(t *testing.T) {
	text := strings.Repeat("ab ", (64<<10)/3)
	var tbl strings.Builder
	for range 45 {
		tbl.WriteString(`<a:tbl><a:tr><a:tc><a:txBody><a:p>`)
	}
	tbl.WriteString(`<a:r><a:t>` + text + `</a:t></a:r>`)
	for range 45 {
		tbl.WriteString(`</a:p></a:txBody></a:tc></a:tr></a:tbl>`)
	}
	slide := `<p:sld ` + nsP + ` ` + nsA + ` ` + nsR + `><p:cSld><p:spTree><p:graphicFrame><a:graphic><a:graphicData>` +
		tbl.String() + `</a:graphicData></a:graphic></p:graphicFrame></p:spTree></p:cSld></p:sld>`
	data := zipOf(t,
		xmlEntry("_rels/.rels", relsXML([3]string{"rId1", "officeDocument", "ppt/presentation.xml"})),
		xmlEntry("ppt/presentation.xml", `<p:presentation `+nsP+` `+nsR+`><p:sldIdLst><p:sldId id="256" r:id="rId2"/></p:sldIdLst></p:presentation>`),
		xmlEntry("ppt/_rels/presentation.xml.rels", relsXML([3]string{"rId2", "slide", "slides/slide1.xml"})),
		xmlEntry("ppt/slides/slide1.xml", slide),
	)
	s, rep := run(t, PPTX{}, data, Options{Name: "n.pptx"})
	if n := s.md.Len(); n > 2*len(text) {
		t.Errorf("wrote %d bytes for %d bytes of text", n, len(text))
	}
	if rep.Tables != 1 {
		t.Errorf("tables = %d, want 1", rep.Tables)
	}
}

// TestExtractXLSX_TableIsWrittenARowAtATime: the sink sees a sheet's rows as
// they are read, so its own limit stops a sheet part way rather than after the
// whole table was built.
func TestExtractXLSX_TableIsWrittenARowAtATime(t *testing.T) {
	data := budgetXLSX(t, 500)
	s := &countingSink{}
	if _, err := (XLSX{}).Extract(context.Background(), bytes.NewReader(data), int64(len(data)), s, Options{Name: "b.xlsx"}); err != nil {
		t.Fatal(err)
	}
	if s.calls < 500 {
		t.Errorf("the sink was called %d times for a sheet of 500 rows", s.calls)
	}
}

type countingSink struct{ calls int }

func (s *countingSink) Text(string) error           { s.calls++; return nil }
func (s *countingSink) Image(Image) (string, error) { return "", nil }

// TestExtractXLSX_CancelledMidSheetStops: the context is read at every row,
// not every 4,096 XML tokens.
func TestExtractXLSX_CancelledMidSheetStops(t *testing.T) {
	data := budgetXLSX(t, 500)
	ctx, cancel := context.WithCancel(context.Background())
	s := &cancelSink{cancel: cancel, after: 20}
	_, err := (XLSX{}).Extract(ctx, bytes.NewReader(data), int64(len(data)), s, Options{Name: "b.xlsx"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if s.calls > s.after+2 {
		t.Errorf("the sink was called %d times after the context was cancelled at %d", s.calls, s.after)
	}
}

type cancelSink struct {
	cancel func()
	after  int
	calls  int
}

func (s *cancelSink) Text(string) error {
	s.calls++
	if s.calls == s.after {
		s.cancel()
	}
	return nil
}
func (s *cancelSink) Image(Image) (string, error) { return "", nil }

// TestExtractHTML_TablePaddingIsBounded: one row of 5,000 cells, then 1,000
// empty rows, each padded to the widest: 20 MB of Markdown from 30 KB of page.
func TestExtractHTML_TablePaddingIsBounded(t *testing.T) {
	page := "<html><body><table><tr>" + strings.Repeat("<td>x", 5000) + strings.Repeat("<tr>", 1000) + "</table></body></html>"
	s, rep := run(t, HTML{}, []byte(page), Options{Name: "p.html"})
	if n := s.md.Len(); n > 1<<20 {
		t.Errorf("wrote %d bytes for a table of 5,000 one-letter cells", n)
	}
	if !hasNote(rep, fmt.Sprintf("wider than %d columns", maxSheetColumns)) {
		t.Errorf("notes = %v", rep.Notes)
	}
}

// The per-table caps leave each table free to pad a few kilobytes of rows to
// most of a megabyte; a document of many such tables must still stop at one
// bound for all of them, whatever the sink in front of it allows.

// paddedRow is the cell count of the wide first row of each padded table, and
// paddedRows the empty rows after it: under maxTableCells together, so no
// per-table cap applies, yet ~800 KB rendered from ~5 KB of document.
const paddedCols, paddedRows = 200, 990

func checkDocTableBound(t *testing.T, s *memSink, rep Report) {
	t.Helper()
	if n := s.md.Len(); n > maxDocTableBytes+(1<<20) {
		t.Errorf("wrote %d bytes; all of a document's tables are capped at %d", n, maxDocTableBytes)
	}
	if !hasNote(rep, "for all of the document's tables") {
		t.Errorf("no note says the document's tables were cut: %v", rep.Notes)
	}
}

func TestExtractHTML_ManyTablesAreBoundedTogether(t *testing.T) {
	one := "<table><tr>" + strings.Repeat("<td>x", paddedCols) + strings.Repeat("<tr>", paddedRows) + "</table>"
	page := "<html><body>" + strings.Repeat(one, 60) + "</body></html>"
	s, rep := run(t, HTML{}, []byte(page), Options{Name: "p.html"})
	checkDocTableBound(t, s, rep)
}

func TestExtractPPTX_ManyTablesOnASlideAreBoundedTogether(t *testing.T) {
	one := `<p:graphicFrame><a:graphic><a:graphicData><a:tbl><a:tr>` +
		strings.Repeat(`<a:tc><a:txBody><a:p><a:r><a:t>x</a:t></a:r></a:p></a:txBody></a:tc>`, paddedCols) + `</a:tr>` +
		strings.Repeat(`<a:tr/>`, paddedRows) + `</a:tbl></a:graphicData></a:graphic></p:graphicFrame>`
	slide := `<p:sld ` + nsP + ` ` + nsA + ` ` + nsR + `><p:cSld><p:spTree>` + strings.Repeat(one, 50) + `</p:spTree></p:cSld></p:sld>`
	data := zipOf(t,
		xmlEntry("_rels/.rels", relsXML([3]string{"rId1", "officeDocument", "ppt/presentation.xml"})),
		xmlEntry("ppt/presentation.xml", `<p:presentation `+nsP+` `+nsR+`><p:sldIdLst><p:sldId id="256" r:id="rId2"/></p:sldIdLst></p:presentation>`),
		xmlEntry("ppt/_rels/presentation.xml.rels", relsXML([3]string{"rId2", "slide", "slides/slide1.xml"})),
		xmlEntry("ppt/slides/slide1.xml", slide),
	)
	s, rep := run(t, PPTX{}, data, Options{Name: "t.pptx"})
	checkDocTableBound(t, s, rep)
}

func TestExtractDOCX_ManyTablesAreBoundedTogether(t *testing.T) {
	one := `<w:tbl><w:tr>` + strings.Repeat(`<w:tc><w:p><w:r><w:t>x</w:t></w:r></w:p></w:tc>`, paddedCols) + `</w:tr>` +
		strings.Repeat(`<w:tr/>`, paddedRows) + `</w:tbl>`
	s, rep, err := extractDOCX(t, docxWith(t, `<w:document `+nsW+`><w:body>`+strings.Repeat(one, 60)+`</w:body></w:document>`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	checkDocTableBound(t, s, rep)
}

// Four sheets that all name the same amplified part: each is cut at its own
// table cap, and together they stop at the document's.
func TestExtractXLSX_ManySheetsAreBoundedTogether(t *testing.T) {
	data := amplifiedXLSX(t, 64<<10, 10, 200)
	// Re-point the workbook at the one sheet four times.
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if _, err := b.ReadFrom(rc); err != nil {
			t.Fatal(err)
		}
		_ = rc.Close()
		body := b.String()
		if f.Name == "xl/workbook.xml" {
			body = strings.Replace(body, `<sheet name="S" r:id="rId1"/>`, `<sheet name="A" r:id="rId1"/><sheet name="B" r:id="rId1"/><sheet name="C" r:id="rId1"/><sheet name="D" r:id="rId1"/>`, 1)
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: f.Name, Method: f.Method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	s, rep := run(t, XLSX{}, buf.Bytes(), Options{Name: "m.xlsx"})
	if rep.Sheets != 4 {
		t.Fatalf("sheets = %d, want 4", rep.Sheets)
	}
	checkDocTableBound(t, s, rep)
}

// A text box's paragraphs and the text boxes inside it were each read again
// at every level of nesting, before any table cap applied: 64 KiB of text
// nested 150 deep became 10 MB, and the reviewer's 622 KB file 22 GiB.
func TestExtractDOCX_NestedTextBoxesCountOnce(t *testing.T) {
	text := strings.Repeat("ab ", (64<<10)/3)
	// Paragraphs nested in one text box.
	deepP := `<w:r><w:drawing><w:txbxContent>` + strings.Repeat(`<w:p>`, 150) + `<w:r><w:t>` + text + `</w:t></w:r>` +
		strings.Repeat(`</w:p>`, 150) + `</w:txbxContent></w:drawing></w:r>`
	// Text boxes nested in text boxes.
	deepBox := strings.Repeat(`<w:r><w:drawing><w:txbxContent><w:p>`, 40) + `<w:r><w:t>` + text + `</w:t></w:r>` +
		strings.Repeat(`</w:p></w:txbxContent></w:drawing></w:r>`, 40)
	for _, box := range []struct{ name, xml string }{{"paragraphs", deepP}, {"boxes", deepBox}} {
		for _, where := range []string{"body", "cell"} {
			t.Run(box.name+" in "+where, func(t *testing.T) {
				body := `<w:p>` + box.xml + `</w:p>`
				if where == "cell" {
					body = `<w:tbl><w:tr><w:tc>` + body + `</w:tc></w:tr></w:tbl>`
				}
				data := docxWith(t, `<w:document `+nsW+`><w:body>`+body+`</w:body></w:document>`)
				// In a cell the table cap would drop the multiplied text
				// after it was built, so the work is measured too.
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				s, _, err := extractDOCX(t, data, Options{})
				runtime.ReadMemStats(&after)
				if err != nil {
					t.Fatal(err)
				}
				if n := s.md.Len(); n > 2*len(text) {
					t.Errorf("wrote %d bytes for %d bytes of text", n, len(text))
				}
				if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 32<<20 {
					t.Errorf("allocated %d MiB for %d bytes of text", alloc>>20, len(text))
				}
			})
		}
	}
}

// The title search read every Title-styled paragraph's text, a nested one
// included in each that held it: blank Title paragraphs nested 150 deep
// around 1 MiB of spaces read 150 MiB to find no title.
func TestExtractDOCX_NestedTitleParagraphsAreReadOnce(t *testing.T) {
	title := `<w:pPr><w:pStyle w:val="Title"/></w:pPr>`
	body := strings.Repeat(`<w:p>`+title, 150) + `<w:r><w:t xml:space="preserve">` + strings.Repeat(" ", 1<<20) + `</w:t></w:r>` + strings.Repeat(`</w:p>`, 150)
	styles := `<w:styles ` + nsW + `><w:style w:type="paragraph" w:styleId="Title"><w:name w:val="Title"/></w:style></w:styles>`
	data := docxWith(t, `<w:document `+nsW+`><w:body>`+body+`</w:body></w:document>`,
		xmlEntry("word/_rels/document.xml.rels", relsXML([3]string{"rId1", "styles", "styles.xml"})),
		xmlEntry("word/styles.xml", styles))
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, _, err := extractDOCX(t, data, Options{}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Errorf("allocated %d MiB to read 1 MiB of blank title", alloc>>20)
	}
}

// Speaker notes read every body shape's paragraphs, and a shape nested in a
// paragraph of another was read again by each shape around it.
func TestExtractPPTX_NestedNoteShapesCountOnce(t *testing.T) {
	text := strings.Repeat("ab ", (64<<10)/3)
	sp := `<p:sp><p:nvSpPr><p:cNvPr id="2" name="s"/><p:cNvSpPr/><p:nvPr><p:ph type="body"/></p:nvPr></p:nvSpPr><p:txBody><a:p>`
	notes := `<p:notes ` + nsP + ` ` + nsA + `><p:cSld><p:spTree>` + strings.Repeat(sp, 40) + `<a:r><a:t>` + text + `</a:t></a:r>` +
		strings.Repeat(`</a:p></p:txBody></p:sp>`, 40) + `</p:spTree></p:cSld></p:notes>`
	slide := `<p:sld ` + nsP + ` ` + nsA + ` ` + nsR + `><p:cSld><p:spTree/></p:cSld></p:sld>`
	data := zipOf(t,
		xmlEntry("_rels/.rels", relsXML([3]string{"rId1", "officeDocument", "ppt/presentation.xml"})),
		xmlEntry("ppt/presentation.xml", `<p:presentation `+nsP+` `+nsR+`><p:sldIdLst><p:sldId id="256" r:id="rId2"/></p:sldIdLst></p:presentation>`),
		xmlEntry("ppt/_rels/presentation.xml.rels", relsXML([3]string{"rId2", "slide", "slides/slide1.xml"})),
		xmlEntry("ppt/slides/slide1.xml", slide),
		xmlEntry("ppt/slides/_rels/slide1.xml.rels", relsXML([3]string{"rId8", "notesSlide", "../notesSlides/notesSlide1.xml"})),
		xmlEntry("ppt/notesSlides/notesSlide1.xml", notes),
	)
	s, _ := run(t, PPTX{}, data, Options{Name: "n.pptx"})
	if n := s.md.Len(); n > 3*len(text) { // "> " quoting adds a little
		t.Errorf("wrote %d bytes for %d bytes of notes", n, len(text))
	}
}

// Three million empty shared strings, 15 MB of part, held a 16-byte string
// header each: the table must cost little more per entry than its text.
func TestExtractXLSX_ManySharedStringsAreHeldCompactly(t *testing.T) {
	const n = 3_000_000
	data := []byte(`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` + strings.Repeat(`<si/>`, n) + `</sst>`)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	sst, err := sharedStrings(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	live := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	runtime.KeepAlive(sst)
	runtime.KeepAlive(data)
	t.Logf("%d entries hold %d MiB", n, live>>20)
	if live > 6*n {
		t.Errorf("%d empty shared strings hold %d MiB; want at most 6 bytes each", n, live>>20)
	}
}
