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
