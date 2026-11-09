package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Heap ceilings on the inputs that made one file cost far more memory than
// its size: an HTML table or element flood, one element with millions of
// attributes, formatting elements the HTML parser clones on every paragraph,
// and the same attribute flood inside a DOCX part. Each is refused, and the
// refusal itself costs little: the count runs before any tree is built.
//
// allocated reports the bytes the Go heap handed out while fn ran. It is the
// cumulative count (TotalAlloc), an upper bound on the peak, so a ceiling on
// it holds the peak too. The inputs are built before it starts.

func allocated(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// peakHeap reports the most live heap seen while fn ran, above what was live
// when it started, sampled every 100µs: for a refusal whose cost is garbage
// the collector takes back, the peak is the bound that matters.
func peakHeap(fn func()) uint64 {
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	read := func() uint64 {
		metrics.Read(sample)
		return sample[0].Value.Uint64()
	}
	base := read()
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(100 * time.Microsecond)
		defer t.Stop()
		for {
			if v := read(); v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
	fn()
	close(stop)
	<-done
	if p := peak.Load(); p > base {
		return p - base
	}
	return 0
}

// raceSlack scales a heap ceiling under the race detector, whose bookkeeping
// and the CI runner's noise move the measured peak by up to about a quarter
// (a 205 MiB local peak read 256 MiB under -race in CI). Every ceiling still
// sits far under the unfixed cost it guards against.
const raceSlack = 2

// ceiling is n, scaled under the race detector.
func ceiling(n uint64) uint64 {
	if raceEnabled {
		return n * raceSlack
	}
	return n
}

func extractBytes(e Extractor, name string, data []byte) (Report, error) {
	return e.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), &memSink{}, Options{Name: name, Images: true})
}

func wantRefusedUnder(t *testing.T, label string, limit uint64, fn func() error) {
	t.Helper()
	var err error
	got := allocated(func() { err = fn() })
	if !IsRefused(err) {
		t.Fatalf("%s: err = %v, want a refusal", label, err)
	}
	if ceiling := ceiling(limit); got > ceiling {
		t.Fatalf("%s: refusing it allocated %d MiB, want at most %d MiB", label, got>>20, ceiling>>20)
	}
	t.Logf("%s: refused (%v) after %d MiB allocated", label, err, got>>20)
}

func TestHTML_OverSizeCapRefused(t *testing.T) {
	// The reviewer's 31 MiB table flood: over the page cap, refused unread.
	page := []byte("<table><tr>" + strings.Repeat("<td>", 31<<20/4))
	wantRefusedUnder(t, "31 MiB table flood", 1<<20, func() error {
		_, err := extractBytes(HTML{}, "flood.html", page)
		return err
	})
	if maxHTMLBytes > 8<<20 {
		t.Fatalf("maxHTMLBytes = %d, want at most 8 MiB", maxHTMLBytes)
	}
}

func TestHTML_TableCellFloodRefusedBeforeParse(t *testing.T) {
	// Under the size cap, one row of two million cells: the table walk keeps
	// only the first 200 columns, so the cells past them were never counted
	// against the element cap while the parser built a node for each.
	page := []byte("<table><tr>" + strings.Repeat("<td>", (8<<20-64)/4))
	wantRefusedUnder(t, "table cell flood", 32<<20, func() error {
		_, err := extractBytes(HTML{}, "cells.html", page)
		return err
	})
}

func TestHTML_AttributeFloodRefusedBeforeParse(t *testing.T) {
	// One element with as many distinct attributes as the page cap allows
	// (repeated names are dropped by the tokenizer, so they cost nothing).
	// The count holds one tag's attributes while it reads them; what it
	// refuses is the parse, which would keep them all as nodes' attributes.
	var b strings.Builder
	b.WriteString("<p ")
	for i := 0; b.Len() < 8<<20-16; i++ {
		b.WriteString("a")
		b.WriteString(strconv.FormatInt(int64(i), 36))
		b.WriteString(" ")
	}
	b.WriteString(">")
	page := []byte(b.String())
	var err error
	peak := peakHeap(func() { _, err = extractBytes(HTML{}, "attrs.html", page) })
	if !IsRefused(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if c := ceiling(256 << 20); peak > c {
		t.Fatalf("refusing it held %d MiB at its peak, want at most %d MiB", peak>>20, c>>20)
	}
	t.Logf("attribute flood: refused (%v) at a peak of %d MiB", err, peak>>20)
}

func TestHTML_ReconstructedFormattingRefusedBeforeParse(t *testing.T) {
	// Formatting elements left open inside a paragraph are cloned by the
	// parser after every paragraph that closes them: 300 distinct <b>s and
	// ten thousand paragraphs are three million nodes from 200 KB of page.
	var b strings.Builder
	b.WriteString("<p>")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "<b class=c%d>", i)
	}
	b.WriteString(strings.Repeat("</p><p>x", 10000))
	page := []byte(b.String())
	wantRefusedUnder(t, "reconstructed formatting", 32<<20, func() error {
		_, err := extractBytes(HTML{}, "clones.html", page)
		return err
	})
}

func TestHTML_PageWithFormattingStillExtracts(t *testing.T) {
	// The formatting count is not a refusal of formatting: closed tags, a few
	// left open, and the same tag repeated (the parser keeps three) pass.
	var b strings.Builder
	b.WriteString("<title>T</title><p><font face=x>")
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "<p><b>bold %d</b> <i>it</i> <font color=red>red</font><a href=/x>l</a></p>", i)
	}
	for i := 0; i < 50; i++ {
		b.WriteString("<p><span><b>left open")
	}
	s, _ := run(t, HTML{}, []byte(b.String()), Options{Name: "ok.html"})
	if !strings.Contains(s.md.String(), "bold 1999") {
		t.Fatalf("the page was not extracted:\n%.300s", s.md.String())
	}
}

// cancelAfterReader serves r and cancels once more than after bytes have
// been read in all, counting what it serves after that: the extraction's
// deadline arrives while the file is being read.
type cancelAfterReader struct {
	r        io.ReaderAt
	after    int64
	cancel   func()
	read     atomic.Int64
	fired    atomic.Bool
	lateRead atomic.Int64
}

func (c *cancelAfterReader) ReadAt(p []byte, off int64) (int, error) {
	if c.fired.Load() {
		c.lateRead.Add(int64(len(p)))
	}
	if c.read.Add(int64(len(p))) > c.after && c.fired.CompareAndSwap(false, true) {
		c.cancel()
	}
	return c.r.ReadAt(p, off)
}

func TestHTML_ParseStopsAtTheDeadline(t *testing.T) {
	// Few elements (the page walk would never reach its context check) and
	// many bytes: whichever pass is reading when the deadline comes, the count
	// before the parse or the parse itself, stops within a read.
	page := []byte("<p>" + strings.Repeat("word ", (6<<20)/5) + "</p>")
	for _, c := range []struct {
		name  string
		after int64
	}{{"while counting", 64 << 10}, {"while parsing", int64(len(page)) + 64<<10}} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &cancelAfterReader{r: bytes.NewReader(page), after: c.after, cancel: cancel}
			start := time.Now()
			_, err := HTML{}.Extract(ctx, r, int64(len(page)), &memSink{}, Options{Name: "slow.html"})
			if !r.fired.Load() {
				t.Fatal("the deadline never came: the page was read only once")
			}
			if late := r.lateRead.Load(); late > 256<<10 {
				t.Fatalf("%d KiB were read after the deadline, want the read to stop within one", late>>10)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the context's error", err)
			}
			t.Logf("stopped %v after the start, %d KiB read past the deadline", time.Since(start), r.lateRead.Load()>>10)
		})
	}
}

// attrFlood is one element carrying n distinct attributes.
func attrFlood(n int) string {
	var b strings.Builder
	b.Grow(n * 10)
	b.WriteString("<w:p ")
	for i := 0; i < n; i++ {
		b.WriteString("a")
		b.WriteString(strconv.FormatInt(int64(i), 36))
		b.WriteString(`="" `)
	}
	b.WriteString("/>")
	return b.String()
}

func TestParseXML_AttributeFloodRefusedBeforeDecode(t *testing.T) {
	part := []byte(`<w:document ` + nsW + `><w:body>` + attrFlood(2_000_000) + `</w:body></w:document>`)
	wantRefusedUnder(t, "one element, 2M attributes", 16<<20, func() error {
		_, err := parseXML(context.Background(), part)
		return err
	})
}

func TestParseXML_AttributeTotalCapped(t *testing.T) {
	// Many elements, each with an ordinary count, past the part's total.
	var b strings.Builder
	b.WriteString(`<w:document ` + nsW + `><w:body>`)
	el := attrFlood(1000)
	for i := 0; i <= maxXMLAttrs/1000; i++ {
		b.WriteString(el)
	}
	b.WriteString(`</w:body></w:document>`)
	part := []byte(b.String())
	wantRefusedUnder(t, "attribute total", 16<<20, func() error {
		_, err := parseXML(context.Background(), part)
		return err
	})
}

func TestParseXML_AttributeValueBytesCapped(t *testing.T) {
	big := strings.Repeat("v", 1<<20)
	var b strings.Builder
	b.WriteString(`<r>`)
	for i := 0; i <= maxXMLAttrBytes>>20; i++ {
		b.WriteString(`<e a="` + big + `"/>`)
	}
	b.WriteString(`</r>`)
	part := []byte(b.String())
	wantRefusedUnder(t, "attribute value bytes", 16<<20, func() error {
		_, err := parseXML(context.Background(), part)
		return err
	})
}

func TestHTML_TextRunsCountAsNodes(t *testing.T) {
	// An element and a text run each: the walk counts both, so the count does.
	page := []byte(strings.Repeat("<i>x</i>", maxXMLNodes/2+1000))
	wantRefusedUnder(t, "element and text flood", 32<<20, func() error {
		_, err := extractBytes(HTML{}, "e.html", page)
		return err
	})
}

func TestScanSheet_AttributeFloodRefused(t *testing.T) {
	sheet := []byte(`<worksheet><sheetData><row r="1">` + strings.Replace(attrFlood(1_000_000), "<w:p ", "<c ", 1) + `</row></sheetData></worksheet>`)
	wantRefusedUnder(t, "worksheet attribute flood", 16<<20, func() error {
		_, _, err := scanSheet(context.Background(), sheet, sharedTable{}, 10, func([]xlsxCell) error { return nil })
		return err
	})
	shared := []byte(`<sst>` + strings.Replace(attrFlood(1_000_000), "<w:p ", "<si ", 1) + `</sst>`)
	wantRefusedUnder(t, "shared strings attribute flood", 16<<20, func() error {
		_, err := sharedStrings(context.Background(), shared)
		return err
	})
}

func TestDOCX_AttributeFloodRefused(t *testing.T) {
	doc := docxWith(t, `<w:document `+nsW+`><w:body>`+attrFlood(2_000_000)+`</w:body></w:document>`)
	// The ceiling is the archive layer reading the 20 MiB part out of the zip
	// (about 64 MiB; ceiling doubles it under -race); the count adds nothing.
	wantRefusedUnder(t, "DOCX attribute flood", 96<<20, func() error {
		_, err := extractBytes(DOCX{}, "flood.docx", doc)
		return err
	})
}

func TestParseXML_QuoteInADoctypeCommentDoesNotHideAFlood(t *testing.T) {
	// A lone quote inside a comment of the internal subset: the decoder skips
	// the comment before it looks at quotes, so the count must too, or it
	// would read the rest of the part as a quoted string and miss the flood.
	part := []byte(`<!DOCTYPE r [<!-- it's -->]><r>` + attrFlood(2_000_000) + `</r>`)
	wantRefusedUnder(t, "flood after a doctype comment", 16<<20, func() error {
		_, err := parseXML(context.Background(), part)
		return err
	})
}

func TestCountXMLAttrs_UnterminatedMarkupFailsClosed(t *testing.T) {
	for _, part := range []string{
		`<r a="1`, `<r a="1"`, `<!-- open`, `<![CDATA[ x`, `<?pi`, `<!DOCTYPE r [ "x" `, `<!DOCTYPE r [<!-- x ]>`, `</r`,
	} {
		if _, _, _, err := countXMLAttrs(context.Background(), []byte(part)); !IsRefused(err) {
			t.Errorf("%q: err = %v, want a refusal", part, err)
		}
	}
}

func TestScanXMLAttrs_CountsOnlyRealAttributes(t *testing.T) {
	// '=' in text, comments, CDATA, processing instructions, a doctype and
	// quoted values is not an attribute; each real one is.
	part := []byte(`<?xml version="1.0"?><!DOCTYPE x [<!ENTITY e "a=b">]><!-- a=b c=d --><r x="1" y='a=b>c'>a=b<![CDATA[ c=d ]]><?pi a=b?><e z="2"/></r>`)
	n, per, vb, err := countXMLAttrs(context.Background(), part)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || per != 2 || vb != len("1")+len("a=b>c")+len("2") {
		t.Fatalf("counted %d attributes, %d on the widest element, %d value bytes; want 3, 2 and 7", n, per, vb)
	}
}

func TestHTML_StrayEndTagsThatCreateElementsCounted(t *testing.T) {
	// In <body>, a stray </p> makes the parser create an empty <p> and a </br>
	// a <br>: end tags that build nodes count as nodes.
	for _, tag := range []string{"</p>", "</br>", "</P>"} {
		page := []byte("<body>" + strings.Repeat(tag, (8<<20-16)/len(tag)))
		wantRefusedUnder(t, tag+" flood", 32<<20, func() error {
			_, err := extractBytes(HTML{}, "stray.html", page)
			return err
		})
	}
}

func TestHTML_PeakAtTheNodeCapIsBounded(t *testing.T) {
	// The documented per-file HTML peak (SECURITY.md, limit.go: about 280
	// MiB): a page just under the node cap, built and walked.
	page := []byte(strings.Repeat("<i>x</i>", maxXMLNodes/2-1000))
	var err error
	peak := peakHeap(func() { _, err = extractBytes(HTML{}, "cap.html", page) })
	if err != nil {
		t.Fatalf("a page under the caps: %v", err)
	}
	if c := ceiling(320 << 20); peak > c {
		t.Fatalf("a page at the node cap held %d MiB at its peak, want at most %d MiB", peak>>20, c>>20)
	}
	t.Logf("peak %d MiB", peak>>20)
}
